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

package filter

import (
	"errors"
	"net/http"
	"testing"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity/oidc"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
)

type fakeJWTVerifier struct {
	claims *oidc.TrafficAccessTokenClaims
	err    error
	rawJWT string
}

func (v *fakeJWTVerifier) Verify(rawJWT string) (*oidc.TrafficAccessTokenClaims, error) {
	v.rawJWT = rawJWT
	return v.claims, v.err
}

func putTestRoute(t *testing.T, routeRegistry *registry.Registry, id string, route sandboxroute.Route) {
	t.Helper()
	activateTestRegistry(routeRegistry)
	route.ID = id
	if route.Namespace == "" {
		route.Namespace = "test"
	}
	if route.Name == "" {
		route.Name = id
	}
	if route.UID == "" {
		route.UID = types.UID("test-" + id)
	}
	if route.ResourceVersion == "" {
		route.ResourceVersion = "1"
	}
	result := routeRegistry.Upsert(route)
	require.Equal(t, sandboxroute.EventResultApplied, result.Result)
}

func activateTestRegistry(routeRegistry *registry.Registry) {
	routeRegistry.SetReady(true)
}

// useTestRegistry points registry.GetRegistry at an isolated Registry for the
// duration of the test and returns it, restoring the original getter on cleanup.
// Tests using it must not call t.Parallel, since GetRegistry is a shared
// package-level variable.
func useTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	r := registry.NewRegistry()
	orig := registry.GetRegistry
	registry.GetRegistry = func() *registry.Registry { return r }
	t.Cleanup(func() { registry.GetRegistry = orig })
	return r
}

// mockRequestHeaderMap implements api.RequestHeaderMap for testing
type mockRequestHeaderMap struct {
	headers map[string]string
}

func newMockRequestHeaderMap() *mockRequestHeaderMap {
	return &mockRequestHeaderMap{headers: make(map[string]string)}
}

func (m *mockRequestHeaderMap) Get(key string) (string, bool) {
	val, ok := m.headers[key]
	return val, ok
}

func (m *mockRequestHeaderMap) GetRaw(name string) string {
	val, _ := m.headers[name]
	return val
}

func (m *mockRequestHeaderMap) Values(key string) []string {
	if val, ok := m.headers[key]; ok {
		return []string{val}
	}
	return nil
}

func (m *mockRequestHeaderMap) Set(key, value string) {
	m.headers[key] = value
}

func (m *mockRequestHeaderMap) Add(key, value string) {
	m.headers[key] = value
}

func (m *mockRequestHeaderMap) Del(key string) {
	delete(m.headers, key)
}

func (m *mockRequestHeaderMap) Range(f func(key, value string) bool) {
	// Include pseudo-headers that a real Envoy filter would provide
	if !f(":scheme", m.Scheme()) {
		return
	}
	if !f(":authority", m.Host()) {
		return
	}
	if !f(":path", m.Path()) {
		return
	}
	for k, v := range m.headers {
		if !f(k, v) {
			break
		}
	}
}

func (m *mockRequestHeaderMap) RangeWithCopy(f func(key, value string) bool) {
	for k, v := range m.headers {
		if !f(k, v) {
			break
		}
	}
}

func (m *mockRequestHeaderMap) GetAllHeaders() map[string][]string {
	result := make(map[string][]string)
	for k, v := range m.headers {
		result[k] = []string{v}
	}
	return result
}

func (m *mockRequestHeaderMap) Scheme() string   { return "http" }
func (m *mockRequestHeaderMap) Method() string   { return "GET" }
func (m *mockRequestHeaderMap) Host() string     { return "localhost" }
func (m *mockRequestHeaderMap) Path() string     { return "/" }
func (m *mockRequestHeaderMap) SetMethod(string) {}
func (m *mockRequestHeaderMap) SetHost(string)   {}
func (m *mockRequestHeaderMap) SetPath(string)   {}

// mockRequestHeaderMapWithHost extends mockRequestHeaderMap to allow custom Host() value
type mockRequestHeaderMapWithHost struct {
	mockRequestHeaderMap
	hostValue string
}

func (m *mockRequestHeaderMapWithHost) Host() string {
	return m.hostValue
}

func (m *mockRequestHeaderMapWithHost) Range(f func(key, value string) bool) {
	if !f(":scheme", m.Scheme()) {
		return
	}
	if !f(":authority", m.hostValue) {
		return
	}
	if !f(":path", m.Path()) {
		return
	}
	for k, v := range m.headers {
		if !f(k, v) {
			break
		}
	}
}

