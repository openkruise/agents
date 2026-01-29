package e2b

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
)

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
		Body: sc.convertToE2BSandbox(sbx, sbx.GetAccessToken()),
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
// Usage:
//
//	```python
//	browser_session = BrowserSession(cdp_url=f"https://api.{E2B_DOMAIN}/browser/{sandbox_id}")
//	```
func (sc *Controller) BrowserUse(r *http.Request) (web.ApiResponse[*browserHandShake], *web.ApiError) {
	sandboxID := r.PathValue("sandboxID")
	sbx, apiErr := sc.getSandboxOfUser(r.Context(), sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*browserHandShake]{}, apiErr
	}

	resp, err := sbx.Request(r.Context(), r.Method, "/json/version", models.CDPPort, r.Body)
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

func (sc *Controller) Debug(_ *http.Request) (web.ApiResponse[sandboxmanager.DebugInfo], *web.ApiError) {
	return web.ApiResponse[sandboxmanager.DebugInfo]{
		Body: sc.manager.GetDebugInfo(),
	}, nil
}
