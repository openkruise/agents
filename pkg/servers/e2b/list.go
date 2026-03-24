package e2b

// GET /v2/sandboxes

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/klog/v2"
)

type ListSandboxesRequest struct {
	Metadata  map[string]string `json:"metadata,omitempty"`
	States    []string          `json:"state,omitempty"`
	NextToken string            `json:"nextToken,omitempty"` // not implemented
	Limit     int               `json:"limit,omitempty"`
}

// ListSandboxes returns a list of all created sandboxes (allocated pods) This API is not ready now.
func (sc *Controller) ListSandboxes(r *http.Request) (web.ApiResponse[[]*models.Sandbox], *web.ApiError) {
	log := klog.FromContext(r.Context())
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
			Message: "User not found",
		}
	}

	request := ListSandboxesRequest{
		Metadata: make(map[string]string),
		Limit:    1000,
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
					return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
						Code: http.StatusBadRequest,
						Message: fmt.Sprintf("Only '%s' and '%s' state are supported, not: '%s'",
							models.SandboxStateRunning, models.SandboxStatePaused, values[0]),
					}
				}
				request.States = append(request.States, innerState)
			}
		case "nextToken":
			request.NextToken = values[0]
		case "limit":
			limit, err := strconv.Atoi(values[0])
			if err != nil || limit <= 0 {
				return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("Invalid limit: %v", values[0]),
				}
			}
			request.Limit = limit
		case "metadata":
			if len(values) > 0 && values[0] != "" {
				decodedStr, err := url.QueryUnescape(values[0])
				if err != nil {
					return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
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
								return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
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
					return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
						Code:    http.StatusBadRequest,
						Message: fmt.Sprintf("Forbidden metadata key: %v", key),
					}
				}
			}
			request.Metadata[key] = values[0]
		}
	}

	log.Info("will list sandboxes", "user", user.Name, "userID", user.ID, "request", request)

	sandboxes, nextToken, err := sc.manager.ListSandboxes(user.ID.String(), &utils.Paginator[infra.Sandbox]{
		Limit:     request.Limit,
		NextToken: request.NextToken,
		Filter:    getListFilter(request),
		GetKey: func(sbx infra.Sandbox) string {
			return sbx.GetAnnotations()[agentsv1alpha1.AnnotationClaimTime]
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
		e2bSandboxes = append(e2bSandboxes, sc.convertToE2BSandbox(sbx, ""))
	}
	return web.ApiResponse[[]*models.Sandbox]{
		Headers: headers,
		Body:    e2bSandboxes,
	}, nil
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
	limit := 100
	var nextTokenParam, sandboxID string
	for key, values := range r.URL.Query() {
		if len(values) == 0 {
			continue
		}
		switch key {
		case "limit":
			parsedLimit, err := strconv.Atoi(values[0])
			if err != nil || parsedLimit <= 0 {
				return web.ApiResponse[[]*models.Snapshot]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("Invalid limit: %v", values[0]),
				}
			}
			limit = parsedLimit
		case "nextToken":
			nextTokenParam = values[0]
		case "sandboxID":
			sandboxID = values[0]
		}
	}

	log.Info("will list snapshots", "user", user.Name, "userID", user.ID, "limit", limit, "sandboxID", sandboxID)

	// Build filter function
	filter := func(cp infra.CheckpointInfo) bool {
		if sandboxID != "" {
			return cp.SandboxID == sandboxID
		}
		return true
	}

	checkpoints, nextToken, err := sc.manager.ListCheckpoints(user.ID.String(), &utils.Paginator[infra.CheckpointInfo]{
		Limit:     limit,
		NextToken: nextTokenParam,
		GetKey: func(cp infra.CheckpointInfo) string {
			return cp.CreationTimestamp // Sort by creation timestamp
		},
		Filter: filter,
	})
	if err != nil {
		return web.ApiResponse[[]*models.Snapshot]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Failed to list snapshots: %v", err),
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
		snapshots = append(snapshots, &models.Snapshot{
			SnapshotID: cp.CheckpointID,
			Names:      nil,
		})
	}

	return web.ApiResponse[[]*models.Snapshot]{
		Headers: headers,
		Body:    snapshots,
	}, nil
}