// mockRequestHeaderMapCustom extends mockRequestHeaderMap to allow custom Host(), Path(), and Scheme() values
type mockRequestHeaderMapCustom struct {
	mockRequestHeaderMap
	hostValue   string
	pathValue   string
	schemeValue string
}

func (m *mockRequestHeaderMapCustom) Host() string {
	if m.hostValue != "" {
		return m.hostValue
	}
	return "localhost"
}

func (m *mockRequestHeaderMapCustom) Path() string {
	if m.pathValue != "" {
		return m.pathValue
	}
	return "/"
}

func (m *mockRequestHeaderMapCustom) Scheme() string {
	if m.schemeValue != "" {
		return m.schemeValue
	}
	return "http"
}

func (m *mockRequestHeaderMapCustom) Range(f func(key, value string) bool) {
	if !f(":scheme", m.Scheme()) {
		return
	}
	if !f(":authority", m.Host()) {
		return
	}
	if !f(":path", m.Path()) {
		return
	}
	for k, v := range m.headers {
		if !f(k, v) {
			break
		}
	}
}

// defaultTestAdapter creates an E2BAdapter matching the default filter config
func defaultTestAdapter() *adapters.E2BAdapter {
	return adapters.NewE2BAdapterWithOptions(0, adapters.E2BAdapterOptions{
		SandboxIDHeader:   DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeader:        DefaultHostHeaderName,
		DefaultPort:       49983,
	})
}

// putRunningTestRoute registers a Running route with the given IP; use
// putTestRoute directly when the test needs a non-Running state.
func putRunningTestRoute(t *testing.T, routeRegistry *registry.Registry, id, ip string) {
	t.Helper()
	putTestRoute(t, routeRegistry, id, sandboxroute.Route{
		IP:              ip,
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})
}

// newTestFilterWithDeps builds a sandboxFilter with explicit adapter and JWT
// manager dependencies alongside its mock callbacks.
func newTestFilterWithDeps(cfg *Config, adapter *adapters.E2BAdapter, jwtManager JWTAuthManager) (*sandboxFilter, *mockFilterCallbackHandler) {
	callbacks := newMockFilterCallbackHandler()
	return &sandboxFilter{
		callbacks:      callbacks,
		config:         cfg,
		adapter:        adapter,
		jwtAuthManager: jwtManager,
	}, callbacks
}

// newTestFilter builds a sandboxFilter from cfg with the default path-based
// adapter and returns it alongside its mock callbacks, covering the common
// per-test setup.
func newTestFilter(cfg *Config) (*sandboxFilter, *mockFilterCallbackHandler) {
	return newTestFilterWithDeps(cfg, defaultTestAdapter(), nil)
}

// newSandboxHeader returns a request header map carrying the sandbox ID header.
func newSandboxHeader(sandboxID string) *mockRequestHeaderMap {
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, sandboxID)
	return header
}

// newHostHeader returns a request header map whose Host is the given value.
func newHostHeader(host string) *mockRequestHeaderMapWithHost {
	return &mockRequestHeaderMapWithHost{
		mockRequestHeaderMap: *newMockRequestHeaderMap(),
		hostValue:            host,
	}
}

// newKruisePathHeader returns a request header map whose :path uses the kruise
// custom protocol.
func newKruisePathHeader(path string) *mockRequestHeaderMapCustom {
	return &mockRequestHeaderMapCustom{
		mockRequestHeaderMap: *newMockRequestHeaderMap(),
		pathValue:            path,
	}
}

// mockDynamicMetadata implements api.DynamicMetadata for testing
type mockDynamicMetadata struct {
	data map[string]map[string]interface{}
}

func newMockDynamicMetadata() *mockDynamicMetadata {
	return &mockDynamicMetadata{data: make(map[string]map[string]interface{})}
}

func (m *mockDynamicMetadata) Get(filterName string) map[string]interface{} {
	return m.data[filterName]
}

func (m *mockDynamicMetadata) Set(filterName string, key string, value interface{}) {
	if m.data[filterName] == nil {
		m.data[filterName] = make(map[string]interface{})
	}
	m.data[filterName][key] = value
}

// mockStreamInfo implements api.StreamInfo for testing
type mockStreamInfo struct {
	dynamicMetadata *mockDynamicMetadata
}

func newMockStreamInfo() *mockStreamInfo {
	return &mockStreamInfo{dynamicMetadata: newMockDynamicMetadata()}
}

func (m *mockStreamInfo) DynamicMetadata() api.DynamicMetadata {
	return m.dynamicMetadata
}

