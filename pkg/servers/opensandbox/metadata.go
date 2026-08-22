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

	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// PatchSandboxMetadata implements PATCH /v1/sandboxes/{sandboxId}/metadata:
// a JSON Merge Patch (RFC 7396) over the sandbox's metadata, stored as
// annotations on the underlying Sandbox CR.
func (sc *Controller) PatchSandboxMetadata(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	ctx := r.Context()
	sandboxID := r.PathValue("sandboxId")

	var patch models.PatchMetadataRequest
	if apiErr := decodeJSONBody(r, &patch); apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}
	if key, found := patch.HasReservedKey(); found {
		return web.ApiResponse[*models.Sandbox]{}, apiErrorf(http.StatusBadRequest, "metadata key %q uses the reserved prefix %q", key, models.ReservedMetadataPrefix)
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}
	// PatchAnnotations persists the merge patch directly (set on non-nil,
	// delete on nil) and updates sbx in place on success, so no separate
	// refresh is needed before converting the response.
	if err := sbx.PatchAnnotations(ctx, patch); err != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErrorf(http.StatusInternalServerError, "failed to persist metadata: %v", err)
	}
	return web.ApiResponse[*models.Sandbox]{Body: sc.convertToOpenSandbox(sbx)}, nil
}
