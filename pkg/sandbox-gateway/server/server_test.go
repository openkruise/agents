package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/stretchr/testify/assert"
)

func TestGetMemberlistBindPort(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		want     int
	}{
		{
			name:     "default port when env not set",
			envValue: "",
			want:     config.DefaultMemberlistBindPort,
		},
		{
			name:     "valid port from env",
			envValue: "8080",
			want:     8080,
		},
		{
			name:     "invalid port falls back to default",
			envValue: "invalid",
			want:     config.DefaultMemberlistBindPort,
		},
		{
			name:     "negative port falls back to default",
			envValue: "-1",
			want:     config.DefaultMemberlistBindPort,
		},
		{
			name:     "zero port falls back to default",
			envValue: "0",
			want:     config.DefaultMemberlistBindPort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up env after test
			defer os.Unsetenv(EnvMemberlistBindPort)

			if tt.envValue != "" {
				os.Setenv(EnvMemberlistBindPort, tt.envValue)
			} else {
				os.Unsetenv(EnvMemberlistBindPort)
			}

			got := getMemberlistBindPort()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewServer(t *testing.T) {
	tests := []struct {
		name         string
		port         int
		wantPort     int
		envPort      string
		wantBindPort int
	}{
		{
			name:         "with custom port",
			port:         9090,
			wantPort:     9090,
			wantBindPort: config.DefaultMemberlistBindPort,
		},
		{
			name:         "with zero port uses default",
			port:         0,
			wantPort:     proxy.SystemPort,
			wantBindPort: config.DefaultMemberlistBindPort,
		},
		{
			name:         "with custom memberlist port from env",
			port:         8080,
			wantPort:     8080,
			envPort:      "9000",
			wantBindPort: 9000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clean up env after test
			defer os.Unsetenv(EnvMemberlistBindPort)

			if tt.envPort != "" {
				os.Setenv(EnvMemberlistBindPort, tt.envPort)
			} else {
				os.Unsetenv(EnvMemberlistBindPort)
			}

			s := NewServer(nil, tt.port)
			assert.Equal(t, tt.wantPort, s.port)
			assert.Equal(t, tt.wantBindPort, s.memberlistBindPort)
			assert.Nil(t, s.client)
		})
	}
}

func TestHandleRefresh_MethodNotAllowed(t *testing.T) {
	s := &Server{}

	// Test GET request
	req := httptest.NewRequest(http.MethodGet, proxy.RefreshAPI, nil)
	rr := httptest.NewRecorder()

	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestHandleRefresh_InvalidJSON(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest(http.MethodPost, proxy.RefreshAPI, bytes.NewBufferString("invalid json"))
	rr := httptest.NewRecorder()

	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestHandleRefresh_RunningState(t *testing.T) {
	// Clear registry before test
	registry.GetRegistry().Clear()

	s := &Server{}

	route := proxy.Route{
		ID:              "test-sandbox-1",
		IP:              "10.0.0.1",
		State:           v1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
		Owner:           "test-owner",
	}
	body, _ := json.Marshal(route)

	req := httptest.NewRequest(http.MethodPost, proxy.RefreshAPI, bytes.NewReader(body))
	rr := httptest.NewRecorder()

	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Verify route was stored in registry
	got, ok := registry.GetRegistry().Get("test-sandbox-1")
	assert.True(t, ok)
	assert.Equal(t, "10.0.0.1", got.IP)
	assert.Equal(t, v1alpha1.SandboxStateRunning, got.State)
	assert.Equal(t, "test-owner", got.Owner)
}

func TestHandleRefresh_NonRunningState(t *testing.T) {
	// Clear registry and add a route first
	registry.GetRegistry().Clear()
	registry.GetRegistry().Update("test-sandbox-2", proxy.Route{
		ID:              "test-sandbox-2",
		IP:              "10.0.0.2",
		State:           v1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	s := &Server{}

	// Send a dead state route
	route := proxy.Route{
		ID:              "test-sandbox-2",
		IP:              "10.0.0.2",
		State:           v1alpha1.SandboxStateDead,
		ResourceVersion: "2",
	}
	body, _ := json.Marshal(route)

	req := httptest.NewRequest(http.MethodPost, proxy.RefreshAPI, bytes.NewReader(body))
	rr := httptest.NewRecorder()

	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Verify route was deleted from registry
	_, ok := registry.GetRegistry().Get("test-sandbox-2")
	assert.False(t, ok)
}

func TestHandleRefresh_AvailableState(t *testing.T) {
	// Clear registry and add a route first
	registry.GetRegistry().Clear()
	registry.GetRegistry().Update("test-sandbox-3", proxy.Route{
		ID:              "test-sandbox-3",
		IP:              "10.0.0.3",
		State:           v1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	s := &Server{}

	// Available state is treated as non-running and will delete the route
	route := proxy.Route{
		ID:              "test-sandbox-3",
		IP:              "10.0.0.3",
		State:           v1alpha1.SandboxStateAvailable,
		ResourceVersion: "2",
	}
	body, _ := json.Marshal(route)

	req := httptest.NewRequest(http.MethodPost, proxy.RefreshAPI, bytes.NewReader(body))
	rr := httptest.NewRecorder()

	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Verify route was deleted (only Running state keeps routes)
	_, ok := registry.GetRegistry().Get("test-sandbox-3")
	assert.False(t, ok)
}

func TestHandleRefresh_UpdateExistingRoute(t *testing.T) {
	// Clear registry and add initial route
	registry.GetRegistry().Clear()
	registry.GetRegistry().Update("test-sandbox-4", proxy.Route{
		ID:              "test-sandbox-4",
		IP:              "10.0.0.4",
		State:           v1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	s := &Server{}

	// Update with newer resource version
	route := proxy.Route{
		ID:              "test-sandbox-4",
		IP:              "10.0.0.5",
		State:           v1alpha1.SandboxStateRunning,
		ResourceVersion: "2",
	}
	body, _ := json.Marshal(route)

	req := httptest.NewRequest(http.MethodPost, proxy.RefreshAPI, bytes.NewReader(body))
	rr := httptest.NewRecorder()

	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusNoContent, rr.Code)

	// Verify route was updated
	got, ok := registry.GetRegistry().Get("test-sandbox-4")
	assert.True(t, ok)
	assert.Equal(t, "10.0.0.5", got.IP)
}

func TestHandleRefresh_EmptyBody(t *testing.T) {
	s := &Server{}

	req := httptest.NewRequest(http.MethodPost, proxy.RefreshAPI, bytes.NewReader([]byte{}))
	rr := httptest.NewRecorder()

	s.handleRefresh(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestServer_Stop_WithNilFields(t *testing.T) {
	s := &Server{
		httpServer:  nil,
		peerManager: nil,
	}

	// Should not panic when stopping a server that was never started
	err := s.Stop(nil)
	assert.NoError(t, err)
}

func TestGetMemberlistBindPort_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		want     int
	}{
		{
			name:     "very large number",
			envValue: "999999",
			want:     999999,
		},
		{
			name:     "port 1 is valid",
			envValue: "1",
			want:     1,
		},
		{
			name:     "float number is invalid",
			envValue: "8080.5",
			want:     config.DefaultMemberlistBindPort,
		},
		{
			name:     "empty string uses default",
			envValue: "",
			want:     config.DefaultMemberlistBindPort,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer os.Unsetenv(EnvMemberlistBindPort)

			if tt.envValue != "" {
				os.Setenv(EnvMemberlistBindPort, tt.envValue)
			} else {
				os.Unsetenv(EnvMemberlistBindPort)
			}

			got := getMemberlistBindPort()
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestNewServer_KubernetesClient(t *testing.T) {
	// NewServer should store the kubernetes client
	s := NewServer(nil, 8080)
	assert.Nil(t, s.client)
	assert.Equal(t, 8080, s.port)
}

func TestHandleRefresh_MultipleRoutes(t *testing.T) {
	// Clear registry before test
	registry.GetRegistry().Clear()

	s := &Server{}

	// Add multiple routes
	routes := []proxy.Route{
		{ID: "sandbox-a", IP: "10.0.1.1", State: v1alpha1.SandboxStateRunning, ResourceVersion: "1"},
		{ID: "sandbox-b", IP: "10.0.1.2", State: v1alpha1.SandboxStateRunning, ResourceVersion: "1"},
		{ID: "sandbox-c", IP: "10.0.1.3", State: v1alpha1.SandboxStateRunning, ResourceVersion: "1"},
	}

	for _, route := range routes {
		body, _ := json.Marshal(route)
		req := httptest.NewRequest(http.MethodPost, proxy.RefreshAPI, bytes.NewReader(body))
		rr := httptest.NewRecorder()
		s.handleRefresh(rr, req)
		assert.Equal(t, http.StatusNoContent, rr.Code)
	}

	// Verify all routes are stored
	allRoutes := registry.GetRegistry().List()
	assert.Len(t, allRoutes, 3)

	// Verify each route
	for _, route := range routes {
		got, ok := registry.GetRegistry().Get(route.ID)
		assert.True(t, ok)
		assert.Equal(t, route.IP, got.IP)
	}
}

func BenchmarkGetMemberlistBindPort(b *testing.B) {
	os.Setenv(EnvMemberlistBindPort, "8080")
	defer os.Unsetenv(EnvMemberlistBindPort)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		getMemberlistBindPort()
	}
}

func BenchmarkHandleRefresh(b *testing.B) {
	registry.GetRegistry().Clear()
	s := &Server{}

	route := proxy.Route{
		ID:              "bench-sandbox",
		IP:              "10.0.0.1",
		State:           v1alpha1.SandboxStateRunning,
		ResourceVersion: strconv.Itoa(b.N),
	}
	body, _ := json.Marshal(route)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodPost, proxy.RefreshAPI, bytes.NewReader(body))
		rr := httptest.NewRecorder()
		s.handleRefresh(rr, req)
	}
}