func (m *mockStreamInfo) GetRouteName() string                  { return "" }
func (m *mockStreamInfo) FilterChainName() string               { return "" }
func (m *mockStreamInfo) Protocol() (string, bool)              { return "", false }
func (m *mockStreamInfo) ResponseCode() (uint32, bool)          { return 0, false }
func (m *mockStreamInfo) ResponseCodeDetails() (string, bool)   { return "", false }
func (m *mockStreamInfo) AttemptCount() uint32                  { return 0 }
func (m *mockStreamInfo) DownstreamLocalAddress() string        { return "" }
func (m *mockStreamInfo) DownstreamRemoteAddress() string       { return "" }
func (m *mockStreamInfo) UpstreamLocalAddress() (string, bool)  { return "", false }
func (m *mockStreamInfo) UpstreamRemoteAddress() (string, bool) { return "", false }
func (m *mockStreamInfo) UpstreamClusterName() (string, bool)   { return "", false }
func (m *mockStreamInfo) FilterState() api.FilterState          { return nil }
func (m *mockStreamInfo) VirtualClusterName() (string, bool)    { return "", false }
func (m *mockStreamInfo) WorkerID() uint32                      { return 0 }

// mockDecoderFilterCallbacks implements api.DecoderFilterCallbacks for testing
type mockDecoderFilterCallbacks struct {
	sendLocalReplyCalled bool
	replyStatusCode      int
	replyBody            string
	replyDetails         string
}

func (m *mockDecoderFilterCallbacks) Continue(statusType api.StatusType) {}

func (m *mockDecoderFilterCallbacks) SendLocalReply(responseCode int, bodyText string, headers map[string][]string, grpcStatus int64, details string) {
	m.sendLocalReplyCalled = true
	m.replyStatusCode = responseCode
	m.replyBody = bodyText
	m.replyDetails = details
}

func (m *mockDecoderFilterCallbacks) RecoverPanic() {}

func (m *mockDecoderFilterCallbacks) AddData(data []byte, isStreaming bool) {}

func (m *mockDecoderFilterCallbacks) InjectData(data []byte) {}

func (m *mockDecoderFilterCallbacks) SetUpstreamOverrideHost(host string, strict bool) error {
	return nil
}

// mockFilterCallbackHandler implements api.FilterCallbackHandler for testing
type mockFilterCallbackHandler struct {
	streamInfo       *mockStreamInfo
	decoderCallbacks *mockDecoderFilterCallbacks
	clearRouteCalls  int
}

func newMockFilterCallbackHandler() *mockFilterCallbackHandler {
	return &mockFilterCallbackHandler{
		streamInfo:       newMockStreamInfo(),
		decoderCallbacks: &mockDecoderFilterCallbacks{},
	}
}

func (m *mockFilterCallbackHandler) StreamInfo() api.StreamInfo {
	return m.streamInfo
}

func (m *mockFilterCallbackHandler) ClearRouteCache() { m.clearRouteCalls++ }

func (m *mockFilterCallbackHandler) RefreshRouteCache() {}

func (m *mockFilterCallbackHandler) Log(level api.LogType, msg string) {}

func (m *mockFilterCallbackHandler) LogLevel() api.LogType { return api.Info }

func (m *mockFilterCallbackHandler) GetProperty(key string) (string, error) {
	return "", nil
}

func (m *mockFilterCallbackHandler) SecretManager() api.SecretManager { return nil }

func (m *mockFilterCallbackHandler) DecoderFilterCallbacks() api.DecoderFilterCallbacks {
	return m.decoderCallbacks
}

func (m *mockFilterCallbackHandler) EncoderFilterCallbacks() api.EncoderFilterCallbacks {
	return nil
}

