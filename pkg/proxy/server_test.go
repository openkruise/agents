package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/stretchr/testify/assert"
)

func TestServer_handleRefresh(t *testing.T) {
	tests := []struct {
		name           string
		body           string
		expectedCode   int
		expectedError  bool
		expectDeleted  bool
		expectRouteSet bool
	}{
		{
			name:          "invalid json body",
			body:          "invalid json",
			expectedCode:  http.StatusBadRequest,
			expectedError: true,
		},
		{
			name: "dead state should delete route",
			body: mustMarshal(Route{
				ID:              "sandbox-1",
				IP:              "10.0.0.1",
				State:           v1alpha1.SandboxStateDead,
				ResourceVersion: "1",
			}),
			expectedCode:  http.StatusNoContent,
			expectedError: false,
			expectDeleted: true,
		},
		{
			name: "running state should set route",
			body: mustMarshal(Route{
				ID:              "sandbox-2",
				IP:              "10.0.0.2",
				State:           v1alpha1.SandboxStateRunning,
				ResourceVersion: "1",
			}),
			expectedCode:   http.StatusNoContent,
			expectedError:  false,
			expectRouteSet: true,
		},
		{
			name: "available state should set route",
			body: mustMarshal(Route{
				ID:              "sandbox-3",
				IP:              "10.0.0.3",
				State:           v1alpha1.SandboxStateAvailable,
				ResourceVersion: "1",
			}),
			expectedCode:   http.StatusNoContent,
			expectedError:  false,
			expectRouteSet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create server with empty opts
			s := NewServer(nil, config.SandboxManagerOptions{})

			// Pre-set a route for delete test
			if tt.expectDeleted {
				route := Route{
					ID:              "sandbox-1",
					IP:              "10.0.0.1",
					State:           v1alpha1.SandboxStateRunning,
					ResourceVersion: "1",
				}
				s.routes.Store(route.ID, route)
			}

			// Create request
			req := httptest.NewRequest(http.MethodPost, RefreshAPI, strings.NewReader(tt.body))

			// Call handleRefresh
			resp, apiErr := s.handleRefresh(req)

			// Verify response
			if tt.expectedError {
				assert.NotNil(t, apiErr)
				assert.Equal(t, tt.expectedCode, apiErr.Code)
			} else {
				assert.Nil(t, apiErr)
				assert.Equal(t, tt.expectedCode, resp.Code)
			}

			// Verify route deletion
			if tt.expectDeleted {
				_, loaded := s.routes.Load("sandbox-1")
				assert.False(t, loaded, "route should be deleted")
			}

			// Verify route set
			if tt.expectRouteSet {
				var routeID string
				if tt.name == "running state should set route" {
					routeID = "sandbox-2"
				} else if tt.name == "available state should set route" {
					routeID = "sandbox-3"
				}
				_, loaded := s.routes.Load(routeID)
				assert.True(t, loaded, "route should be set")
			}
		})
	}
}

func mustMarshal(v interface{}) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(data)
}

func TestServer_handleRefresh_EmptyBody(t *testing.T) {
	s := NewServer(nil, config.SandboxManagerOptions{})

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader([]byte{}))
	resp, apiErr := s.handleRefresh(req)

	assert.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	assert.Contains(t, apiErr.Message, "failed to unmarshal body")
	assert.Equal(t, 0, resp.Code)
}
