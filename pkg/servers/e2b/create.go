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
	domain, apiErr := sc.resolveSandboxDomain(r)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}
	namespace := sc.getNamespaceOfUser(user)
	log.Info("create sandbox request received", "request", request)
	if sc.manager.GetInfra().HasTemplate(ctx, infra.HasTemplateOptions{
		Namespace: namespace,
		Name:      request.TemplateID,
	}) {
		log.Info("infra has template, will create sandbox with claim", "templateID", request.TemplateID)
		return sc.createSandboxWithClaim(ctx, request, user, domain)
	} else if sc.manager.GetInfra().HasCheckpoint(ctx, infra.HasCheckpointOptions{
		Namespace:    namespace,
		CheckpointID: request.TemplateID,
	}) {
		log.Info("infra has checkpoint, will create sandbox with clone", "templateID", request.TemplateID)
		return sc.createSandboxWithClone(ctx, request, user, domain)
	}
	return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
		Code:    http.StatusBadRequest,
		Message: "Template or Checkpoint not found",
	}
}

func (sc *Controller) createSandboxWithClaim(ctx context.Context, request models.NewSandboxRequest, user *models.CreatedTeamAPIKey, domain string) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	if request.Extensions.Name != "" || request.Extensions.GenerateName != "" {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "sandbox-name and sandbox-generate-name are only supported for clone",
		}
	}
	log := klog.FromContext(ctx)
	claimStart := time.Now()
	var accessToken string

	// storageAuthAnnotation holds the annotation key-value pair built by the
	// BuildStorageAuthAnnotation hook (populated later, captured by reference).
	var storageAuthKey, storageAuthValue string

	infraOpts := infra.ClaimSandboxOptions{
		Namespace:    sc.getNamespaceOfUser(user),
		Template:     request.TemplateID,
		User:         user.ID.String(),
		ClaimTimeout: resolveServerTimeout(request.Extensions.TimeoutSeconds),
		Modifier: func(sbx infra.Sandbox) {
			sc.basicSandboxCreateModifier(ctx, sbx, request)
			sc.csiMountOptionsConfigRecord(ctx, sbx, request)
			sc.injectStorageAuthAnnotation(sbx, storageAuthKey, storageAuthValue)
		},
		ReserveFailedSandboxFor: request.Extensions.ReserveFailedSandboxFor,
		CreateOnNoStock:         request.Extensions.CreateOnNoStock,
		UserMetadataKeys:        sandboxcr.BuildUserMetadataKeys(request.Extensions.Labels, request.Metadata),
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

	csiMount, err := sc.buildCSIMountOptions(ctx, request)
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}
	infraOpts.CSIMount = csiMount

	if csiMount != nil && csiutils.BuildStorageAuthAnnotation != nil {
		var authErr error
		storageAuthKey, storageAuthValue, authErr = csiutils.BuildStorageAuthAnnotation(ctx, sc.cache.GetClient(), request.Extensions.CSIMount.MountConfigs)
		if authErr != nil {
			log.Error(authErr, "failed to build storage auth annotation")
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: authErr.Error(),
			}
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

	// Create network CRs (TrafficPolicy) if network config is provided.
	// Network policy creation failure must fail the sandbox creation and killed the sandbox.
	if apiErr := createNetworkPolicyForSandbox(ctx, sbx, request, log); apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}

	return web.ApiResponse[*models.Sandbox]{
		Code: http.StatusCreated,
		Body: sc.convertToE2BSandbox(sbx, accessToken, domain),
	}, nil
}

