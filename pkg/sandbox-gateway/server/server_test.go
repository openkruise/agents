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

package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

func TestHealthHandlers(t *testing.T) {
	tests := []struct {
		name            string
		path            string
		method          string
		readinessErrors []error
		includeNilCheck bool
		expectCalls     int
		expectStatus    int
	}{
		{name: "health ready", path: HealthAPI, method: http.MethodGet, expectStatus: http.StatusOK},
		{name: "health method rejected", path: HealthAPI, method: http.MethodPost, expectStatus: http.StatusMethodNotAllowed},
		{name: "readiness defaults ready", path: ReadyAPI, method: http.MethodGet, expectStatus: http.StatusOK},
		{name: "readiness succeeds", path: ReadyAPI, method: http.MethodGet, readinessErrors: []error{nil}, expectCalls: 1, expectStatus: http.StatusOK},
		{name: "multiple readiness checks succeed", path: ReadyAPI, method: http.MethodGet, readinessErrors: []error{nil, nil}, expectCalls: 2, expectStatus: http.StatusOK},
		{name: "first readiness check fails fast", path: ReadyAPI, method: http.MethodGet, readinessErrors: []error{errors.New("initializing"), nil}, expectCalls: 1, expectStatus: http.StatusServiceUnavailable},
		{name: "second readiness check fails", path: ReadyAPI, method: http.MethodGet, readinessErrors: []error{nil, errors.New("initializing")}, expectCalls: 2, expectStatus: http.StatusServiceUnavailable},
		{name: "nil readiness check is ignored", path: ReadyAPI, method: http.MethodGet, readinessErrors: []error{nil}, includeNilCheck: true, expectCalls: 1, expectStatus: http.StatusOK},
		{name: "readiness method rejected", path: ReadyAPI, method: http.MethodPost, expectStatus: http.StatusMethodNotAllowed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			checks := make([]ReadinessCheck, 0, len(tt.readinessErrors)+1)
			if tt.includeNilCheck {
				checks = append(checks, nil)
			}
			for _, readinessErr := range tt.readinessErrors {
				checks = append(checks, func() error {
					calls++
					return readinessErr
				})
			}
			server := NewServer(nil, 0, nil, checks...)
			request := httptest.NewRequest(tt.method, tt.path, nil)
			response := httptest.NewRecorder()
			if tt.path == HealthAPI {
				server.handleHealth(response, request)
			} else {
				server.handleReady(response, request)
			}
			assert.Equal(t, tt.expectStatus, response.Code)
			assert.Equal(t, tt.expectCalls, calls)
		})
	}
}

func TestStartRegistersHealthHandlers(t *testing.T) {
	server := NewServer(nil, 0, nil)
	mux := server.newServeMux()

	tests := []struct {
		name         string
		path         string
		method       string
		expectStatus int
	}{
		{name: "health route", path: HealthAPI, method: http.MethodGet, expectStatus: http.StatusOK},
		{name: "readiness route", path: ReadyAPI, method: http.MethodGet, expectStatus: http.StatusOK},
		{name: "refresh route", path: proxy.RefreshAPI, method: http.MethodGet, expectStatus: http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(tt.method, tt.path, nil)
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, request)
			assert.Equal(t, tt.expectStatus, response.Code)
		})
	}
}

func TestGetMemberlistBindPort(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		want     int
	}{
		{name: "default when unset", want: config.DefaultMemberlistBindPort},
		{name: "valid port", envValue: "8080", want: 8080},
		{name: "invalid port", envValue: "invalid", want: config.DefaultMemberlistBindPort},
		{name: "negative port", envValue: "-1", want: config.DefaultMemberlistBindPort},
		{name: "zero port", envValue: "0", want: config.DefaultMemberlistBindPort},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvMemberlistBindPort, tt.envValue)
			assert.Equal(t, tt.want, getMemberlistBindPort())
		})
	}
}

func TestNewServer(t *testing.T) {
	tests := []struct {
		name         string
		port         int
		envPort      string
		wantPort     int
		wantBindPort int
	}{
		{name: "custom port", port: 9090, wantPort: 9090, wantBindPort: config.DefaultMemberlistBindPort},
		{name: "zero uses default", wantPort: proxy.SystemPort, wantBindPort: config.DefaultMemberlistBindPort},
		{name: "negative uses default", port: -1, wantPort: proxy.SystemPort, wantBindPort: config.DefaultMemberlistBindPort},
		{name: "custom memberlist port", port: 8080, envPort: "9000", wantPort: 8080, wantBindPort: 9000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvMemberlistBindPort, tt.envPort)
			routeRegistry := newTestRegistry(t)
			server := NewServer(nil, tt.port, routeRegistry)
			assert.Equal(t, tt.wantPort, server.port)
			assert.Equal(t, tt.wantBindPort, server.memberlistBindPort)
			assert.Same(t, routeRegistry, server.registry)
			assert.Nil(t, server.client)
		})
	}
}

