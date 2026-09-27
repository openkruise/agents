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
	"net/http"

	"k8s.io/klog/v2"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/servers/web"
)

// DeleteSandbox handles `DELETE /v1/sandboxes/{sandboxId}`. The loadOwnedSandbox
// middleware has already resolved and ownership-checked the sandbox, so a
// missing or foreign sandbox never reaches here (it returns 404 first).
//
// The OpenSandbox spec returns 204 on success and admits 404 for a missing
// sandbox, so — unlike the E2B layer's idempotent 204-on-not-found — a delete of
// an already-gone sandbox surfaces the middleware's 404. The manager decides
// between recycle and hard delete; quota release is owned by the manager.
func (s *Server) DeleteSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	sandboxID := r.PathValue(pathValueSandboxID)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: errMessageUserNotInContext,
		}
	}
	sbx, apiErr := sandboxFromContextOrError(ctx)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}

	quotaSpec := user.QuotaSpec
	if quotaSpec != nil {
		quotaSpec = quotaSpec.DeepCopy()
	}
	if err := s.manager.DeleteSandbox(ctx, sandboxmanager.DeleteSandboxOptions{
		Sandbox: sbx,
		User:    user.ID.String(),
		Quota:   quotaSpec,
	}); err != nil {
		log.Error(err, "failed to delete sandbox", "sandboxID", sandboxID)
		return web.ApiResponse[struct{}]{}, mapLifecycleErrorToApiError(err)
	}

	log.Info("opensandbox sandbox deleted", "sandboxID", sandboxID)
	return web.ApiResponse[struct{}]{Code: http.StatusNoContent}, nil
}
