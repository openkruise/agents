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

// GET /v2/sandboxes

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/pagination"
)

type ListSandboxesRequest struct {
	Metadata  map[string]string `json:"metadata,omitempty"`
	States    []string          `json:"state,omitempty"`
	NextToken string            `json:"nextToken,omitempty"` // not implemented
	Limit     int               `json:"limit,omitempty"`
	// The following fields are parsed from URL query parameters or are internal
	// flags; they are not part of the JSON request body.
	Order           string    `json:"-"`
	Template        string    `json:"-"`
	StartedAfter    time.Time `json:"-"`
	HasStartedAfter bool      `json:"-"`
}

const (
	sandboxListOrderAsc  = "asc"
	sandboxListOrderDesc = "desc"
)

// ListSandboxes returns a list of all created sandboxes (allocated pods) This API is not ready now.
func (sc *Controller) ListSandboxes(r *http.Request) (web.ApiResponse[[]*models.Sandbox], *web.ApiError) {
	log := klog.FromContext(r.Context())
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
			Message: "User not found",
		}
	}

	request, apiErr := parseListSandboxesRequest(r)
	if apiErr != nil {
		return web.ApiResponse[[]*models.Sandbox]{}, apiErr
	}
	domain, apiErr := sc.resolveSandboxDomain(r)
	if apiErr != nil {
		return web.ApiResponse[[]*models.Sandbox]{}, apiErr
	}

	log.Info("will list sandboxes", "user", user.Name, "userID", user.ID, "request", request)

	sandboxes, nextToken, err := sc.manager.ListSandboxes(r.Context(), infra.SelectSandboxesOptions{
		Namespace: sc.getNamespaceOfUser(user),
		User:      user.ID.String(),
	}, &pagination.Paginator[infra.Sandbox]{
		Limit:      request.Limit,
		NextToken:  request.NextToken,
		Descending: request.Order != sandboxListOrderAsc,
		Filter:     getListFilter(request),
		GetKey:     sandboxClaimSortKey,
		GetUniqueKey: func(sbx infra.Sandbox) string {
			return sbx.GetSandboxID()
		},
	})
	var headers map[string]string
	if nextToken != "" {
		headers = map[string]string{
			"x-next-token": nextToken,
		}
	}
	if err != nil {
		return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Failed to list sandboxes: %v", err),
		}
	}

	e2bSandboxes := make([]*models.Sandbox, 0, len(sandboxes))
	for _, sbx := range sandboxes {
		e2bSandboxes = append(e2bSandboxes, sc.convertToE2BSandbox(sbx, "", domain))
	}
	return web.ApiResponse[[]*models.Sandbox]{
		Headers: headers,
		Body:    e2bSandboxes,
	}, nil
}

