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
	sbx, err := sc.manager.ClaimSandbox(ctx, user.ID.String(), request.TemplateID, func(sbx infra.Sandbox) {
		sbx.SetTimeout(time.Duration(request.Timeout) * time.Second)
		annotations := sbx.GetAnnotations()
		if annotations == nil {
			annotations = make(map[string]string)
		}
		for k, v := range request.Metadata {
			annotations[k] = v
		}
		annotations[AnnotationEnvdAccessToken] = accessToken
		sbx.SetAnnotations(annotations)
	})
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Message: err.Error(),
		}
	}
	claimCost := time.Since(claimStart)

	initStart := time.Now()
	pool, ok := sc.manager.GetInfra().GetPoolByObject(sbx)
	if !ok {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Failed to get sandbox pool",
		}
	}
	if pool.GetAnnotations()[AnnotationShouldInitEnvd] == utils.True {
		start = time.Now()
		if err = sc.initEnvd(ctx, sbx, request.EnvVars, accessToken); err != nil {
			log.Error(err, "failed to init envd")
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
				Message: err.Error(),
			}
		}
		log.Info("init envd done", "cost", time.Since(start))
	}
	initCost := time.Since(initStart)

	log.Info("sandbox allocated", "id", sbx.GetSandboxID(), "sbx", klog.KObj(sbx), "totalCost", time.Since(start),
		"claimCost", claimCost, "initCost", initCost)
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
		Body: sc.convertToE2BSandbox(sbx, sbx.GetAnnotations()[AnnotationEnvdAccessToken]),
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

// SetSandboxTimeout sets the timeout of a claimed sandbox
func (sc *Controller) SetSandboxTimeout(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	err := sc.setSandboxTimeout(r, false)
	if err != nil {
		if err.Code != http.StatusNotFound {
			// Just to follow E2B spec, I don't know why it is designed
			err.Code = http.StatusInternalServerError
		}
		return web.ApiResponse[struct{}]{}, err
	}
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) setSandboxTimeout(r *http.Request, allowNonRunning bool) *web.ApiError {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	var request models.SetTimeoutRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return &web.ApiError{
			Message: err.Error(),
		}
	}
	if request.TimeoutSeconds <= 0 || request.TimeoutSeconds > sc.maxTimeout {
		return &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("timeout should between 30 and %d", sc.maxTimeout),
		}
	}

	id := r.PathValue("sandboxID")
	sbx, apiErr := sc.getSandboxOfUser(ctx, id)
	if apiErr != nil {
		return apiErr
	}

	if !allowNonRunning {
		state, reason := sbx.GetState()
		if state != v1alpha1.SandboxStateRunning {
			log.Info("cannot set sandbox timeout for sandbox not running", "name", sbx.GetName(), "state", state, "reason", reason)
			return &web.ApiError{
				Code:    http.StatusConflict,
				Message: fmt.Sprintf("sandbox %s is not running", sbx.GetName()),
			}
		}
	}

	if err := sbx.SaveTimeout(ctx, time.Duration(request.TimeoutSeconds)*time.Second); err != nil {
		return &web.ApiError{
			Message: fmt.Sprintf("Failed to set sandbox timeout: %v", err),
		}
	}

	log.Info("sandbox timeout set", "id", id, "timeout", request.TimeoutSeconds)
	return nil
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
