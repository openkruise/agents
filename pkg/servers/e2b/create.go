/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package e2b

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/pausedretention"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/csiutils"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// noServerTimeout is used when the server should not impose its own deadline on
// the claim/clone/wait-ready phases of CreateSandbox. The operation is then
// bounded only by the client request context (cancellation). It is a far-future
// duration rather than a true infinity so that existing timeout handling
// (context deadlines, retry step counts, wait-ready polling) keeps working
// unchanged. ~100 years is indistinguishable from unlimited for any real
// request and stays well within time.Duration's int64 range (max ~292 years).
const noServerTimeout = 100 * 365 * 24 * time.Hour

// createTimeoutGuard bounds the pre-ready crash window for timed creates.
const createTimeoutGuard = 10 * time.Minute

// mapInfraErrorToApiError converts an infra-layer error to an ApiError with the
// appropriate HTTP status code based on managererrors.ErrorCode.
func mapInfraErrorToApiError(err error) *web.ApiError {
	// The create API maps validation/lookups to 400, quota misses to 403, and
	// everything else to 500.
	switch managererrors.GetErrCode(err) {
	case managererrors.ErrorBadRequest, managererrors.ErrorNotFound:
		return &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	case managererrors.ErrorConflict:
		return &web.ApiError{Code: http.StatusConflict, Message: err.Error()}
	case managererrors.ErrorQuotaExceeded:
		return &web.ApiError{Code: http.StatusForbidden, Message: err.Error()}
	default:
		// ErrorInternal, ErrorUnknown, or untyped errors (e.g., retry exhausted) → 500
		return &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
}

func validateCreateResourceOverride(request models.NewSandboxRequest) *web.ApiError {
	res := request.Extensions.InplaceUpdate.Resources
	if res == nil {
		return nil
	}
	if _, ok := res.Requests[corev1.ResourceMemory]; ok {
		return &web.ApiError{Code: http.StatusBadRequest, Message: "memory inplace update is not supported"}
	}
	if _, ok := res.Limits[corev1.ResourceMemory]; ok {
		return &web.ApiError{Code: http.StatusBadRequest, Message: "memory inplace update is not supported"}
	}
	return nil
}

// resolveServerTimeout maps an extension-provided seconds value to a server-side
// timeout. A positive value yields a finite timeout; an absent (zero) or
// non-positive value yields noServerTimeout.
func resolveServerTimeout(seconds int) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	return noServerTimeout
}

// CreateSandbox allocates a Pod as a new sandbox
func (sc *Controller) CreateSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}
	request, parseErr := sc.parseCreateSandboxRequest(r)
	if parseErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, parseErr
	}
	if validateErr := validateCreateResourceOverride(request); validateErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, validateErr
	}
	namespace := sc.getNamespaceOfUser(user)
	log.Info("create sandbox request received", "request", request)
	if sc.manager.GetInfra().HasTemplate(ctx, infra.HasTemplateOptions{
		Namespace: namespace,
		Name:      request.TemplateID,
	}) {
		log.Info("infra has template, will create sandbox with claim", "templateID", request.TemplateID)
		return sc.createSandboxWithClaim(ctx, request, user)
	} else if sc.manager.GetInfra().HasCheckpoint(ctx, infra.HasCheckpointOptions{
		Namespace:    namespace,
		CheckpointID: request.TemplateID,
	}) {
		log.Info("infra has checkpoint, will create sandbox with clone", "templateID", request.TemplateID)
		return sc.createSandboxWithClone(ctx, request, user)
	}
	return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
		Code:    http.StatusBadRequest,
		Message: "Template or Checkpoint not found",
	}
}

