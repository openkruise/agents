package e2b

// GET /v2/sandboxes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
)

type ListSandboxesRequest struct {
	Metadata  map[string]string `json:"metadata,omitempty"`
	States    []string          `json:"state,omitempty"`
	NextToken NextToken         `json:"nextToken,omitempty"` // not implemented
	Limit     int               `json:"limit,omitempty"`
}

type NextToken struct {
	CreationTimestamp time.Time `json:"creationTimestamp"`
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
			decoded, err := decodeNextToken(values[0])
			if err != nil {
				return web.ApiResponse[[]*models.Sandbox]{}, &web.ApiError{
					Code:    http.StatusBadRequest,
					Message: fmt.Sprintf("Invalid nextToken: %v", values[0]),
				}
			}
			request.NextToken = decoded
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

	sandboxes, err := sc.manager.ListSandboxes(user.ID.String(), request.Limit, getListFilter(request))
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
		Body: e2bSandboxes,
	}, nil
}

func decodeNextToken(str string) (NextToken, error) {
	var nextToken NextToken
	decoded, err := base64.StdEncoding.DecodeString(str)
	if err != nil {
		return nextToken, fmt.Errorf("failed to decode base64 string: %w", err)
	}

	if err = json.Unmarshal(decoded, &nextToken); err != nil {
		return nextToken, fmt.Errorf("failed to unmarshal JSON: %w", err)
	}

	return nextToken, nil
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
		creationTimestamp := request.NextToken.CreationTimestamp
		if !creationTimestamp.IsZero() && !sbx.GetCreationTimestamp().After(creationTimestamp) {
			return false
		}

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