// TestDecodeHeadersExtractionVectors covers every routable ID/port extraction
// vector: the matrix of parsing itself is authoritative in the adapters package.
func TestDecodeHeadersExtractionVectors(t *testing.T) {
	tests := []struct {
		name      string
		routes    map[string]string // route ID -> upstream IP
		request   func() api.RequestHeaderMap
		endStream bool
		wantHost  string
		wantPath  string
	}{
		{
			name: "sandbox header beats host header",
			routes: map[string]string{
				"default--sandbox-header": "10.0.0.1",
				"default--host-header":    "10.0.0.2",
			},
			request: func() api.RequestHeaderMap {
				header := newHostHeader("8080-default--host-header.example.com")
				header.Set(DefaultSandboxHeaderName, "default--sandbox-header")
				header.Set(DefaultSandboxPortHeader, "9090")
				return header
			},
			endStream: true,
			wantHost:  "10.0.0.1:9090",
		},
		{
			name:   "host header fallback carries the port",
			routes: map[string]string{"default--host-sandbox": "10.0.0.2"},
			request: func() api.RequestHeaderMap {
				return newHostHeader("8080-default--host-sandbox.example.com")
			},
			endStream: true,
			wantHost:  "10.0.0.2:8080",
		},
		{
			name:   "sandbox header uses the default port",
			routes: map[string]string{"default--running-sandbox": "10.0.0.5"},
			request: func() api.RequestHeaderMap {
				return newSandboxHeader("default--running-sandbox")
			},
			endStream: true,
			wantHost:  "10.0.0.5:49983",
		},
		{
			name:   "port header overrides the default port",
			routes: map[string]string{"default--port-sandbox": "10.0.0.6"},
			request: func() api.RequestHeaderMap {
				header := newSandboxHeader("default--port-sandbox")
				header.Set(DefaultSandboxPortHeader, "8080")
				return header
			},
			endStream: true,
			wantHost:  "10.0.0.6:8080",
		},
		{
			name:   "IPv6 upstream via sandbox header",
			routes: map[string]string{"default--ipv6-sandbox": "2001:db8::1"},
			request: func() api.RequestHeaderMap {
				return newSandboxHeader("default--ipv6-sandbox")
			},
			endStream: true,
			wantHost:  "2001:db8::1:49983",
		},
		{
			name:   "IPv6 upstream via host header",
			routes: map[string]string{"default--ipv6-sandbox": "2001:db8::1"},
			request: func() api.RequestHeaderMap {
				return newHostHeader("8080-default--ipv6-sandbox.example.com")
			},
			endStream: true,
			wantHost:  "2001:db8::1:8080",
		},
		{
			name:   "kruise custom protocol rewrites the path",
			routes: map[string]string{"ns--mysandbox": "10.0.0.10"},
			request: func() api.RequestHeaderMap {
				return newKruisePathHeader("/kruise/ns--mysandbox/3000/api/v1/data")
			},
			endStream: true,
			wantHost:  "10.0.0.10:3000",
			wantPath:  "/api/v1/data",
		},
		{
			name:   "endStream false behaves identically",
			routes: map[string]string{"default--running-sandbox": "10.0.0.5"},
			request: func() api.RequestHeaderMap {
				return newSandboxHeader("default--running-sandbox")
			},
			wantHost: "10.0.0.5:49983",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := useTestRegistry(t)
			for id, ip := range tt.routes {
				putRunningTestRoute(t, r, id, ip)
			}
			filter, callbacks := newTestFilter(DefaultConfig())
			header := tt.request()

			status := filter.DecodeHeaders(header, tt.endStream)

			assert.Equal(t, api.Continue, status)
			assert.False(t, callbacks.decoderCallbacks.sendLocalReplyCalled)
			metadata := callbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
			require.NotNil(t, metadata)
			assert.Equal(t, tt.wantHost, metadata["host"])
			if tt.wantPath != "" {
				path, ok := header.Get(":path")
				assert.True(t, ok)
				assert.Equal(t, tt.wantPath, path)
			}
		})
	}
}