func (sc *Controller) createSandboxWithClone(ctx context.Context, request models.NewSandboxRequest, user *models.CreatedTeamAPIKey, domain string) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	log := klog.FromContext(ctx)
	start := time.Now()

	if request.Extensions.InplaceUpdate.Image != "" {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "InplaceUpdate is not supported for clone",
		}
	}

	// storageAuthKey/Value holds the annotation key-value pair built by the
	// BuildStorageAuthAnnotation hook (populated later, captured by reference).
	var storageAuthKey, storageAuthValue string

	infraOpts := infra.CloneSandboxOptions{
		Namespace:    sc.getNamespaceOfUser(user),
		User:         user.ID.String(),
		CheckPointID: request.TemplateID,
		CloneTimeout: resolveServerTimeout(request.Extensions.TimeoutSeconds),
		Modifier: func(sbx infra.Sandbox) {
			sc.basicSandboxCreateModifier(ctx, sbx, request)
			sc.injectStorageAuthAnnotation(sbx, storageAuthKey, storageAuthValue)
		},
		ReserveFailedSandboxFor: request.Extensions.ReserveFailedSandboxFor,
		Name:                    request.Extensions.Name,
		GenerateName:            request.Extensions.GenerateName,
	}
	infraOpts.WaitReadyTimeout = resolveServerTimeout(request.Extensions.WaitReadySeconds)

	csiMount, err := sc.buildCSIMountOptions(ctx, request)
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}

	// Persist the user-provided CSI mount config as raw JSON. During clone this
	// lets the request-supplied csi mount config take precedence over the
	// csi-volume-config restored from the checkpoint (see
	// prepareSandboxFromCheckpoint), keeping the persisted annotation consistent
	// with the mount actually performed so later re-mounts reuse the same config.
	if csiMount != nil {
		csiMountConfigRaw, err := json.Marshal(request.Extensions.CSIMount.MountConfigs)
		if err != nil {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: fmt.Sprintf("failed to marshal csi mount config: %s", err.Error()),
			}
		}
		csiMount.MountOptionListRaw = string(csiMountConfigRaw)
	}
	infraOpts.CSIMount = csiMount

	if csiMount != nil && csiutils.BuildStorageAuthAnnotation != nil {
		var authErr error
		storageAuthKey, storageAuthValue, authErr = csiutils.BuildStorageAuthAnnotation(ctx, sc.cache.GetClient(), request.Extensions.CSIMount.MountConfigs)
		if authErr != nil {
			log.Error(authErr, "failed to build storage auth annotation")
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: authErr.Error(),
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

	// Create network CRs (TrafficPolicy) if network config is provided.
	// Network policy creation failure must fail the sandbox creation and killed the sandbox.
	if apiErr := createNetworkPolicyForSandbox(ctx, sbx, request, log); apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}

	return web.ApiResponse[*models.Sandbox]{
		Code: http.StatusCreated,
		Body: sc.convertToE2BSandbox(sbx, utils.GetAccessToken(sbx), domain),
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

	// Validate and build network config.
	networkConfig, err := validateAndBuildNetworkConfig(request.Network)
	if err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}
	request.Network = networkConfig

	return request, nil
}

