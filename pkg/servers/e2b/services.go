package e2b

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	sandbox_manager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
)

// CreateSandbox allocates a Pod as a new sandbox
func (sc *Controller) CreateSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	start := time.Now()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}
	var request models.NewSandboxRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: err.Error(),
		}
	}

	if err := request.ParseExtensions(); err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Bad extension param: %s", err.Error()),
		}
	}

	for k := range request.Metadata {
		if errLists := validation.IsQualifiedName(k); len(errLists) > 0 {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Unqualified metadata key [%s]: %s", k, strings.Join(errLists, ", ")),
			}
		}

		if !ValidateMetadataKey(k) {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("Forbidden metadata key [%s]: cannot contain prefixes: %v", k, BlackListPrefix),
			}
		}
	}

	if request.Timeout == 0 {
		request.Timeout = 300
	}

	if request.Timeout < 30 || request.Timeout > sc.maxTimeout {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("timeout should between 30 and %d", sc.maxTimeout),
		}
	}

	accessToken := uuid.NewString()
	claimStart := time.Now()
	sbx, err := sc.manager.ClaimSandbox(ctx, user.ID.String(), request.TemplateID, infra.ClaimSandboxOptions{
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
			opts := infra.TimeoutOptions{}
			if request.AutoPause {
				opts.ShutdownTime = TimeAfterSeconds(claimStart, sc.maxTimeout)
				opts.PauseTime = TimeAfterSeconds(claimStart, request.Timeout)
			} else {
				opts.ShutdownTime = TimeAfterSeconds(claimStart, request.Timeout)
			}
			sbx.SetTimeout(opts)

			annotations := sbx.GetAnnotations()
			if annotations == nil {
				annotations = make(map[string]string)
			}
			for k, v := range request.Metadata {
				annotations[k] = v
			}
			annotations[v1alpha1.AnnotationEnvdAccessToken] = accessToken
			route := sbx.GetRoute()
			annotations[v1alpha1.AnnotationEnvdURL] = fmt.Sprintf("http://%s:%d", route.IP, models.EnvdPort)
			sbx.SetAnnotations(annotations)
		},
		Image: request.Extensions.Image,
	})
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: err.Error(),
		}
	}
	claimCost := time.Since(claimStart)

	initEnvdStart := time.Now()
	initEnvdCost := time.Duration(0)
	pool, ok := sc.manager.GetInfra().GetPoolByObject(sbx)
	if !ok {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Failed to get sandbox pool",
		}
	}
	if pool.GetAnnotations()[v1alpha1.AnnotationShouldInitEnvd] == utils.True {
		if err = sc.initEnvd(ctx, sbx, request.EnvVars, accessToken); err != nil {
			log.Error(err, "failed to init envd")
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Message: err.Error(),
			}
		}
		initEnvdCost = time.Since(initEnvdStart)
		log.Info("init envd done")
	}

	mountStart := time.Now()
	mountCost := time.Duration(0)
	// Currently, CSIMount depends on envd, which cannot be guaranteed to exist in all sandboxes.
	// After agent-runtime is ready, move the CSI mount logic to pool.ClaimSandbox as a built-in process.
	if request.Extensions.CSIMount.Driver != "" {
		if err = sbx.CSIMount(ctx, request.Extensions.CSIMount.Driver, request.Extensions.CSIMount.Request); err != nil {
			log.Error(err, "failed to mount storage")
			if err := sbx.Kill(ctx); err != nil {
				log.Error(err, "failed to kill sandbox", "id", sbx.GetSandboxID())
			} else {
				log.Info("sandbox killed", "id", sbx.GetSandboxID())
			}
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Message: err.Error(),
			}
		}
		mountCost = time.Since(mountStart)
		log.Info("storage mounted")
	}
	log.Info("sandbox allocated", "id", sbx.GetSandboxID(), "sbx", klog.KObj(sbx), "totalCost", time.Since(start),
		"claimCost", claimCost, "initEnvdCost", initEnvdCost, "mountCost", mountCost)
	return web.ApiResponse[*models.Sandbox]{
		Code: http.StatusCreated,
		Body: sc.convertToE2BSandbox(sbx, accessToken),
	}, nil
}