// TestDecodeHeadersLocalReplies covers every local-reply branch: missing route,
// non-running states, and registry readiness gating.
func TestDecodeHeadersLocalReplies(t *testing.T) {
	tests := []struct {
		name         string
		arrange      func(t *testing.T, r *registry.Registry)
		sandboxID    string
		wantCode     int
		wantDetails  string
		wantBodyPart string
	}{
		{
			name: "missing sandbox",
			arrange: func(t *testing.T, r *registry.Registry) {
				activateTestRegistry(r)
			},
			sandboxID:    "nonexistent-sandbox",
			wantCode:     502,
			wantDetails:  "sandbox_not_found",
			wantBodyPart: "nonexistent-sandbox",
		},
		{
			name: "creating sandbox is not routable",
			arrange: func(t *testing.T, r *registry.Registry) {
				putTestRoute(t, r, "default--test-sandbox", sandboxroute.Route{
					IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateCreating, ResourceVersion: "1",
				})
			},
			sandboxID:    "default--test-sandbox",
			wantCode:     502,
			wantDetails:  "sandbox_not_running",
			wantBodyPart: "healthy sandbox not found",
		},
		{
			name: "available sandbox is not routable",
			arrange: func(t *testing.T, r *registry.Registry) {
				putTestRoute(t, r, "default--test-sandbox", sandboxroute.Route{
					IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateAvailable, ResourceVersion: "1",
				})
			},
			sandboxID:    "default--test-sandbox",
			wantCode:     502,
			wantDetails:  "sandbox_not_running",
			wantBodyPart: "healthy sandbox not found",
		},
		{
			name: "empty state is not routable",
			arrange: func(t *testing.T, r *registry.Registry) {
				putTestRoute(t, r, "default--test-sandbox", sandboxroute.Route{
					IP: "10.0.0.1", ResourceVersion: "1",
				})
			},
			sandboxID:    "default--test-sandbox",
			wantCode:     502,
			wantDetails:  "sandbox_not_running",
			wantBodyPart: "healthy sandbox not found",
		},
		{
			name:        "registry not ready at startup",
			sandboxID:   "opaque-id",
			wantCode:    503,
			wantDetails: "gateway_not_ready",
		},
		{
			name: "registry torn down after readiness",
			arrange: func(t *testing.T, r *registry.Registry) {
				putRunningTestRoute(t, r, "opaque-id", "10.0.0.1")
				r.SetReady(false)
			},
			sandboxID:   "opaque-id",
			wantCode:    503,
			wantDetails: "gateway_not_ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := useTestRegistry(t)
			if tt.arrange != nil {
				tt.arrange(t, r)
			}
			filter, callbacks := newTestFilter(DefaultConfig())
			header := newSandboxHeader(tt.sandboxID)

			status := filter.DecodeHeaders(header, true)

			assert.Equal(t, api.LocalReply, status)
			assert.True(t, callbacks.decoderCallbacks.sendLocalReplyCalled)
			assert.Equal(t, tt.wantCode, callbacks.decoderCallbacks.replyStatusCode)
			assert.Equal(t, tt.wantDetails, callbacks.decoderCallbacks.replyDetails)
			if tt.wantBodyPart != "" {
				assert.Contains(t, callbacks.decoderCallbacks.replyBody, tt.wantBodyPart)
			}
		})
	}
}

// TestDecodeHeadersPassthrough covers the adapter-map-error branch: requests
// with no extractable sandbox identity continue to normal routing.
func TestDecodeHeadersPassthrough(t *testing.T) {
	tests := []struct {
		name    string
		request func() api.RequestHeaderMap
	}{
		{
			name:    "no extractable headers",
			request: func() api.RequestHeaderMap { return newMockRequestHeaderMap() },
		},
		{
			name:    "empty sandbox ID header",
			request: func() api.RequestHeaderMap { return newSandboxHeader("") },
		},
		{
			name:    "invalid host format",
			request: func() api.RequestHeaderMap { return newHostHeader("invalid-host-format.example.com") },
		},
		{
			name:    "invalid kruise path without port segment",
			request: func() api.RequestHeaderMap { return newKruisePathHeader("/kruise/sandbox1234") },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := useTestRegistry(t)
			putRunningTestRoute(t, r, "default--app1", "10.0.0.1")
			filter, callbacks := newTestFilter(DefaultConfig())

			status := filter.DecodeHeaders(tt.request(), true)

			assert.Equal(t, api.Continue, status)
			assert.False(t, callbacks.decoderCallbacks.sendLocalReplyCalled)
		})
	}
}