func parseListSandboxesRequest(r *http.Request) (ListSandboxesRequest, *web.ApiError) {
	request := ListSandboxesRequest{
		Metadata: make(map[string]string),
		Limit:    models.MaxListLimit,
	}
	for key, values := range r.URL.Query() {
		if len(values) == 0 {
			continue
		}
		switch key {
		case "state":
			for _, state := range strings.Split(values[0], ",") {
				innerState := convertE2bStateToInnerState(state)
				if innerState != agentsv1alpha1.SandboxStateRunning && innerState != agentsv1alpha1.SandboxStatePaused {
					return request, &web.ApiError{
						Code: http.StatusBadRequest,
						Message: fmt.Sprintf("Only '%s' and '%s' state are supported, not: '%s'",
							models.SandboxStateRunning, models.SandboxStatePaused, values[0]),
					}
				}
				request.States = append(request.States, innerState)
			}
		case "nextToken":
			nextToken, ok := singleQueryValue(values)
			if !ok {
				return request, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: "Invalid nextToken: must be a single non-empty value",
				}
			}
			request.NextToken = nextToken
		case "limit":
			raw, ok := singleQueryValue(values)
			if !ok {
				return request, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("Invalid limit: must be a single value between %d and %d", models.MinListLimit, models.MaxListLimit),
				}
			}
			limit, err := strconv.Atoi(raw)
			if err != nil || limit < models.MinListLimit || limit > models.MaxListLimit {
				return request, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("Invalid limit: %v, must be between %d and %d", raw, models.MinListLimit, models.MaxListLimit),
				}
			}
			request.Limit = limit
		case "order":
			order, ok := singleQueryValue(values)
			if !ok || (order != sandboxListOrderAsc && order != sandboxListOrderDesc) {
				return request, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: "Invalid order: must be a single value of asc or desc",
				}
			}
			request.Order = order
		case "template":
			template, ok := singleQueryValue(values)
			if !ok {
				return request, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: "Invalid template: must be a single non-empty value",
				}
			}
			request.Template = template
		case "startedAfter":
			raw, ok := singleQueryValue(values)
			if !ok {
				return request, invalidStartedAfter()
			}
			startedAfter, err := time.Parse(time.RFC3339, raw)
			if err != nil {
				return request, invalidStartedAfter()
			}
			request.StartedAfter = startedAfter
			request.HasStartedAfter = true
		case "metadata":
			if len(values) > 0 && values[0] != "" {
				decodedStr, err := url.QueryUnescape(values[0])
				if err != nil {
					return request, &web.ApiError{
						Code:    http.StatusBadRequest,
						Message: fmt.Sprintf("Invalid metadata format: %v", err),
					}
				}

				metadataPairs := strings.Split(decodedStr, "&")
				for _, pair := range metadataPairs {
					if pair == "" {
						continue
					}
					kv := strings.SplitN(pair, "=", 2)
					if len(kv) == 2 {
						key := kv[0]
						value := kv[1]
						for _, prefix := range BlackListPrefix {
							if strings.HasPrefix(key, prefix) {
								return request, &web.ApiError{
									Code:    http.StatusBadRequest,
									Message: fmt.Sprintf("Forbidden metadata key: %v", key),
								}
							}
						}
						request.Metadata[key] = value
					}
				}
			}

		default:
			// metadata
			for _, prefix := range BlackListPrefix {
				if strings.HasPrefix(key, prefix) {
					return request, &web.ApiError{
						Code:    http.StatusBadRequest,
						Message: fmt.Sprintf("Forbidden metadata key: %v", key),
					}
				}
			}
			request.Metadata[key] = values[0]
		}
	}
	return request, nil
}

// singleQueryValue reports whether a query parameter has exactly one non-empty value.
func singleQueryValue(values []string) (string, bool) {
	if len(values) != 1 || values[0] == "" {
		return "", false
	}
	return values[0], true
}

func invalidStartedAfter() *web.ApiError {
	return &web.ApiError{
		Code:    http.StatusBadRequest,
		Message: "Invalid startedAfter: must be a single RFC3339 timestamp",
	}
}

const sandboxClaimTimeLayout = "2006-01-02T15:04:05.000000000Z"

// sandboxClaimSortKey uses fixed-width UTC fractional seconds so lexical order
// matches time order, including mixed second and subsecond precision.
// Missing or unparsable claim times return an empty key, which excludes the
// sandbox from paginated results.
func sandboxClaimSortKey(sbx infra.Sandbox) string {
	claimTime, err := sbx.GetClaimTime()
	if err != nil || claimTime.IsZero() {
		return ""
	}
	return claimTime.UTC().Format(sandboxClaimTimeLayout)
}

func convertE2bStateToInnerState(e2bState string) string {
	switch e2bState {
	case models.SandboxStateRunning:
		return agentsv1alpha1.SandboxStateRunning
	case models.SandboxStatePaused:
		return agentsv1alpha1.SandboxStatePaused
	default:
		return ""
	}
}

func getListFilter(request ListSandboxesRequest) func(sbx infra.Sandbox) bool {
	if len(request.States) == 0 {
		request.States = []string{agentsv1alpha1.SandboxStateRunning, agentsv1alpha1.SandboxStatePaused}
	}
	return func(sbx infra.Sandbox) bool {
		if len(request.States) > 0 {
			state, _ := sbx.GetState()
			if !slices.Contains(request.States, state) {
				return false
			}
		}
		if request.Template != "" && sbx.GetTemplate() != request.Template {
			return false
		}
		if request.HasStartedAfter {
			claimTime, err := sbx.GetClaimTime()
			if err != nil || claimTime.Before(request.StartedAfter) {
				return false
			}
		}
		if len(request.Metadata) > 0 {
			for key, value := range request.Metadata {
				if sbx.GetAnnotations()[key] != value {
					return false
				}
			}
		}
		return true
	}
}

