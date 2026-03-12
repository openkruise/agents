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

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
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

	if extension := request.Extensions.InplaceUpdate; extension.Image != "" {
		opts.InplaceUpdate = &config.InplaceUpdateOptions{
			Image: extension.Image,
		}
	}

	if request.Extensions.WaitReadySeconds > 0 {
		opts.WaitReadyTimeout = time.Duration(request.Extensions.WaitReadySeconds) * time.Second
	}

	if len(request.Extensions.CSIMount.MountConfigs) != 0 {
		csiMountOptions := make([]config.MountConfig, 0, len(request.Extensions.CSIMount.MountConfigs))
		for _, mountConfig := range request.Extensions.CSIMount.MountConfigs {
			driverName, csiReqConfigRaw, err := sc.csiMountOptionsConfig(ctx, mountConfig.MountPath, mountConfig.PvName, mountConfig.SubPath, mountConfig.ReadOnly)
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
	if request.Extensions.CSIMount.PersistentVolumeName != "" {
		driverName, csiReqConfigRaw, err := sc.csiMountOptionsConfig(ctx,
			request.Extensions.CSIMount.ContainerMountPoint, request.Extensions.CSIMount.PersistentVolumeName, request.Extensions.CSIMount.PersistentVolumeSubpath)
		if err != nil {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: err.Error(),
			}
		}
		opts.CSIMount = &config.CSIMountOptions{
			Driver:     driverName,
			RequestRaw: csiReqConfigRaw,
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
		Body: sc.convertToE2BSandbox(sbx, sbx.GetAccessToken()),
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

	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string)
	}
	for k, v := range request.Metadata {
		annotations[k] = v
	}
	sbx.SetAnnotations(annotations)
}