func TestDecodeHeadersRuntimeMTLSRouting(t *testing.T) {
	tests := []struct {
		name     string
		enabled  bool
		request  func() api.RequestHeaderMap
		wantMTLS bool
		wantHost string
		wantPath string
	}{
		{
			name: "disabled default port remains plaintext",
			request: func() api.RequestHeaderMap {
				header := newMockRequestHeaderMap()
				header.Set(DefaultSandboxHeaderName, "default--runtime-mtls")
				return header
			},
			wantHost: "10.0.0.9:49983",
		},
		{
			name:    "enabled default port",
			enabled: true,
			request: func() api.RequestHeaderMap {
				header := newMockRequestHeaderMap()
				header.Set(DefaultSandboxHeaderName, "default--runtime-mtls")
				return header
			},
			wantMTLS: true,
			wantHost: "10.0.0.9:49983",
		},
		{
			name:    "enabled explicit runtime port",
			enabled: true,
			request: func() api.RequestHeaderMap {
				header := newMockRequestHeaderMap()
				header.Set(DefaultSandboxHeaderName, "default--runtime-mtls")
				header.Set(DefaultSandboxPortHeader, "49983")
				return header
			},
			wantMTLS: true,
			wantHost: "10.0.0.9:49983",
		},
		{
			name:    "enabled hostname runtime port",
			enabled: true,
			request: func() api.RequestHeaderMap {
				return newHostHeader("49983-default--runtime-mtls.example.com")
			},
			wantMTLS: true,
			wantHost: "10.0.0.9:49983",
		},
		{
			name:    "enabled customized path runtime port",
			enabled: true,
			request: func() api.RequestHeaderMap {
				return newKruisePathHeader("/kruise/default--runtime-mtls/49983/health")
			},
			wantMTLS: true,
			wantHost: "10.0.0.9:49983",
			wantPath: "/health",
		},
		{
			name:    "enabled non-runtime port remains plaintext",
			enabled: true,
			request: func() api.RequestHeaderMap {
				header := newMockRequestHeaderMap()
				header.Set(DefaultSandboxHeaderName, "default--runtime-mtls")
				header.Set(DefaultSandboxPortHeader, "8080")
				return header
			},
			wantHost: "10.0.0.9:8080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := useTestRegistry(t)
			putRunningTestRoute(t, r, "default--runtime-mtls", "10.0.0.9")

			cfg := DefaultConfig()
			cfg.EnableRuntimeMTLS = tt.enabled
			gatewayFilter, callbacks := newTestFilterWithDeps(cfg, NewFilterConfig(cfg).Adapter, nil)
			header := tt.request()

			status := gatewayFilter.DecodeHeaders(header, true)

			assert.Equal(t, api.Continue, status)
			assert.Equal(t, tt.wantHost, callbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]["host"])
			metadata := callbacks.streamInfo.dynamicMetadata.data[runtimeMTLSMetadataNamespace]
			if tt.wantMTLS {
				assert.Equal(t, true, metadata[runtimeMTLSMetadataKey])
				assert.Equal(t, 1, callbacks.clearRouteCalls)
			} else {
				assert.Nil(t, metadata)
				assert.Zero(t, callbacks.clearRouteCalls)
			}
			if tt.wantPath != "" {
				path, ok := header.Get(":path")
				assert.True(t, ok)
				assert.Equal(t, tt.wantPath, path)
			}
		})
	}
}

// TestFilterFactory tests the FilterFactory function
func TestFilterFactory(t *testing.T) {
	cfg := NewFilterConfig(DefaultConfig())
	mockCallbacks := newMockFilterCallbackHandler()
	filter := FilterFactory(cfg, mockCallbacks)

	// Verify the returned filter is a sandboxFilter
	sf, ok := filter.(*sandboxFilter)
	assert.True(t, ok)
	assert.Equal(t, DefaultSandboxHeaderName, sf.config.SandboxHeaderName)
	assert.NotNil(t, sf.adapter)
}