// DescribeSandbox returns details of a specific sandbox
func (sc *Controller) DescribeSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	id := r.PathValue("sandboxID")
	log := klog.FromContext(r.Context())
	log.Info("describe sandbox", "id", id)

	sbx, err := sc.getSandboxOfUser(r.Context(), id)
	if err != nil {
		log.Error(err, "failed to get sandbox", "id", id)
		return web.ApiResponse[*models.Sandbox]{}, err
	}

	return web.ApiResponse[*models.Sandbox]{
		Body: sc.convertToE2BSandbox(sbx, sbx.GetAnnotations()[v1alpha1.AnnotationEnvdAccessToken]),
	}, nil
}

// DeleteSandbox deletes a specific sandbox
func (sc *Controller) DeleteSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	id := r.PathValue("sandboxID")
	log := klog.FromContext(r.Context())
	sbx, apiError := sc.getSandboxOfUser(r.Context(), id)
	if apiError != nil {
		return web.ApiResponse[struct{}]{}, apiError
	}

	if err := sbx.Kill(r.Context()); err != nil {
		log.Error(err, "failed to delete sandbox", "id", id)
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to delete sandbox: %v", err),
		}
	}

	log.Info("sandbox deleted", "id", id)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) buildSetTimeoutOptions(autoPause bool, now time.Time, timeoutSeconds int) infra.TimeoutOptions {
	if autoPause {
		return infra.TimeoutOptions{
			PauseTime:    TimeAfterSeconds(now, timeoutSeconds),
			ShutdownTime: TimeAfterSeconds(now, sc.maxTimeout),
		}
	}
	return infra.TimeoutOptions{
		ShutdownTime: TimeAfterSeconds(now, timeoutSeconds),
	}
}

func TimeAfterSeconds(now time.Time, afterSeconds int) time.Time {
	return now.Add(time.Duration(afterSeconds) * time.Second)
}

type browserHandShake struct {
	Browser              string `json:"Browser"`
	ProtocolVersion      string `json:"Protocol-Version"`
	UserAgent            string `json:"User-Agent"`
	V8Version            string `json:"V8-Version"`
	WebKitVersion        string `json:"WebKit-Version"`
	WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
}

// BrowserUse is a cdp entry for browser_use to create a session
// Use case:
//
//	```python
//	browser_session = BrowserSession(cdp_url=f"https://api.{CDP_DOMAIN}/browser/{sandbox_id}")
//	```
func (sc *Controller) BrowserUse(r *http.Request) (web.ApiResponse[*browserHandShake], *web.ApiError) {
	sandboxID := r.PathValue("sandboxID")
	sbx, apiErr := sc.getSandboxOfUser(r.Context(), sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*browserHandShake]{}, apiErr
	}

	resp, err := sbx.Request(r, "/json/version", models.CDPPort)
	if err != nil {
		return web.ApiResponse[*browserHandShake]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to proxy request to sandbox port %d: %v", models.CDPPort, err),
		}
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return web.ApiResponse[*browserHandShake]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to read response body: %v", err),
		}
	}
	var h browserHandShake
	if err = json.Unmarshal(body, &h); err != nil {
		return web.ApiResponse[*browserHandShake]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to unmarshal response body: %v", err),
		}
	}

	h.WebSocketDebuggerURL = browserWebSocketReplacer.ReplaceAllString(h.WebSocketDebuggerURL,
		fmt.Sprintf("wss://%s", managerutils.GetSandboxAddress(sandboxID, sc.domain, models.CDPPort)))
	return web.ApiResponse[*browserHandShake]{
		Code: resp.StatusCode,
		Body: &h,
	}, nil
}

func (sc *Controller) Debug(_ *http.Request) (web.ApiResponse[sandbox_manager.DebugInfo], *web.ApiError) {
	return web.ApiResponse[sandbox_manager.DebugInfo]{
		Body: sc.manager.GetDebugInfo(),
	}, nil
}
