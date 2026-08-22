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

package opensandbox

import (
	"errors"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

func pauseResumeErrorCode(err error) int {
	if apierrors.IsNotFound(err) {
		return http.StatusNotFound
	}
	if managererrors.GetErrCode(err) == managererrors.ErrorConflict || errors.Is(err, cacheutils.ErrWaitTaskConflict) {
		return http.StatusConflict
	}
	return http.StatusInternalServerError
}

// PauseSandbox implements POST /v1/sandboxes/{sandboxId}/pause. Per the
// package AGENTS.md, SandboxManager.PauseSandbox is synchronous on this
// backend, so the 202 below is returned only once the sandbox has actually
// finished pausing rather than while a Pausing transition is still in flight.
func (sc *Controller) PauseSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	sandboxID := r.PathValue("sandboxId")
	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	if err := sc.manager.PauseSandbox(ctx, sbx, infra.PauseOptions{}); err != nil {
		return web.ApiResponse[struct{}]{}, apiErrorf(pauseResumeErrorCode(err), "failed to pause sandbox %s: %v", sandboxID, err)
	}
	return web.ApiResponse[struct{}]{Code: http.StatusAccepted}, nil
}

// ResumeSandbox implements POST /v1/sandboxes/{sandboxId}/resume.
func (sc *Controller) ResumeSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	sandboxID := r.PathValue("sandboxId")
	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	if err := sc.manager.ResumeSandbox(ctx, sbx, infra.ResumeOptions{}); err != nil {
		return web.ApiResponse[struct{}]{}, apiErrorf(pauseResumeErrorCode(err), "failed to resume sandbox %s: %v", sandboxID, err)
	}
	return web.ApiResponse[struct{}]{Code: http.StatusAccepted}, nil
}

// RenewSandboxExpiration implements POST /v1/sandboxes/{sandboxId}/renew-expiration.
func (sc *Controller) RenewSandboxExpiration(r *http.Request) (web.ApiResponse[*models.RenewSandboxExpirationResponse], *web.ApiError) {
	ctx := r.Context()
	sandboxID := r.PathValue("sandboxId")
	var request models.RenewSandboxExpirationRequest
	if apiErr := decodeJSONBody(r, &request); apiErr != nil {
		return web.ApiResponse[*models.RenewSandboxExpirationResponse]{}, apiErr
	}
	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*models.RenewSandboxExpirationResponse]{}, apiErr
	}
	current := sbx.GetTimeout().ShutdownTime
	if !current.IsZero() && !request.ExpiresAt.After(current) {
		return web.ApiResponse[*models.RenewSandboxExpirationResponse]{}, apiErrorf(http.StatusBadRequest, "expiresAt must be after the current expiration %s", current.Format(time.RFC3339))
	}
	newTimeout := sbx.GetTimeout()
	newTimeout.ShutdownTime = request.ExpiresAt
	result, err := sbx.SaveTimeoutWithPolicy(ctx, infra.SaveTimeoutOptions{
		Timeout: newTimeout,
	}, timeout.UpdatePolicyExtendOnly)
	if err != nil {
		return web.ApiResponse[*models.RenewSandboxExpirationResponse]{}, apiError(http.StatusInternalServerError, err.Error())
	}
	if !result.Updated {
		return web.ApiResponse[*models.RenewSandboxExpirationResponse]{}, apiError(http.StatusConflict, "expiration was not extended (a concurrent update already set a later deadline)")
	}
	return web.ApiResponse[*models.RenewSandboxExpirationResponse]{
		Body: &models.RenewSandboxExpirationResponse{ExpiresAt: request.ExpiresAt},
	}, nil
}