// TestDecodeHeadersAccessTokenAuth tests access token authentication logic
func TestDecodeHeadersAccessTokenAuth(t *testing.T) {
	tests := []struct {
		name                string
		disableAuth         bool
		useKruisePath       bool
		routeAccessToken    string
		requestToken        string
		setTokenHeader      bool
		expectedStatus      api.StatusType
		expectLocalReply    bool
		expectedStatusCode  int
		expectedReplyDetail string
	}{
		{
			name:             "valid token - request matches route token",
			routeAccessToken: "secret-token-123",
			requestToken:     "secret-token-123",
			setTokenHeader:   true,
			expectedStatus:   api.Continue,
			expectLocalReply: false,
		},
		{
			name:                "invalid token - request token does not match",
			routeAccessToken:    "secret-token-123",
			requestToken:        "wrong-token",
			setTokenHeader:      true,
			expectedStatus:      api.LocalReply,
			expectLocalReply:    true,
			expectedStatusCode:  401,
			expectedReplyDetail: "unauthorized",
		},
		{
			name:                "missing token - route requires token but request has none",
			routeAccessToken:    "secret-token-123",
			requestToken:        "",
			setTokenHeader:      false,
			expectedStatus:      api.LocalReply,
			expectLocalReply:    true,
			expectedStatusCode:  401,
			expectedReplyDetail: "unauthorized",
		},
		{
			name:             "no token configured - backward compatible, skip auth",
			routeAccessToken: "",
			requestToken:     "",
			setTokenHeader:   false,
			expectedStatus:   api.Continue,
			expectLocalReply: false,
		},
		{
			name:             "no token configured - request carries token anyway, still allowed",
			routeAccessToken: "",
			requestToken:     "some-token",
			setTokenHeader:   true,
			expectedStatus:   api.Continue,
			expectLocalReply: false,
		},
		{
			name:             "auth disabled skips validation despite route token",
			disableAuth:      true,
			routeAccessToken: "secret-token-123",
			expectedStatus:   api.Continue,
			expectLocalReply: false,
		},
		{
			name:             "valid token via kruise path",
			useKruisePath:    true,
			routeAccessToken: "kruise-secret",
			requestToken:     "kruise-secret",
			setTokenHeader:   true,
			expectedStatus:   api.Continue,
			expectLocalReply: false,
		},
		{
			name:                "invalid token via kruise path",
			useKruisePath:       true,
			routeAccessToken:    "kruise-secret",
			requestToken:        "wrong-token",
			setTokenHeader:      true,
			expectedStatus:      api.LocalReply,
			expectLocalReply:    true,
			expectedStatusCode:  401,
			expectedReplyDetail: "unauthorized",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := useTestRegistry(t)
			putTestRoute(t, r, "default--auth-sandbox", sandboxroute.Route{
				IP:              "10.0.0.1",
				State:           agentsv1alpha1.SandboxStateRunning,
				ResourceVersion: "1",
				AccessToken:     tt.routeAccessToken,
			})

			cfg := DefaultConfig()
			cfg.EnableAuth = !tt.disableAuth
			filter, mockCallbacks := newTestFilter(cfg)

			var header api.RequestHeaderMap
			if tt.useKruisePath {
				header = newKruisePathHeader("/kruise/default--auth-sandbox/49983/api/v1/data")
			} else {
				header = newSandboxHeader("default--auth-sandbox")
			}
			if tt.setTokenHeader {
				header.Set("x-access-token", tt.requestToken)
			}

			status := filter.DecodeHeaders(header, true)

			assert.Equal(t, tt.expectedStatus, status)
			assert.Equal(t, tt.expectLocalReply, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
			if tt.expectLocalReply {
				assert.Equal(t, tt.expectedStatusCode, mockCallbacks.decoderCallbacks.replyStatusCode)
				assert.Equal(t, tt.expectedReplyDetail, mockCallbacks.decoderCallbacks.replyDetails)
				assert.Contains(t, mockCallbacks.decoderCallbacks.replyBody, "unauthorized")
			} else {
				// Verify upstream was set correctly for successful cases
				metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
				assert.NotNil(t, metadata)
				assert.Equal(t, "10.0.0.1:49983", metadata["host"])
			}
		})
	}
}