func TestHandleRefresh(t *testing.T) {
	tests := []struct {
		name          string
		method        string
		body          string
		route         *sandboxroute.Route
		setup         func(*registry.Registry)
		expectStatus  int
		expectID      string
		expectPresent bool
		expectIP      string
		expectAuth    bool
	}{
		{name: "method not allowed", method: http.MethodGet, expectStatus: http.StatusMethodNotAllowed},
		{name: "invalid JSON", method: http.MethodPost, body: "not-json", expectStatus: http.StatusBadRequest},
		{
			name:   "full running route preserves traffic auth",
			method: http.MethodPost,
			route: func() *sandboxroute.Route {
				route := route("short-a", "ns", "a", "uid-a", "1", v1alpha1.SandboxStateRunning)
				route.RequireTrafficAuth = true
				return route
			}(),
			expectStatus:  http.StatusNoContent,
			expectID:      "short-a",
			expectPresent: true,
			expectIP:      "10.0.0.1",
			expectAuth:    true,
		},
		{
			name:          "old peer ID-only running route accepted",
			method:        http.MethodPost,
			route:         route("ns--a", "", "", "uid-a", "1", v1alpha1.SandboxStateRunning),
			expectStatus:  http.StatusNoContent,
			expectID:      "ns--a",
			expectPresent: true,
			expectIP:      "10.0.0.1",
		},
		{
			name:         "partial ObjectKey rejected",
			method:       http.MethodPost,
			route:        route("short-a", "ns", "", "uid-a", "1", v1alpha1.SandboxStateRunning),
			expectStatus: http.StatusBadRequest,
			expectID:     "short-a",
		},
		{
			name:         "opaque ID-only route rejected",
			method:       http.MethodPost,
			route:        route("short-a", "", "", "uid-a", "1", v1alpha1.SandboxStateRunning),
			expectStatus: http.StatusBadRequest,
			expectID:     "short-a",
		},
		{
			name:         "missing UID rejected",
			method:       http.MethodPost,
			route:        route("short-a", "ns", "a", "", "1", v1alpha1.SandboxStateRunning),
			expectStatus: http.StatusBadRequest,
			expectID:     "short-a",
		},
		{
			name:         "missing ID rejected",
			method:       http.MethodPost,
			route:        route("", "ns", "a", "uid-a", "1", v1alpha1.SandboxStateRunning),
			expectStatus: http.StatusBadRequest,
		},
		{
			name:         "missing resource version rejected",
			method:       http.MethodPost,
			route:        route("short-a", "ns", "a", "uid-a", "", v1alpha1.SandboxStateRunning),
			expectStatus: http.StatusBadRequest,
			expectID:     "short-a",
		},
		{
			name:         "malformed resource version rejected",
			method:       http.MethodPost,
			route:        route("short-a", "ns", "a", "uid-a", "invalid", v1alpha1.SandboxStateRunning),
			expectStatus: http.StatusBadRequest,
			expectID:     "short-a",
		},
		{
			name:          "startup before readiness accepts mutation",
			method:        http.MethodPost,
			route:         route("short-startup", "ns", "startup", "uid-startup", "1", v1alpha1.SandboxStateRunning),
			expectStatus:  http.StatusNoContent,
			expectID:      "short-startup",
			expectPresent: true,
			expectIP:      "10.0.0.1",
		},
		{
			name:   "readiness teardown still accepts mutation",
			method: http.MethodPost,
			setup: func(registry *registry.Registry) {
				registry.Upsert(*route("existing", "ns", "existing", "uid-existing", "1", v1alpha1.SandboxStateRunning))
				registry.SetReady(false)
			},
			route:         route("short-teardown", "ns", "teardown", "uid-teardown", "1", v1alpha1.SandboxStateRunning),
			expectStatus:  http.StatusNoContent,
			expectID:      "short-teardown",
			expectPresent: true,
			expectIP:      "10.0.0.1",
		},
		{
			name:   "stale full update is idempotent",
			method: http.MethodPost,
			setup: func(registry *registry.Registry) {
				current := route("short-a", "ns", "a", "uid-a", "2", v1alpha1.SandboxStateRunning)
				current.IP = "10.0.0.2"
				registry.Upsert(*current)
			},
			route:         route("short-a", "ns", "a", "uid-a", "1", v1alpha1.SandboxStateRunning),
			expectStatus:  http.StatusNoContent,
			expectID:      "short-a",
			expectPresent: true,
			expectIP:      "10.0.0.2",
		},
		{
			name:   "full non-running route conditionally deletes",
			method: http.MethodPost,
			setup: func(registry *registry.Registry) {
				registry.Upsert(*route("short-a", "ns", "a", "uid-a", "1", v1alpha1.SandboxStateRunning))
			},
			route:        route("short-a", "ns", "a", "uid-a", "2", v1alpha1.SandboxStateDead),
			expectStatus: http.StatusNoContent,
			expectID:     "short-a",
		},
		{
			name:   "old peer ID-only delete removes current short ID",
			method: http.MethodPost,
			setup: func(registry *registry.Registry) {
				registry.Upsert(*route("short-a", "ns", "a", "uid-a", "1", v1alpha1.SandboxStateRunning))
			},
			route:        route("ns--a", "", "", "uid-a", "2", v1alpha1.SandboxStateDead),
			expectStatus: http.StatusNoContent,
			expectID:     "short-a",
		},
		{
			name:   "stale peer delete is ignored successfully",
			method: http.MethodPost,
			setup: func(registry *registry.Registry) {
				registry.Upsert(*route("short-a", "ns", "a", "uid-a", "2", v1alpha1.SandboxStateRunning))
			},
			route:         route("ns--a", "", "", "uid-a", "1", v1alpha1.SandboxStateDead),
			expectStatus:  http.StatusNoContent,
			expectID:      "short-a",
			expectPresent: true,
			expectIP:      "10.0.0.1",
		},
		{
			name:   "opaque ID-only update cannot alter full route",
			method: http.MethodPost,
			setup: func(registry *registry.Registry) {
				registry.Upsert(*route("short-a", "ns", "a", "uid-a", "1", v1alpha1.SandboxStateRunning))
			},
			route:         route("short-a", "", "", "uid-a", "99", v1alpha1.SandboxStateRunning),
			expectStatus:  http.StatusBadRequest,
			expectID:      "short-a",
			expectPresent: true,
			expectIP:      "10.0.0.1",
		},
		{
			name:   "equal-RV deletion fence returns success without resurrection",
			method: http.MethodPost,
			setup: func(registry *registry.Registry) {
				registry.Upsert(*route("old", "ns", "a", "uid-a", "1", v1alpha1.SandboxStateRunning))
				registry.Delete(sandboxroute.Delete{
					ObjectKey:       types.NamespacedName{Namespace: "ns", Name: "a"},
					ResourceVersion: "1",
				})
			},
			route:        route("old", "ns", "a", "uid-a", "1", v1alpha1.SandboxStateRunning),
			expectStatus: http.StatusNoContent,
			expectID:     "old",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			routeRegistry := newTestRegistry(t)
			if tt.setup != nil {
				tt.setup(routeRegistry)
			}
			server := &Server{registry: routeRegistry}
			body := tt.body
			if tt.route != nil {
				encoded, err := json.Marshal(tt.route)
				require.NoError(t, err)
				body = string(encoded)
			}
			request := httptest.NewRequest(tt.method, proxy.RefreshAPI, bytes.NewBufferString(body))
			response := httptest.NewRecorder()

			server.handleRefresh(response, request)

			assert.Equal(t, tt.expectStatus, response.Code)
			stored, present := routeRegistry.Get(tt.expectID)
			assert.Equal(t, tt.expectPresent, present)
			if tt.expectPresent {
				assert.Equal(t, tt.expectIP, stored.IP)
				assert.Equal(t, tt.expectAuth, stored.RequireTrafficAuth)
			}
		})
	}
}

func TestServerStopWithoutStart(t *testing.T) {
	tests := []struct {
		name   string
		server *Server
	}{
		{name: "nil runtime fields", server: &Server{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.NoError(t, tt.server.Stop(nil))
		})
	}
}

func newTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	return registry.NewRegistry()
}

func route(id, namespace, name, uid, resourceVersion, state string) *sandboxroute.Route {
	return &sandboxroute.Route{
		ID:              id,
		Namespace:       namespace,
		Name:            name,
		UID:             types.UID(uid),
		ResourceVersion: resourceVersion,
		State:           state,
		IP:              "10.0.0.1",
	}
}
