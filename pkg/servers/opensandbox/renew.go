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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// RenewSandboxExpiration handles `POST /v1/sandboxes/{sandboxId}/renew-expiration`.
//
// Semantic translation: OpenSandbox renews an absolute expiration (`expiresAt`),
// while agents stores an absolute ShutdownTime alongside an optional auto-pause
// PauseTime. Shutdown-only sandboxes accept shorter, equal, or later future
// deadlines. The backend rechecks the latest deadlines on every retry and
// preserves PauseTime; sandboxes with a pause deadline retain strict extension.
// A sandbox without either deadline retains the existing finite-renew behavior.
// Missing, malformed, past, or over-limit deadlines are rejected with 400.
func (s *Server) RenewSandboxExpiration(r *http.Request) (web.ApiResponse[RenewSandboxExpirationResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	sandboxID := r.PathValue(pathValueSandboxID)

	sbx, apiErr := sandboxFromContextOrError(ctx)
	if apiErr != nil {
		return web.ApiResponse[RenewSandboxExpirationResponse]{}, apiErr
	}

	request, apiErr := parseRenewExpirationRequest(r)
	if apiErr != nil {
		return web.ApiResponse[RenewSandboxExpirationResponse]{}, apiErr
	}

	now := time.Now()
	newExpiresAt, err := time.Parse(time.RFC3339, request.ExpiresAt)
	if err != nil {
		return web.ApiResponse[RenewSandboxExpirationResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("invalid expiresAt %q: expected RFC3339 timestamp", request.ExpiresAt),
		}
	}
	if !newExpiresAt.After(now) {
		return web.ApiResponse[RenewSandboxExpirationResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "expiresAt must be in the future",
		}
	}
	// Bound the renewed lifetime by the same operator-visible ceiling the create
	// path uses, so renew cannot be used to outlive --e2b-max-timeout.
	if s.maxTimeout > 0 {
		maxExpiresAt := now.Add(time.Duration(s.maxTimeout) * time.Second)
		if newExpiresAt.After(maxExpiresAt) {
			return web.ApiResponse[RenewSandboxExpirationResponse]{}, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("expiresAt exceeds the maximum timeout of %d seconds", s.maxTimeout),
			}
		}
	}

	normalized := timeout.NormalizeTime(newExpiresAt)
	if err := s.manager.UpdateSandboxExpiration(ctx, sbx, normalized); err != nil {
		log.Error(err, "failed to renew sandbox expiration", "sandboxID", sandboxID)
		return web.ApiResponse[RenewSandboxExpirationResponse]{}, mapLifecycleErrorToApiError(err)
	}

	log.Info("opensandbox sandbox expiration renewed", "sandboxID", sandboxID, "expiresAt", normalized)
	return web.ApiResponse[RenewSandboxExpirationResponse]{
		Body: RenewSandboxExpirationResponse{ExpiresAt: normalized.Format(time.RFC3339)},
	}, nil
}

// parseRenewExpirationRequest decodes the renew body. Unknown fields are
// rejected so a contract drift surfaces as 400 instead of being silently
// dropped, matching the create path's decoding discipline.
func parseRenewExpirationRequest(r *http.Request) (RenewSandboxExpirationRequest, *web.ApiError) {
	var request RenewSandboxExpirationRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("invalid request body: %v", err),
		}
	}
	if request.ExpiresAt == "" {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "expiresAt is required",
		}
	}
	return request, nil
}