// ListSnapshots returns a list of all snapshots for the user
func (sc *Controller) ListSnapshots(r *http.Request) (web.ApiResponse[[]*models.Snapshot], *web.ApiError) {
	log := klog.FromContext(r.Context())
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[[]*models.Snapshot]{}, &web.ApiError{
			Message: "User not found",
		}
	}

	// Parse query parameters
	limit := models.MaxListLimit
	var nextTokenParam, sandboxID, name string
	for key, values := range r.URL.Query() {
		if len(values) == 0 {
			continue
		}
		switch key {
		case "limit":
			parsedLimit, err := strconv.Atoi(values[0])
			if err != nil || parsedLimit < models.MinListLimit || parsedLimit > models.MaxListLimit {
				return web.ApiResponse[[]*models.Snapshot]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("Invalid limit: %v, must be between %d and %d", values[0], models.MinListLimit, models.MaxListLimit),
				}
			}
			limit = parsedLimit
		case "nextToken":
			nextTokenParam = values[0]
		case "sandboxID":
			sandboxID = values[0]
		case "name":
			name = values[0]
		}
	}

	log.Info("will list snapshots",
		"user", user.Name, "userID", user.ID, "limit", limit, "sandboxID", sandboxID, "name", name, "nextToken", nextTokenParam)

	// Build filter function
	filter := func(cp infra.CheckpointInfo) bool {
		if sandboxID != "" {
			return cp.SandboxID == sandboxID
		}
		return true
	}

	checkpoints, nextToken, err := sc.listCheckpointsForUser(r.Context(), user, name, limit, nextTokenParam, filter)
	if err != nil {
		return web.ApiResponse[[]*models.Snapshot]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Failed to list snapshots: %v", err),
		}
	}
	if name != "" {
		// The paginator sorts ascending by creation timestamp, so the newest
		// match is the last element: concurrent alias replacements converge to
		// it without a second pass.
		if len(checkpoints) > 0 {
			checkpoints = checkpoints[len(checkpoints)-1:]
		}
	}

	var headers map[string]string
	if nextToken != "" {
		headers = map[string]string{
			"x-next-token": nextToken,
		}
	}

	// Convert CheckpointInfo to Snapshot
	snapshots := make([]*models.Snapshot, 0, len(checkpoints))
	for _, cp := range checkpoints {
		names := []string{}
		if cp.Alias != "" {
			names = []string{cp.Alias}
		}
		snapshots = append(snapshots, &models.Snapshot{
			SnapshotID: cp.CheckpointID,
			Names:      names,
		})
	}

	return web.ApiResponse[[]*models.Snapshot]{
		Headers: headers,
		Body:    snapshots,
	}, nil
}

// listCheckpointsForUser lists the user's checkpoints, paginated ascending.
// A named query instead pulls all matching checkpoints without pagination,
// so the caller can keep the newest of concurrent alias duplicates.
func (sc *Controller) listCheckpointsForUser(ctx context.Context, user *models.CreatedTeamAPIKey, name string, limit int, nextTokenParam string, filter func(cp infra.CheckpointInfo) bool) ([]infra.CheckpointInfo, string, error) {
	if name != "" {
		limit, nextTokenParam = 0, ""
	}
	return sc.manager.ListCheckpoints(ctx, infra.SelectSucceededCheckpointsOptions{
		Namespace: sc.getNamespaceOfUser(user),
		User:      user.ID.String(),
		Alias:     name,
	}, &pagination.Paginator[infra.CheckpointInfo]{
		Limit:     limit,
		NextToken: nextTokenParam,
		GetKey: func(cp infra.CheckpointInfo) string {
			return cp.CreationTimestamp // Sort by creation timestamp
		},
		GetUniqueKey: func(cp infra.CheckpointInfo) string {
			return fmt.Sprintf("%s/%s", cp.Namespace, cp.Name)
		},
		Filter: filter,
	})
}
