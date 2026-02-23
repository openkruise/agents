package e2b

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
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

	var accessToken string
	if request.Secure {
		accessToken = uuid.NewString()
	}
	claimStart := time.Now()
	opts := infra.ClaimSandboxOptions{
		Template: request.TemplateID,
		User:     user.ID.String(),
		Modifier: func(sbx infra.Sandbox) {
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
			if request.AutoPause {
				timeoutOptions.ShutdownTime = TimeAfterSeconds(now, sc.maxTimeout)
				timeoutOptions.PauseTime = TimeAfterSeconds(now, request.Timeout)
			} else {
				timeoutOptions.ShutdownTime = TimeAfterSeconds(now, request.Timeout)
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
			if !request.Extensions.SkipInitRuntime {
				route := sbx.GetRoute()
				annotations[v1alpha1.AnnotationRuntimeURL] = fmt.Sprintf("http://%s:%d", route.IP, models.EnvdPort)
				annotations[v1alpha1.AnnotationRuntimeAccessToken] = accessToken
			}
			sbx.SetAnnotations(annotations)
		},
		ReserveFailedSandbox: request.Extensions.ReserveFailedSandbox,
	}

	if !request.Extensions.SkipInitRuntime {
		opts.InitRuntime = &infra.InitRuntimeOptions{
			EnvVars:     request.EnvVars,
			AccessToken: accessToken,
		}
	}

	if extension := request.Extensions.InplaceUpdate; extension.Image != "" {
		opts.InplaceUpdate = &infra.InplaceUpdateOptions{
			Image: extension.Image,
		}
		if extension.TimeoutSeconds > 0 {
			opts.InplaceUpdate.Timeout = time.Duration(extension.TimeoutSeconds) * time.Second
		}
	}

	if request.Extensions.CSIMount.PersistentVolumeName != "" {
		driverName, csiReqConfigRaw, err := sc.csiMountOptionsConfig(ctx,
			request.Extensions.CSIMount.ContainerMountPoint, request.Extensions.CSIMount.PersistentVolumeName)
		if err != nil {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: err.Error(),
			}
		}
		opts.CSIMount = &infra.CSIMountOptions{
			Driver:     driverName,
			RequestRaw: csiReqConfigRaw,
		}
	}

	sbx, err := sc.manager.ClaimSandbox(ctx, opts)
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: err.Error(),
		}
	}
	log.Info("sandbox created", "id", sbx.GetSandboxID(), "sbx", klog.KObj(sbx), "totalCost", time.Since(claimStart))
	return web.ApiResponse[*models.Sandbox]{
		Code: http.StatusCreated,
		Body: sc.convertToE2BSandbox(sbx, accessToken),
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