func (sc *Controller) createSandboxWithClaim(ctx context.Context, request models.NewSandboxRequest, user *models.CreatedTeamAPIKey) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	if request.Extensions.Name != "" || request.Extensions.GenerateName != "" {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "sandbox-name and sandbox-generate-name are only supported for clone",
		}
	}

	log := klog.FromContext(ctx)
	claimStart := time.Now()
	var accessToken string
	saveTimeoutOptions := buildCreateSaveTimeoutOptions(request)
	infraOpts := infra.ClaimSandboxOptions{
		Namespace:    sc.getNamespaceOfUser(user),
		Template:     request.TemplateID,
		User:         user.ID.String(),
		ClaimTimeout: resolveServerTimeout(request.Extensions.TimeoutSeconds),
		Modifier: func(sbx infra.Sandbox) {
			sc.basicSandboxCreateModifier(ctx, sbx, request)
			// record the basic csi mount persistent volume config to sandbox
			sc.csiMountOptionsConfigRecord(ctx, sbx, request)
		},
		ReserveFailedSandboxFor: request.Extensions.ReserveFailedSandboxFor,
		CreateOnNoStock:         request.Extensions.CreateOnNoStock,
		UserMetadataKeys:        sandboxcr.BuildUserMetadataKeys(request.Extensions.Labels, request.Metadata),
		SaveTimeoutOptions:      &saveTimeoutOptions,
	}

	if !request.Extensions.SkipInitRuntime {
		accessToken = config.NewDefaultAccessToken()
		infraOpts.InitRuntime = &config.InitRuntimeOptions{
			EnvVars:     request.EnvVars,
			AccessToken: accessToken,
		}
	}

	if extension := request.Extensions.InplaceUpdate; extension.Image != "" || extension.Resources != nil {
		infraOpts.InplaceUpdate = &config.InplaceUpdateOptions{
			Image: extension.Image,
		}
		if extension.Resources != nil && (len(extension.Resources.Requests) > 0 || len(extension.Resources.Limits) > 0) {
			infraOpts.InplaceUpdate.Resources = &config.InplaceUpdateResourcesOptions{
				Requests: extension.Resources.Requests,
				Limits:   extension.Resources.Limits,
			}
		}
	}

	infraOpts.WaitReadyTimeout = resolveServerTimeout(request.Extensions.WaitReadySeconds)

	if len(request.Extensions.CSIMount.MountConfigs) != 0 {
		csiMountOptions := make([]config.MountConfig, 0, len(request.Extensions.CSIMount.MountConfigs))
		csiClient := csiutils.NewCSIMountHandler(sc.cache.GetClient(), sc.cache.GetAPIReader(), sc.storageRegistry, utils.DefaultSandboxDeployNamespace)
		for _, mountConfig := range request.Extensions.CSIMount.MountConfigs {
			driverName, csiReqConfigRaw, err := csiClient.CSIMountOptionsConfig(ctx, mountConfig)
			if err != nil {
				return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: err.Error(),
				}
			}
			csiMountOptions = append(csiMountOptions, config.MountConfig{
				Driver:     driverName,
				RequestRaw: csiReqConfigRaw,
			})
		}
		infraOpts.CSIMount = &config.CSIMountOptions{
			MountOptionList: csiMountOptions,
		}
	}

	opts := sandboxmanager.ClaimSandboxOptions{
		Infra: infraOpts,
		Quota: user.QuotaSpec.DeepCopy(),
	}

	sbx, err := sc.manager.ClaimSandbox(ctx, opts)
	if err != nil {
		log.Error(err, "sandbox creation failed")
		return web.ApiResponse[*models.Sandbox]{}, mapInfraErrorToApiError(err)
	}
	log.Info("sandbox created", "id", sbx.GetSandboxID(), "sbx", klog.KObj(sbx),
		"resourceVersion", sbx.GetResourceVersion(), "totalCost", time.Since(claimStart))
	return web.ApiResponse[*models.Sandbox]{
		Code: http.StatusCreated,
		Body: sc.convertToE2BSandbox(sbx, accessToken),
	}, nil
}