func (sc *Controller) basicSandboxCreateModifier(ctx context.Context, sbx infra.Sandbox, request models.NewSandboxRequest) {
	log := klog.FromContext(ctx)
	// E2B-managed sandboxes persist paused-retention preference so later timeout
	// writes and controller auto-pause use the same policy. never-timeout keeps
	// deadline fields empty; the annotation is only policy state.
	now := time.Now()
	timeoutOptions := timeout.Options{}
	if !request.Extensions.NeverTimeout {
		retention, _ := pausedretention.ParseReservePausedSandboxDuration(request.Extensions.ReservePausedSandboxDuration)
		timeoutOptions = computeTimeoutOptions(request.AutoPause, now, request.Timeout, retention)
	}
	sbx.SetTimeout(timeoutOptions)
	log.Info("timeout options calculated", "options", timeoutOptions)

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

	// Inject allow-internet-access label. Default is "true"; when network config
	// is provided and allowInternetAccess is explicitly false, set to "false".
	// GlobalTrafficPolicy selects pods by this label to enforce egress rules.
	allowInternetAccess := agentsv1alpha1.True
	if request.AllowInternetAccess != nil && !*request.AllowInternetAccess {
		allowInternetAccess = agentsv1alpha1.False
	}
	labels[agentsv1alpha1.LabelAllowInternetAccess] = allowInternetAccess
	sbx.SetLabels(labels)

	podLabels := make(map[string]string)
	for k, v := range request.Extensions.Labels {
		podLabels[k] = v
	}
	// Inject the sandbox-name label onto the pod
	podLabels[agentsv1alpha1.LabelSandboxName] = sbx.GetName()
	// Propagate allow-internet-access label to the pod as well.
	podLabels[agentsv1alpha1.LabelAllowInternetAccess] = allowInternetAccess

	// Propagate request labels to the pod template metadata. This ensures the
	// sandbox hash includes the labels, and the controller patches the pod metadata
	// directly for metadata-only changes (no image/resources) without setting the
	// InplaceUpdate condition.
	infra.MergePodLabels(sbx, podLabels)
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
		log.Info("failed to marshal csi mount config", err)
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

// buildCSIMountOptions builds CSI mount options from the request's CSI mount
// configurations. Returns nil if no mounts are configured.
func (sc *Controller) buildCSIMountOptions(ctx context.Context, request models.NewSandboxRequest) (*config.CSIMountOptions, error) {
	if len(request.Extensions.CSIMount.MountConfigs) == 0 {
		return nil, nil
	}

	csiMountOptions := make([]config.MountConfig, 0, len(request.Extensions.CSIMount.MountConfigs))
	csiClient := csiutils.NewCSIMountHandler(sc.cache.GetClient(), sc.cache.GetAPIReader(), sc.storageRegistry, utils.DefaultSandboxDeployNamespace)
	for _, mountConfig := range request.Extensions.CSIMount.MountConfigs {
		driverName, csiReqConfigRaw, err := csiClient.CSIMountOptionsConfig(ctx, mountConfig)
		if err != nil {
			return nil, err
		}
		csiMountOptions = append(csiMountOptions, config.MountConfig{
			Driver:     driverName,
			RequestRaw: csiReqConfigRaw,
		})
	}

	return &config.CSIMountOptions{
		MountOptionList: csiMountOptions,
	}, nil
}

// injectStorageAuthAnnotation injects the storage-auth annotation into the sandbox
// when enterprise agent-identity credential metadata is available.
func (sc *Controller) injectStorageAuthAnnotation(sbx infra.Sandbox, key, value string) {
	if key == "" || value == "" {
		return
	}
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	annotations[key] = value
	sbx.SetAnnotations(annotations)
}

// createNetworkPolicyForSandbox creates the TrafficPolicy CR for the sandbox
// based on the request network config. On failure, the sandbox is killed to
// prevent it from running without network policies, and an ApiError is returned.
// Returns nil if no network config is provided or creation succeeds.
func createNetworkPolicyForSandbox(ctx context.Context, sbx infra.Sandbox, request models.NewSandboxRequest, log klog.Logger) *web.ApiError {
	if request.Network == nil {
		return nil
	}
	if netErr := sbx.CreateNetworkPolicy(ctx, infra.SandboxNetworkConfig{
		AllowOut: request.Network.AllowOut,
		DenyOut:  request.Network.DenyOut,
	}); netErr != nil {
		log.Error(netErr, "failed to create network policy, sandbox creation failed",
			"sandboxID", sbx.GetSandboxID())
		killed := killSandboxAfterFailure(ctx, sbx, log)
		return &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("failed to create network policy: %v; clean up sandbox: %v", netErr, killed),
		}
	}
	return nil
}

// killSandboxAfterFailure attempts to delete a sandbox when a post-creation step
// (e.g., network policy creation) fails, preventing orphaned sandboxes from
// running without the intended security configuration. A fresh context with a
// timeout is used when the original request context is already canceled.
func killSandboxAfterFailure(ctx context.Context, sbx infra.Sandbox, log klog.Logger) bool {
	cleanupCtx := ctx
	if ctx.Err() != nil {
		var cancel context.CancelFunc
		cleanupCtx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
	}
	if killErr := sbx.Kill(cleanupCtx); killErr != nil {
		log.Error(killErr, "failed to kill sandbox after post-creation failure",
			"sandboxID", sbx.GetSandboxID())
		return false
	}
	log.Info("sandbox killed after post-creation failure", "sandboxID", sbx.GetSandboxID())
	return true
}
