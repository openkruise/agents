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

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
)

// ---- healthServer tests ----

func TestHealthServer_Check(t *testing.T) {
	hs := &healthServer{}
	resp, err := hs.Check(context.Background(), &grpc_health_v1.HealthCheckRequest{})
	require.NoError(t, err)
	assert.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, resp.Status)
}

func TestHealthServer_List(t *testing.T) {
	hs := &healthServer{}
	resp, err := hs.List(context.Background(), &grpc_health_v1.HealthListRequest{})
	require.NoError(t, err)
	require.Contains(t, resp.Statuses, "envoy-ext-proc")
	assert.Equal(t, grpc_health_v1.HealthCheckResponse_SERVING, resp.Statuses["envoy-ext-proc"].Status)
}

func TestHealthServer_Watch(t *testing.T) {
	hs := &healthServer{}
	err := hs.Watch(&grpc_health_v1.HealthCheckRequest{}, nil)
	require.Error(t, err)
	st, ok := status.FromError(err)
	require.True(t, ok)
	assert.Equal(t, codes.Unimplemented, st.Code())
}

// ---- handleRefresh tests ----

func TestHandleRefresh_Success(t *testing.T) {
	s := newTestServer(nil)

	route := Route{ID: "sb-refresh", IP: "10.0.0.1", ResourceVersion: "1", Owner: "user1"}
	body, err := json.Marshal(route)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader(body))
	resp, apiErr := s.handleRefresh(req)

	assert.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, resp.Code)

	// Verify the route was actually stored
	got, ok := s.LoadRoute("sb-refresh")
	require.True(t, ok)
	assert.Equal(t, "10.0.0.1", got.IP)
}

func TestHandleRefresh_InvalidBody(t *testing.T) {
	s := newTestServer(nil)

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewBufferString("not-json"))
	resp, apiErr := s.handleRefresh(req)

	assert.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	assert.Empty(t, resp.Code)
}

func TestHandleRefresh_EmptyBody(t *testing.T) {
	s := newTestServer(nil)

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewBufferString("{}"))
	resp, apiErr := s.handleRefresh(req)

	assert.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, resp.Code)
}

func TestHandleRefresh_ContextPropagated(t *testing.T) {
	s := newTestServer(nil)

	route := Route{ID: "sb-ctx", IP: "9.9.9.9", ResourceVersion: "1"}
	body, err := json.Marshal(route)
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), "test-key", "test-value")
	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader(body)).WithContext(ctx)
	_, apiErr := s.handleRefresh(req)

	assert.Nil(t, apiErr)
	got, ok := s.LoadRoute("sb-ctx")
	require.True(t, ok)
	assert.Equal(t, "9.9.9.9", got.IP)
}

func TestHandleRefresh_OverwritesExistingRoute(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()

	// Pre-store an older route
	s.SetRoute(ctx, Route{ID: "sb-over", IP: "1.1.1.1", ResourceVersion: "1"})

	// Send a newer route via handleRefresh
	newer := Route{ID: "sb-over", IP: "2.2.2.2", ResourceVersion: "2"}
	body, _ := json.Marshal(newer)
	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader(body))
	_, apiErr := s.handleRefresh(req)

	assert.Nil(t, apiErr)
	got, _ := s.LoadRoute("sb-over")
	assert.Equal(t, "2.2.2.2", got.IP)
}

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
			s := NewServer(nil, nil, config.SandboxManagerOptions{})

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
	s := NewServer(nil, nil, config.SandboxManagerOptions{})

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader([]byte{}))
	resp, apiErr := s.handleRefresh(req)

	assert.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	assert.Contains(t, apiErr.Message, "failed to unmarshal body")
	assert.Equal(t, 0, resp.Code)
}