func (sc *Controller) createSandboxWithClone(ctx context.Context, request models.NewSandboxRequest, user *models.CreatedTeamAPIKey) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	log := klog.FromContext(ctx)
	start := time.Now()

	if request.Extensions.InplaceUpdate.Image != "" {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "InplaceUpdate is not supported for clone",
		}
	}

	saveTimeoutOptions := buildCreateSaveTimeoutOptions(request)
	infraOpts := infra.CloneSandboxOptions{
		Namespace:    sc.getNamespaceOfUser(user),
		User:         user.ID.String(),
		CheckPointID: request.TemplateID,
		CloneTimeout: resolveServerTimeout(request.Extensions.TimeoutSeconds),
		Modifier: func(sbx infra.Sandbox) {
			sc.basicSandboxCreateModifier(ctx, sbx, request)
		},
		ReserveFailedSandboxFor: request.Extensions.ReserveFailedSandboxFor,
		Name:                    request.Extensions.Name,
		GenerateName:            request.Extensions.GenerateName,
		SaveTimeoutOptions:      &saveTimeoutOptions,
	}
	infraOpts.WaitReadyTimeout = resolveServerTimeout(request.Extensions.WaitReadySeconds)

	if len(request.Extensions.CSIMount.MountConfigs) != 0 {
		csiMountOptions := make([]config.MountConfig, 0, len(request.Extensions.CSIMount.MountConfigs))
		csiClient := csiutils.NewCSIMountHandler(sc.cache.GetClient(), sc.cache.GetAPIReader(), sc.storageRegistry, utils.DefaultSandboxDeployNamespace)
		for _, mountConfigRequest := range request.Extensions.CSIMount.MountConfigs {
			driverName, csiReqConfigRaw, err := csiClient.CSIMountOptionsConfig(ctx, mountConfigRequest)
			if err != nil {
				return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: err.Error(),
				}
			}
			csiMountOptions = append(csiMountOptions, config.MountConfig{
				Driver:     driverName,
				RequestRaw: csiReqConfigRaw,
			})
			infraOpts.CSIMount = &config.CSIMountOptions{
				MountOptionList: csiMountOptions,
			}
		}
	}

	opts := sandboxmanager.CloneSandboxOptions{
		Infra: infraOpts,
		Quota: user.QuotaSpec.DeepCopy(),
	}

	sbx, err := sc.manager.CloneSandbox(ctx, opts)
	if err != nil {
		log.Error(err, "sandbox clone failed")
		return web.ApiResponse[*models.Sandbox]{}, mapInfraErrorToApiError(err)
	}
	log.Info("sandbox cloned", "id", sbx.GetSandboxID(), "sbx", klog.KObj(sbx),
		"resourceVersion", sbx.GetResourceVersion(), "totalCost", time.Since(start))
	return web.ApiResponse[*models.Sandbox]{
		Code: http.StatusCreated,
		Body: sc.convertToE2BSandbox(sbx, utils.GetAccessToken(sbx)),
	}, nil
}

func (sc *Controller) parseCreateSandboxRequest(r *http.Request) (models.NewSandboxRequest, *web.ApiError) {
	var request models.NewSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, &web.ApiError{
			Message: err.Error(),
		}
	}

	// Validate and convert volumeMounts to CSI mount configs
	if len(request.VolumeMounts) > 0 {
		if err := models.ValidateVolumeMounts(request.VolumeMounts); err != nil {
			return request, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: err.Error(),
			}
		}
		for _, vm := range request.VolumeMounts {
			request.Extensions.CSIMount.MountConfigs = append(request.Extensions.CSIMount.MountConfigs, agentsv1alpha1.CSIMountConfig{
				PvName:    vm.Name,
				MountPath: vm.Path,
			})
		}
	}

	if err := request.ParseExtensions(); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Bad extension param: %s", err.Error()),
		}
	}

	for k := range request.Metadata {
		if errLists := validation.IsQualifiedName(k); len(errLists) > 0 {
			return request, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Unqualified metadata key [%s]: %s", k, strings.Join(errLists, ", ")),
			}
		}

		if !ValidateMetadataKey(k) {
			return request, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Forbidden metadata key [%s]: cannot contain prefixes: %v", k, BlackListPrefix),
			}
		}
	}

	if request.Timeout == 0 {
		request.Timeout = models.DefaultTimeoutSeconds
	}

	if request.Timeout < models.DefaultMinTimeoutSeconds || request.Timeout > sc.maxTimeout {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("timeout should between %d and %d", models.DefaultMinTimeoutSeconds, sc.maxTimeout),
		}
	}

	return request, nil
}

