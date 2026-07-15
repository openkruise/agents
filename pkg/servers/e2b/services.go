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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"k8s.io/klog/v2"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils"
)

// DescribeSandbox returns details of a specific sandbox
func (sc *Controller) DescribeSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	id := r.PathValue("sandboxID")
	log := klog.FromContext(r.Context())
	log.Info("describe sandbox", "id", id)

	// Use liveSandboxStates so that dead sandboxes return 404 (NotFound).
	// The E2B SDK's SandboxState enum does not include "dead", so returning it
	// would cause a ValueError on the client side. A dead sandbox is effectively
	// unusable, so treating it as not-found from the API perspective is correct.
	sbx, err := sc.getSandboxOfUser(r.Context(), id, liveSandboxStates)
	if err != nil {
		log.Error(err, "failed to get sandbox", "id", id)
		return web.ApiResponse[*models.Sandbox]{}, err
	}

	domain, apiErr := sc.resolveSandboxDomain(r)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}

	return web.ApiResponse[*models.Sandbox]{
		Body: sc.convertToE2BSandbox(sbx, utils.GetAccessToken(sbx), domain),
	}, nil
}

// DeleteSandbox deletes a specific sandbox
func (sc *Controller) DeleteSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	id := r.PathValue("sandboxID")
	log := klog.FromContext(r.Context())
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}

	sbx, apiError := sc.getSandboxOfUser(r.Context(), id, claimedSandboxStates)
	if apiError != nil {
		log.Error(apiError, "failed to get sandbox, just return success", "id", id)
		return web.ApiResponse[struct{}]{
			Code: http.StatusNoContent,
		}, nil
	}

	quotaSpec := user.QuotaSpec
	if quotaSpec != nil {
		quotaSpec = quotaSpec.DeepCopy()
	}
	if err := sc.manager.DeleteSandbox(r.Context(), sandboxmanager.DeleteSandboxOptions{
		Sandbox: sbx,
		User:    user.ID.String(),
		Quota:   quotaSpec,
	}); err != nil {
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

	cdpPort, apiErr := parseCDPPort(r)
	if apiErr != nil {
		return web.ApiResponse[*browserHandShake]{}, apiErr
	}
	sbx, apiErr := sc.getSandboxOfUser(r.Context(), sandboxID, liveSandboxStates)
	if apiErr != nil {
		return web.ApiResponse[*browserHandShake]{}, apiErr
	}
	domain, apiErr := sc.resolveSandboxDomain(r)
	if apiErr != nil {
		return web.ApiResponse[*browserHandShake]{}, apiErr
	}
	sandboxAddr := sc.adapter.GetSandboxAddress(domain, r.URL.Path, sandboxID, int32(cdpPort)) // #nosec G115 -- port range

	resp, err := sbx.Request(r.Context(), r.Method, "/json/version", cdpPort, r.Body)
	if err != nil {
		return web.ApiResponse[*browserHandShake]{}, &web.ApiError{
			Message: fmt.Sprintf("Failed to proxy request to sandbox port %d: %v", cdpPort, err),
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
		fmt.Sprintf("wss://%s", sandboxAddr))
	return web.ApiResponse[*browserHandShake]{
		Code: resp.StatusCode,
		Body: &h,
	}, nil
}

func parseCDPPort(r *http.Request) (int, *web.ApiError) {
	portStr := r.URL.Query().Get("cdpPort")
	if portStr == "" {
		return models.CDPPort, nil
	}

	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Invalid cdpPort: %s, must be an integer between 1 and 65535", portStr),
		}
	}

	return port, nil
}

func (sc *Controller) Debug(_ *http.Request) (web.ApiResponse[sandboxmanager.DebugInfo], *web.ApiError) {
	return web.ApiResponse[sandboxmanager.DebugInfo]{
		Body: sc.manager.GetDebugInfo(),
	}, nil
}
