package e2b

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/csiutils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

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
	log.Info("create sandbox request received", "request", request)
	if sc.manager.GetInfra().HasTemplate(request.TemplateID) {
		log.Info("infra has template, will create sandbox with claim", "templateID", request.TemplateID)
		return sc.createSandboxWithClaim(ctx, request, user)
	} else if sc.manager.GetInfra().HasCheckpoint(request.TemplateID) {
		log.Info("infra has checkpoint, will create sandbox with clone", "templateID", request.TemplateID)
		return sc.createSandboxWithClone(ctx, request, user)
	}
	return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
		Code:    http.StatusBadRequest,
		Message: "Template or Checkpoint not found",
	}
}

func (sc *Controller) createSandboxWithClaim(ctx context.Context, request models.NewSandboxRequest, user *models.CreatedTeamAPIKey) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	log := klog.FromContext(ctx)
	claimStart := time.Now()
	var accessToken string
	if request.Secure {
		accessToken = uuid.NewString()
	}
	opts := infra.ClaimSandboxOptions{
		Template:     request.TemplateID,
		User:         user.ID.String(),
		ClaimTimeout: time.Duration(request.Extensions.TimeoutSeconds) * time.Second,
		Modifier: func(sbx infra.Sandbox) {
			sc.basicSandboxCreateModifier(ctx, sbx, request)
			// record the basic csi mount persistent volume config to sandbox
			sc.csiMountOptionsConfigRecord(ctx, sbx, request)
		},
		ReserveFailedSandbox: request.Extensions.ReserveFailedSandbox,
		CreateOnNoStock:      request.Extensions.CreateOnNoStock,
	}

	if !request.Extensions.SkipInitRuntime {
		opts.InitRuntime = &config.InitRuntimeOptions{
			EnvVars:     request.EnvVars,
			AccessToken: accessToken,
		}
	}

	if extension := request.Extensions.InplaceUpdate; extension.Image != "" || extension.Resources != nil {
		opts.InplaceUpdate = &config.InplaceUpdateOptions{
			Image: extension.Image,
		}
		if extension.Resources != nil && (len(extension.Resources.Requests) > 0 || len(extension.Resources.Limits) > 0) {
			if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxInPlaceResourceResizeGate) {
				return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("in-place resource resize is disabled by feature gate %s", features.SandboxInPlaceResourceResizeGate),
				}
			}
			opts.InplaceUpdate.Resources = &config.InplaceUpdateResourcesOptions{
				Requests: extension.Resources.Requests,
				Limits:   extension.Resources.Limits,
			}
		}
	}

	if request.Extensions.WaitReadySeconds > 0 {
		opts.WaitReadyTimeout = time.Duration(request.Extensions.WaitReadySeconds) * time.Second
	}

	if len(request.Extensions.CSIMount.MountConfigs) != 0 {
		csiMountOptions := make([]config.MountConfig, 0, len(request.Extensions.CSIMount.MountConfigs))
		csiClient := csiutils.NewCSIMountHandler(sc.client, sc.cache, sc.storageRegistry, utils.DefaultSandboxDeployNamespace)
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
		opts.CSIMount = &config.CSIMountOptions{
			MountOptionList: csiMountOptions,
		}
	}

	sbx, err := sc.manager.ClaimSandbox(ctx, opts)
	if err != nil {
		log.Error(err, "sandbox creation failed")
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: err.Error(),
		}
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

	opts := infra.CloneSandboxOptions{
		User:         user.ID.String(),
		CheckPointID: request.TemplateID,
		CloneTimeout: time.Duration(request.Extensions.TimeoutSeconds) * time.Second,
		Modifier: func(sbx infra.Sandbox) {
			sc.basicSandboxCreateModifier(ctx, sbx, request)
		},
	}
	if request.Extensions.WaitReadySeconds > 0 {
		opts.WaitReadyTimeout = time.Duration(request.Extensions.WaitReadySeconds) * time.Second
	}

	if len(request.Extensions.CSIMount.MountConfigs) != 0 {
		csiMountOptions := make([]config.MountConfig, 0, len(request.Extensions.CSIMount.MountConfigs))
		csiClient := csiutils.NewCSIMountHandler(sc.client, sc.cache, sc.storageRegistry, utils.DefaultSandboxDeployNamespace)
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
			opts.CSIMount = &config.CSIMountOptions{
				MountOptionList: csiMountOptions,
			}
		}
	}

	sbx, err := sc.manager.CloneSandbox(ctx, opts)
	if err != nil {
		log.Error(err, "sandbox clone failed")
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: err.Error(),
		}
	}
	log.Info("sandbox cloned", "id", sbx.GetSandboxID(), "sbx", klog.KObj(sbx),
		"resourceVersion", sbx.GetResourceVersion(), "totalCost", time.Since(start))
	return web.ApiResponse[*models.Sandbox]{
		Code: http.StatusCreated,
		Body: sc.convertToE2BSandbox(sbx, utilruntime.GetAccessToken(sbx)),
	}, nil
}

func (sc *Controller) parseCreateSandboxRequest(r *http.Request) (models.NewSandboxRequest, *web.ApiError) {
	var request models.NewSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, &web.ApiError{
			Message: err.Error(),
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
	// The E2B Timeout feature involves three sets of interfaces: create, connect, and pause,
	// with two behavioral modes based on the `autoPause` parameter during creation:
	//
	// - `autoPause = false` (default): Automatically delete Sandbox when timeout
	// - `autoPause = true`: Pause Sandbox when timeout
	//
	// The Timeout feature is implemented through two parameters in the `Sandbox` Infra:
	//
	// - During creation (create interface), set the corresponding parameter to `time.Now().Add(timeout)`
	// - During connection (connect, timeout interfaces), set the corresponding parameter to `time.Now().Add(timeout)` as well
	// - During pause (pause interface):
	//   - if autoPause == true: Set `ShutdownTime` to `time.Now().Add(maxTimeout)` and clear `PauseTime`
	//   - if autoPause == false: Set `ShutdownTime` to `time.Now().Add(maxTimeout)`
	now := time.Now()
	timeoutOptions := infra.TimeoutOptions{}
	if !request.Extensions.NeverTimeout {
		if request.AutoPause {
			timeoutOptions.ShutdownTime = TimeAfterSeconds(now, sc.maxTimeout)
			timeoutOptions.PauseTime = TimeAfterSeconds(now, request.Timeout)
		} else {
			timeoutOptions.ShutdownTime = TimeAfterSeconds(now, request.Timeout)
		}
	}
	sbx.SetTimeout(timeoutOptions)
	log.Info("timeout options calculated", "options", timeoutOptions)

	// propagate annotations to sandbox
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	for k, v := range request.Metadata {
		annotations[k] = v
	}
	sbx.SetAnnotations(annotations)

	// propagate labels to sandbox
	labels := sbx.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	for k, v := range request.Extensions.Labels {
		labels[k] = v
	}
	sbx.SetLabels(labels)

	// propagate annotations to podtemplate
	labels = sbx.GetPodLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	for k, v := range request.Extensions.Labels {
		labels[k] = v
	}
	sbx.SetPodLabels(labels)
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