func (sc *Controller) basicSandboxCreateModifier(ctx context.Context, sbx infra.Sandbox, request models.NewSandboxRequest) {
	log := klog.FromContext(ctx)
	// E2B-managed sandboxes persist paused-retention preference so later timeout
	// writes and controller auto-pause use the same policy. Claim/clone
	// post-process saves the final create timeout after wait-ready, so wait-ready
	// time is not counted against the sandbox lifetime. Timed creates still get a
	// short shutdown guard so leaked not-ready objects can be collected.
	if request.Extensions.NeverTimeout {
		sbx.SetTimeout(timeout.Options{})
		log.V(utils.DebugLogLevel).Info("timeout options cleared before create")
	} else {
		// Keep a bounded shutdown backstop until the final timeout is saved after readiness.
		sbx.SetTimeout(timeout.Options{ShutdownTime: time.Now().Add(createTimeoutGuard)})
		log.V(utils.DebugLogLevel).Info("timeout guard set before create")
	}

	// propagate annotations to sandbox
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration] = request.Extensions.ReservePausedSandboxDuration
	for k, v := range request.Metadata {
		annotations[k] = v
	}
	if request.Extensions.ReturnPodIP {
		annotations[models.ExtensionKeyReturnPodIP] = agentsv1alpha1.True
	}
	sbx.SetAnnotations(annotations)

	// propagate labels to sandbox metadata (does not affect pod template hash)
	labels := sbx.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	for k, v := range request.Extensions.Labels {
		labels[k] = v
	}
	sbx.SetLabels(labels)

	// Propagate request labels to the pod template metadata. This ensures the
	// sandbox hash includes the labels, and the controller patches the pod metadata
	// directly for metadata-only changes (no image/resources) without setting the
	// InplaceUpdate condition.
	infra.MergePodLabels(sbx, request.Extensions.Labels)
}

func buildCreateSaveTimeoutOptions(request models.NewSandboxRequest) infra.SaveTimeoutOptions {
	if !request.Extensions.NeverTimeout {
		return infra.SaveTimeoutOptions{
			TimeoutGetter: func(infra.TimeoutSnapshot) timeout.Options {
				retention, _ := pausedretention.ParseReservePausedSandboxDuration(request.Extensions.ReservePausedSandboxDuration)
				return computeTimeoutOptions(request.AutoPause, time.Now(), request.Timeout, retention)
			}}
	}
	return infra.SaveTimeoutOptions{Timeout: timeout.Options{}}
}

func (sc *Controller) csiMountOptionsConfigRecord(ctx context.Context, sbx infra.Sandbox, request models.NewSandboxRequest) {
	log := klog.FromContext(ctx)
	// fetch the csi mount config from request
	if len(request.Extensions.CSIMount.MountConfigs) == 0 {
		return
	}
	// marshal the csi mount confit to json
	csiMountConfigRaw, err := json.Marshal(request.Extensions.CSIMount.MountConfigs)
	if err != nil {
		log.Error(err, "failed to marshal csi mount config")
		return
	}
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	// record the csi mount config to annotation
	annotations[models.ExtensionKeyClaimWithCSIMount_MountConfig] = string(csiMountConfigRaw)
	sbx.SetAnnotations(annotations)
}