func TestDecodeHeadersJWTAuthentication(t *testing.T) {
	const (
		sandboxID  = "default--jwt-sandbox"
		sandboxUID = "sandbox-uid"
	)
	tests := []struct {
		name             string
		managerState     string
		claims           *oidc.TrafficAccessTokenClaims
		verifyErr        error
		routeToken       string
		headerName       string
		requestJWT       string
		expectStatus     api.StatusType
		expectHTTPCode   int
		expectJWTRemoved bool
		skipRouteAuth    bool
	}{
		{
			name:             "route without JWT requirement skips verification",
			managerState:     "missing",
			requestJWT:       "unused-jwt",
			expectStatus:     api.Continue,
			expectJWTRemoved: true,
			skipRouteAuth:    true,
		},
		{
			name:         "valid JWT ignores route UUID",
			managerState: "ready",
			claims: &oidc.TrafficAccessTokenClaims{Sandbox: oidc.SandboxClaims{
				SandboxID: sandboxID, SandboxUID: sandboxUID,
			}},
			routeToken:       "different-route-token",
			requestJWT:       "valid-jwt",
			expectStatus:     api.Continue,
			expectJWTRemoved: true,
		},
		{
			name:         "valid JWT with empty route token",
			managerState: "ready",
			claims: &oidc.TrafficAccessTokenClaims{Sandbox: oidc.SandboxClaims{
				SandboxID: sandboxID, SandboxUID: sandboxUID,
			}},
			requestJWT:       "valid-jwt",
			expectStatus:     api.Continue,
			expectJWTRemoved: true,
		},
		{
			name:         "custom JWT header",
			managerState: "ready",
			claims: &oidc.TrafficAccessTokenClaims{Sandbox: oidc.SandboxClaims{
				SandboxID: sandboxID, SandboxUID: sandboxUID,
			}},
			headerName:       "x-custom-jwt",
			requestJWT:       "custom-jwt",
			expectStatus:     api.Continue,
			expectJWTRemoved: true,
		},
		{
			name:           "missing JWT",
			managerState:   "ready",
			verifyErr:      errors.New("token must not be empty"),
			expectStatus:   api.LocalReply,
			expectHTTPCode: http.StatusForbidden,
		},
		{
			name:           "invalid JWT",
			managerState:   "ready",
			requestJWT:     "invalid-jwt",
			verifyErr:      errors.New("invalid signature"),
			expectStatus:   api.LocalReply,
			expectHTTPCode: http.StatusForbidden,
		},
		{
			name:         "sandbox ID mismatch",
			managerState: "ready",
			requestJWT:   "valid-jwt",
			claims: &oidc.TrafficAccessTokenClaims{Sandbox: oidc.SandboxClaims{
				SandboxID: "other", SandboxUID: sandboxUID,
			}},
			expectStatus:   api.LocalReply,
			expectHTTPCode: http.StatusForbidden,
		},
		{
			name:         "sandbox UID mismatch",
			managerState: "ready",
			requestJWT:   "valid-jwt",
			claims: &oidc.TrafficAccessTokenClaims{Sandbox: oidc.SandboxClaims{
				SandboxID: sandboxID, SandboxUID: "other",
			}},
			expectStatus:   api.LocalReply,
			expectHTTPCode: http.StatusForbidden,
		},
		{
			name:           "manager missing",
			managerState:   "missing",
			requestJWT:     "valid-jwt",
			expectStatus:   api.LocalReply,
			expectHTTPCode: http.StatusServiceUnavailable,
		},
		{
			name:           "verifier initializing",
			managerState:   "initializing",
			requestJWT:     "valid-jwt",
			expectStatus:   api.LocalReply,
			expectHTTPCode: http.StatusServiceUnavailable,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			putTestRoute(t, useTestRegistry(t), sandboxID, sandboxroute.Route{
				ID: sandboxID, UID: types.UID(sandboxUID), IP: "10.0.0.1",
				State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1", AccessToken: tt.routeToken,
				RequireTrafficAuth: !tt.skipRouteAuth,
			})

			headerName := tt.headerName
			if headerName == "" {
				headerName = DefaultTrafficAccessTokenHeader
			}
			cfg := DefaultConfig()
			cfg.EnableAuth = true
			cfg.EnableJWTAuth = true
			cfg.TrafficAccessTokenHeader = headerName
			var manager JWTAuthManager
			var verifier *fakeJWTVerifier
			switch tt.managerState {
			case "ready":
				verifier = &fakeJWTVerifier{claims: tt.claims, err: tt.verifyErr}
				manager = &fakeJWTAuthManager{verifier: verifier}
			case "initializing":
				manager = &fakeJWTAuthManager{}
			}
			filter, callbacks := newTestFilterWithDeps(cfg, defaultTestAdapter(), manager)
			header := newSandboxHeader(sandboxID)
			header.Set(accessTokenHeader, "runtime-token")
			if tt.requestJWT != "" {
				header.Set(headerName, tt.requestJWT)
			}

			status := filter.DecodeHeaders(header, false)
			assert.Equal(t, tt.expectStatus, status)
			assert.Equal(t, tt.expectHTTPCode, callbacks.decoderCallbacks.replyStatusCode)
			assert.Equal(t, "runtime-token", header.GetRaw(accessTokenHeader), "x-access-token must be preserved")
			_, jwtPresent := header.Get(headerName)
			assert.Equal(t, !tt.expectJWTRemoved && tt.requestJWT != "", jwtPresent)
			if verifier != nil {
				assert.Equal(t, tt.requestJWT, verifier.rawJWT)
			}
		})
	}
}

func TestDecodeHeadersRequiredJWTWithoutJWTMode(t *testing.T) {
	const sandboxID = "default--jwt-required"
	tests := []struct {
		name       string
		enableAuth bool
	}{
		{name: "authentication disabled"},
		{name: "UUID authentication enabled", enableAuth: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			putTestRoute(t, useTestRegistry(t), sandboxID, sandboxroute.Route{
				ID: sandboxID, IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning,
				ResourceVersion: "1", RequireTrafficAuth: true,
			})

			cfg := DefaultConfig()
			cfg.EnableAuth = tt.enableAuth
			filter, callbacks := newTestFilter(cfg)
			header := newSandboxHeader(sandboxID)

			status := filter.DecodeHeaders(header, false)
			assert.Equal(t, api.LocalReply, status)
			assert.Equal(t, http.StatusServiceUnavailable, callbacks.decoderCallbacks.replyStatusCode)
			assert.Equal(t, "jwt_verifier_not_ready", callbacks.decoderCallbacks.replyDetails)
		})
	}
}
