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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute"
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

	route := testIDOnlyRoute("ns--sb-refresh", v1alpha1.SandboxStateRunning, "1")
	route.IP = "10.0.0.1"
	route.Owner = "user1"
	body, err := json.Marshal(route)
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Verify the route was actually stored
	got, ok := s.LoadRoute("ns--sb-refresh")
	require.True(t, ok)
	assert.Equal(t, "10.0.0.1", got.IP)
	assert.Equal(t, "ns", got.Namespace)
	assert.Equal(t, "sb-refresh", got.Name)
}

func TestHandleRefresh_InvalidBody(t *testing.T) {
	s := newTestServer(nil)

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewBufferString("not-json"))
	rr := httptest.NewRecorder()
	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestHandleRefresh_EmptyBody(t *testing.T) {
	s := newTestServer(nil)

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewBufferString("{}"))
	rr := httptest.NewRecorder()
	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestHandleRefresh_ContextPropagated(t *testing.T) {
	s := newTestServer(nil)

	route := testIDOnlyRoute("ns--sb-ctx", v1alpha1.SandboxStateRunning, "1")
	route.IP = "9.9.9.9"
	body, err := json.Marshal(route)
	require.NoError(t, err)

	ctx := context.WithValue(context.Background(), "test-key", "test-value")
	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader(body)).WithContext(ctx)
	rr := httptest.NewRecorder()
	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusNoContent, rr.Code)
	got, ok := s.LoadRoute("ns--sb-ctx")
	require.True(t, ok)
	assert.Equal(t, "9.9.9.9", got.IP)
}

func TestHandleRefresh_OverwritesExistingRoute(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()

	// Pre-store an older route
	old := testFullRoute("ns--sb-over", "ns", "sb-over", v1alpha1.SandboxStateRunning, "1")
	old.IP = "1.1.1.1"
	s.SetRoute(ctx, old)

	// Send a newer route via handleRefresh
	newer := testIDOnlyRoute("ns--sb-over", v1alpha1.SandboxStateRunning, "2")
	newer.IP = "2.2.2.2"
	body, _ := json.Marshal(newer)
	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader(body))
	rr := httptest.NewRecorder()
	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusNoContent, rr.Code)
	got, _ := s.LoadRoute("ns--sb-over")
	assert.Equal(t, "2.2.2.2", got.IP)
}

