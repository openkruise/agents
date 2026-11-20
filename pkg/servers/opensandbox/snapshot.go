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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/pagination"
)

const (
	pathValueSnapshotID = "snapshotId"
	snapshotStateReady  = "Ready"
)

type Snapshot struct {
	ID        string         `json:"id"`
	SandboxID string         `json:"sandboxId"`
	Status    SnapshotStatus `json:"status"`
	CreatedAt string         `json:"createdAt"`
}

type SnapshotStatus struct {
	State string `json:"state"`
}

type ListSnapshotsResponse struct {
	Items      []Snapshot     `json:"items"`
	Pagination PaginationInfo `json:"pagination"`
}

// CreateSnapshot waits for a native checkpoint and returns its completed record.
func (s *Server) CreateSnapshot(r *http.Request) (response web.ApiResponse[Snapshot], apiErr *web.ApiError) {
	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return response, &web.ApiError{Code: http.StatusInternalServerError, Message: errMessageUserNotInContext}
	}
	if apiErr := validateCreateSnapshotBody(r.Body); apiErr != nil {
		return response, apiErr
	}

	checkpoint, err := s.manager.CreateSandboxCheckpoint(ctx, user.ID.String(), infra.GetSandboxOptions{
		Namespace: NamespaceOfUser(user),
		SandboxID: r.PathValue(pathValueSandboxID),
	}, infra.CreateCheckpointOptions{})
	if err != nil {
		klog.FromContext(ctx).Error(err, "failed to create snapshot")
		switch managererrors.GetErrCode(err) {
		case managererrors.ErrorNotFound:
			return response, &web.ApiError{Code: http.StatusNotFound, Message: errMessageSandboxNotFound}
		case managererrors.ErrorInternal:
			return response, &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
		default:
			return response, mapLifecycleErrorToApiError(err)
		}
	}

	snapshot, err := projectSnapshot(checkpoint)
	if err != nil {
		klog.FromContext(ctx).Error(err, "failed to project completed checkpoint")
		return response, &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
	return web.ApiResponse[Snapshot]{Code: http.StatusAccepted, Body: snapshot}, nil
}

func validateCreateSnapshotBody(body io.Reader) *web.ApiError {
	// Accept and discard the OpenSandbox name. Manager receives the native
	// defaults, independently of any E2B extension headers.
	var request struct {
		Name *string `json:"name"`
	}
	decoder := json.NewDecoder(body)
	if err := decoder.Decode(&request); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("invalid request body: %v", err)}
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return &web.ApiError{Code: http.StatusBadRequest, Message: "request body must contain only one JSON value"}
	}
	return nil
}

// ListSnapshots returns the caller's successful checkpoints in native order.
func (s *Server) ListSnapshots(r *http.Request) (response web.ApiResponse[ListSnapshotsResponse], apiErr *web.ApiError) {
	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return response, &web.ApiError{Code: http.StatusInternalServerError, Message: errMessageUserNotInContext}
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return response, &web.ApiError{Code: http.StatusBadRequest, Message: "invalid snapshot query"}
	}
	page, pageSize, apiErr := parseListPagination(query, false)
	if apiErr != nil {
		return response, apiErr
	}

	sourceID := query.Get("sandboxId")
	states := query["state"]
	readyMatches := !query.Has("name") && (len(states) == 0 || slices.Contains(states, snapshotStateReady))
	checkpoints, _, err := s.manager.ListCheckpoints(ctx, infra.SelectSucceededCheckpointsOptions{
		Namespace: NamespaceOfUser(user),
		User:      user.ID.String(),
	}, &pagination.Paginator[infra.CheckpointInfo]{
		Filter: func(checkpoint infra.CheckpointInfo) bool {
			return readyMatches && (sourceID == "" || sourceID == checkpoint.SandboxID)
		},
		GetKey: func(checkpoint infra.CheckpointInfo) string {
			if createdAt := snapshotCreatedAt(checkpoint.CreationTimestamp); createdAt != "" {
				return createdAt
			}
			// Keep malformed timestamps in the result so projection reports them.
			return "~" + checkpoint.CreationTimestamp
		},
		GetUniqueKey: func(checkpoint infra.CheckpointInfo) string {
			return checkpoint.Namespace + "/" + checkpoint.Name
		},
	})
	if err != nil {
		klog.FromContext(ctx).Error(err, "failed to list snapshots")
		return response, mapLifecycleErrorToApiError(err)
	}

	selected, pageInfo := paginateList(checkpoints, page, pageSize)
	items := make([]Snapshot, 0, len(selected))
	for _, checkpoint := range selected {
		snapshot, err := projectSnapshot(checkpoint)
		if err != nil {
			klog.FromContext(ctx).Error(err, "failed to project snapshot")
			return response, &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
		}
		items = append(items, snapshot)
	}
	return web.ApiResponse[ListSnapshotsResponse]{Body: ListSnapshotsResponse{
		Items:      items,
		Pagination: pageInfo,
	}}, nil
}

func projectSnapshot(checkpoint infra.CheckpointInfo) (Snapshot, error) {
	if checkpoint.CheckpointID == "" || checkpoint.SandboxID == "" || checkpoint.Phase != string(agentsv1alpha1.CheckpointSucceeded) {
		return Snapshot{}, fmt.Errorf("checkpoint %q is missing required snapshot data", checkpoint.CheckpointID)
	}
	createdAt := snapshotCreatedAt(checkpoint.CreationTimestamp)
	if createdAt == "" {
		return Snapshot{}, fmt.Errorf("checkpoint %q has an invalid creation timestamp", checkpoint.CheckpointID)
	}
	return Snapshot{
		ID:        checkpoint.CheckpointID,
		SandboxID: checkpoint.SandboxID,
		Status:    SnapshotStatus{State: snapshotStateReady},
		CreatedAt: createdAt,
	}, nil
}

func snapshotCreatedAt(timestamp string) string {
	createdAt, err := time.Parse(time.RFC3339, timestamp)
	if err != nil || createdAt.IsZero() {
		return ""
	}
	return createdAt.UTC().Format(time.RFC3339)
}

// DeleteSnapshot deletes the checkpoint independently of its source sandbox.
func (s *Server) DeleteSnapshot(r *http.Request) (response web.ApiResponse[struct{}], apiErr *web.ApiError) {
	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return response, &web.ApiError{Code: http.StatusInternalServerError, Message: errMessageUserNotInContext}
	}
	snapshotID := r.PathValue(pathValueSnapshotID)
	err := s.manager.DeleteCheckpoint(ctx, user.ID.String(), infra.DeleteCheckpointOptions{
		Namespace:    NamespaceOfUser(user),
		CheckpointID: snapshotID,
	})
	if err != nil {
		switch managererrors.GetErrCode(err) {
		case managererrors.ErrorNotFound, managererrors.ErrorNotAllowed:
			// Absence and foreign ownership share the same public result.
		default:
			klog.FromContext(ctx).Error(err, "failed to delete snapshot", "snapshotID", snapshotID)
			return response, &web.ApiError{Code: http.StatusInternalServerError, Message: "failed to delete snapshot"}
		}
	}
	return web.ApiResponse[struct{}]{Code: http.StatusNoContent}, nil
}
