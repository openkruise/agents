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
	"fmt"
	"net/http"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/pagination"
)

// CreateSnapshot implements POST /v1/sandboxes/{sandboxId}/snapshots,
// reusing the same Sandbox.CreateCheckpoint capability the E2B API's
// CreateSnapshot handler uses. The spec's response carries the snapshot in
// its "Creating" state; this backend's CreateCheckpoint call is synchronous,
// so — like pause/resume — the 202 here is returned only once creation has
// already reached a terminal outcome. A checkpoint ID is only known on
// success, so a Ready state is reported directly rather than a Creating
// placeholder that would immediately be stale.
func (sc *Controller) CreateSnapshot(r *http.Request) (web.ApiResponse[*models.Snapshot], *web.ApiError) {
	ctx := r.Context()
	sandboxID := r.PathValue("sandboxId")

	var request models.CreateSnapshotRequest
	if apiErr := decodeJSONBody(r, &request); apiErr != nil {
		return web.ApiResponse[*models.Snapshot]{}, apiErr
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*models.Snapshot]{}, apiErr
	}
	if state, reason := sbx.GetState(); state != agentsv1alpha1.SandboxStateRunning {
		return web.ApiResponse[*models.Snapshot]{}, &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("sandbox %s is not running (state=%s reason=%s)", sandboxID, state, reason),
		}
	}

	checkpointID, err := sbx.CreateCheckpoint(ctx, infra.CreateCheckpointOptions{})
	if err != nil {
		return web.ApiResponse[*models.Snapshot]{}, &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return web.ApiResponse[*models.Snapshot]{
		Code: http.StatusAccepted,
		Body: &models.Snapshot{
			ID:        checkpointID,
			SandboxID: sandboxID,
			Name:      request.Name,
			Status:    models.SnapshotStatus{State: models.SnapshotStateReady},
			CreatedAt: time.Now().UTC().Format(time.RFC3339),
		},
	}, nil
}

// ListSnapshots implements GET /v1/snapshots, reusing SandboxManager's
// checkpoint listing exactly as the E2B API's ListSnapshots does.
func (sc *Controller) ListSnapshots(r *http.Request) (web.ApiResponse[*models.ListSnapshotsResponse], *web.ApiError) {
	ctx := r.Context()
	user := userFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.ListSnapshotsResponse]{}, &web.ApiError{Code: http.StatusUnauthorized, Message: "caller not authenticated"}
	}
	page, pageSize, apiErr := parsePagination(r)
	if apiErr != nil {
		return web.ApiResponse[*models.ListSnapshotsResponse]{}, apiErr
	}
	sandboxIDFilter := r.URL.Query().Get("sandboxId")

	checkpoints, _, err := sc.manager.ListCheckpoints(ctx, infra.SelectSucceededCheckpointsOptions{
		Namespace: sc.namespaceOfUser(user),
		User:      user.ID.String(),
	}, &pagination.Paginator[infra.CheckpointInfo]{
		Limit: 0,
		Filter: func(cp infra.CheckpointInfo) bool {
			return sandboxIDFilter == "" || cp.SandboxID == sandboxIDFilter
		},
		GetKey:       func(cp infra.CheckpointInfo) string { return cp.CreationTimestamp },
		GetUniqueKey: func(cp infra.CheckpointInfo) string { return fmt.Sprintf("%s/%s", cp.Namespace, cp.Name) },
	})
	if err != nil {
		return web.ApiResponse[*models.ListSnapshotsResponse]{}, &web.ApiError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("failed to list snapshots: %v", err)}
	}

	totalItems := len(checkpoints)
	start := min((page-1)*pageSize, totalItems)
	end := min(start+pageSize, totalItems)
	items := make([]*models.Snapshot, 0, end-start)
	for _, cp := range checkpoints[start:end] {
		items = append(items, &models.Snapshot{
			ID:        cp.CheckpointID,
			SandboxID: cp.SandboxID,
			Status:    models.SnapshotStatus{State: models.SnapshotStateReady},
			CreatedAt: cp.CreationTimestamp,
		})
	}
	totalPages := (totalItems + pageSize - 1) / pageSize

	return web.ApiResponse[*models.ListSnapshotsResponse]{
		Body: &models.ListSnapshotsResponse{
			Items: items,
			Pagination: models.PaginationInfo{
				Page:        page,
				PageSize:    pageSize,
				TotalItems:  totalItems,
				TotalPages:  totalPages,
				HasNextPage: end < totalItems,
			},
		},
	}, nil
}