func TestServer_handleRefresh(t *testing.T) {
	tests := []struct {
		name              string
		body              string
		expectedCode      int
		expectDeleted     bool
		expectRouteSet    bool
		expectTrafficAuth bool
	}{
		{
			name:         "invalid json body",
			body:         "invalid json",
			expectedCode: http.StatusBadRequest,
		},
		{
			name: "partial ObjectKey is rejected",
			body: mustMarshal(sandboxroute.Route{
				ID:              "partial",
				Namespace:       "ns",
				UID:             "uid-partial",
				ResourceVersion: "1",
			}),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "opaque ID-only route is rejected",
			body:         mustMarshal(testIDOnlyRoute("opaque", v1alpha1.SandboxStateRunning, "1")),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "malformed resource version is rejected",
			body:         mustMarshal(testFullRoute("short", "ns", "short", v1alpha1.SandboxStateRunning, "invalid")),
			expectedCode: http.StatusBadRequest,
		},
		{
			name: "missing required metadata is rejected",
			body: mustMarshal(sandboxroute.Route{
				ID:              "missing-uid",
				ResourceVersion: "1",
			}),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:          "dead state should delete route",
			body:          mustMarshal(testIDOnlyRoute("ns--sandbox-1", v1alpha1.SandboxStateDead, "1")),
			expectedCode:  http.StatusNoContent,
			expectDeleted: true,
		},
		{
			name: "running state should set route with traffic auth",
			body: mustMarshal(func() sandboxroute.Route {
				route := testIDOnlyRoute("ns--sandbox-2", v1alpha1.SandboxStateRunning, "1")
				route.IP = "10.0.0.2"
				route.RequireTrafficAuth = true
				return route
			}()),
			expectedCode:      http.StatusNoContent,
			expectRouteSet:    true,
			expectTrafficAuth: true,
		},
		{
			name: "available state should set route",
			body: mustMarshal(func() sandboxroute.Route {
				route := testIDOnlyRoute("ns--sandbox-3", v1alpha1.SandboxStateAvailable, "1")
				route.IP = "10.0.0.3"
				return route
			}()),
			expectedCode:   http.StatusNoContent,
			expectRouteSet: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create server with empty opts
			s := NewServer(config.SandboxManagerOptions{})

			// Pre-set a route for delete test
			if tt.expectDeleted {
				route := testFullRoute("ns--sandbox-1", "ns", "sandbox-1", v1alpha1.SandboxStateRunning, "1")
				route.IP = "10.0.0.1"
				s.SetRoute(t.Context(), route)
			}

			// Create request
			req := httptest.NewRequest(http.MethodPost, RefreshAPI, strings.NewReader(tt.body))
			rr := httptest.NewRecorder()

			// Call handleRefresh
			s.handleRefresh(rr, req)

			// Verify response
			assert.Equal(t, tt.expectedCode, rr.Code)

			// Verify route deletion
			if tt.expectDeleted {
				_, loaded := s.LoadRoute("ns--sandbox-1")
				assert.False(t, loaded, "route should be deleted")
			}

			// Verify route set
			if tt.expectRouteSet {
				var routeID string
				if tt.name == "running state should set route with traffic auth" {
					routeID = "ns--sandbox-2"
				} else if tt.name == "available state should set route" {
					routeID = "ns--sandbox-3"
				}
				rawRoute, loaded := s.LoadRoute(routeID)
				assert.True(t, loaded, "route should be set")
				if loaded {
					assert.Equal(t, tt.expectTrafficAuth, rawRoute.RequireTrafficAuth)
				}
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

func testIDOnlyRoute(id, state, resourceVersion string) sandboxroute.Route {
	return sandboxroute.Route{
		ID:              id,
		UID:             types.UID("uid-" + id),
		State:           state,
		ResourceVersion: resourceVersion,
	}
}

func testFullRoute(id, namespace, name, state, resourceVersion string) sandboxroute.Route {
	route := testIDOnlyRoute(id, state, resourceVersion)
	route.Namespace = namespace
	route.Name = name
	return route
}

func TestServer_handleRefresh_EmptyBody(t *testing.T) {
	s := NewServer(config.SandboxManagerOptions{})

	req := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader([]byte{}))
	rr := httptest.NewRecorder()
	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
	assert.Contains(t, rr.Body.String(), "failed to unmarshal body")
}

func TestHandleRefreshInvalidRouteMetric(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		expectDelta float64
	}{
		{name: "JSON decode error is not a route event", body: "not-json"},
		{name: "decoded invalid route is recorded", body: "{}", expectDelta: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(nil)
			labels := routeInvalidLabels()
			before := proxyCounterValue(t, "sandbox_route_invalid_total", labels)
			body := []byte(tt.body)

			request := httptest.NewRequest(http.MethodPost, RefreshAPI, bytes.NewReader(body))
			response := httptest.NewRecorder()
			s.handleRefresh(response, request)

			assert.Equal(t, http.StatusBadRequest, response.Code)
			assert.Equal(t, before+tt.expectDelta, proxyCounterValue(t, "sandbox_route_invalid_total", labels))
		})
	}
}

func TestHandleRefreshLegacyPeerMetric(t *testing.T) {
	s := newTestServer(nil)
	before := proxyCounterValue(t, "sandbox_route_legacy_peer_total", map[string]string{})
	body := mustMarshal(testIDOnlyRoute("ns--legacy", v1alpha1.SandboxStateRunning, "1"))

	request := httptest.NewRequest(http.MethodPost, RefreshAPI, strings.NewReader(body))
	response := httptest.NewRecorder()
	s.handleRefresh(response, request)

	assert.Equal(t, http.StatusNoContent, response.Code)
	assert.Equal(t, before+1, proxyCounterValue(t, "sandbox_route_legacy_peer_total", map[string]string{}))
}
