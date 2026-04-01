package filter

import (
	"testing"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

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

func (m *mockFilterCallbackHandler) ClearRouteCache() {}

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

// TestDecodeHeadersSandboxHeaderPriority tests that sandbox header takes priority over host header
func TestDecodeHeadersSandboxHeaderPriority(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--sandbox-header", proxy.Route{
		IP:              "10.0.0.1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})
	r.Update("default--host-header", proxy.Route{
		IP:              "10.0.0.2",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header with both sandbox header and host header
	// Sandbox header should take priority
	header := &mockRequestHeaderMapWithHost{
		mockRequestHeaderMap: *newMockRequestHeaderMap(),
		hostValue:            "8080-default--host-header.example.com",
	}
	header.Set(DefaultSandboxHeaderName, "default--sandbox-header")
	header.Set(DefaultSandboxPortHeader, "9090")

	status := filter.DecodeHeaders(header, true)

	// Verify - should use sandbox header, not host header
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly with sandbox header info
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.1:9090", metadata["host"])
}

// TestDecodeHeadersFallbackToHostHeader tests fallback to host header when sandbox header is missing
func TestDecodeHeadersFallbackToHostHeader(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--host-sandbox", proxy.Route{
		IP:              "10.0.0.2",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header with only host header (no sandbox header)
	header := &mockRequestHeaderMapWithHost{
		mockRequestHeaderMap: *newMockRequestHeaderMap(),
		hostValue:            "8080-default--host-sandbox.example.com",
	}

	status := filter.DecodeHeaders(header, true)

	// Verify - should use host header
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly with host header info
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.2:8080", metadata["host"])
}

// TestDecodeHeadersNoHeaders tests the case when both sandbox and host headers are missing
func TestDecodeHeadersNoHeaders(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", ResourceVersion: "1"})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header without sandbox-id or valid host
	header := newMockRequestHeaderMap()

	status := filter.DecodeHeaders(header, true)

	// Verify - should continue without any side effects
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

// TestDecodeHeadersSandboxNotFound tests the case when sandbox is not found in registry
func TestDecodeHeadersSandboxNotFound(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with sandbox-id that doesn't exist
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, "nonexistent-sandbox")

	status := filter.DecodeHeaders(header, true)

	// Verify - should return LocalReply with 404
	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks.decoderCallbacks.replyStatusCode)
	assert.Contains(t, mockCallbacks.decoderCallbacks.replyBody, "nonexistent-sandbox")
	assert.Equal(t, "sandbox_not_found", mockCallbacks.decoderCallbacks.replyDetails)
}

// TestDecodeHeadersSandboxNotFoundHostFallback tests sandbox not found via host header
func TestDecodeHeadersSandboxNotFoundHostFallback(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with host in format: port-namespace--name.domain
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-nonexistent--sandbox.example.com"}

	status := filter.DecodeHeaders(header, true)

	// Verify - should return LocalReply with 404
	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks.decoderCallbacks.replyStatusCode)
	assert.Contains(t, mockCallbacks.decoderCallbacks.replyBody, "nonexistent--sandbox")
	assert.Equal(t, "sandbox_not_found", mockCallbacks.decoderCallbacks.replyDetails)
}

// TestDecodeHeadersSandboxNotRunning tests the case when sandbox exists but is not in running state
func TestDecodeHeadersSandboxNotRunning(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		{"creating state", agentsv1alpha1.SandboxStateCreating},
		{"available state", agentsv1alpha1.SandboxStateAvailable},
		{"empty state", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := registry.GetRegistry()
			defer r.Clear()
			r.Update("default--test-sandbox", proxy.Route{
				IP:              "10.0.0.1",
				State:           tt.state,
				ResourceVersion: "1",
			})

			cfg := &Config{
				SandboxHeaderName: DefaultSandboxHeaderName,
				SandboxPortHeader: DefaultSandboxPortHeader,
				HostHeaderName:    DefaultHostHeaderName,
				DefaultPort:       DefaultSandboxPort,
			}
			mockCallbacks := newMockFilterCallbackHandler()
			filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

			// Create header map with sandbox-id
			header := newMockRequestHeaderMap()
			header.Set(DefaultSandboxHeaderName, "default--test-sandbox")

			status := filter.DecodeHeaders(header, true)

			// Verify - should return LocalReply with 502
			assert.Equal(t, api.LocalReply, status)
			assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
			assert.Equal(t, 502, mockCallbacks.decoderCallbacks.replyStatusCode)
			assert.Contains(t, mockCallbacks.decoderCallbacks.replyBody, "healthy sandbox not found")
			assert.Equal(t, "sandbox_not_running", mockCallbacks.decoderCallbacks.replyDetails)
		})
	}
}

// TestDecodeHeadersSandboxNotRunningHostFallback tests non-running sandbox via host header
func TestDecodeHeadersSandboxNotRunningHostFallback(t *testing.T) {
	tests := []struct {
		name  string
		state string
	}{
		{"creating state", agentsv1alpha1.SandboxStateCreating},
		{"available state", agentsv1alpha1.SandboxStateAvailable},
		{"empty state", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := registry.GetRegistry()
			defer r.Clear()
			r.Update("default--test-sandbox", proxy.Route{
				IP:              "10.0.0.1",
				State:           tt.state,
				ResourceVersion: "1",
			})

			cfg := &Config{
				SandboxHeaderName: DefaultSandboxHeaderName,
				SandboxPortHeader: DefaultSandboxPortHeader,
				HostHeaderName:    DefaultHostHeaderName,
				DefaultPort:       DefaultSandboxPort,
			}
			mockCallbacks := newMockFilterCallbackHandler()
			filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

			// Create header map with host in format: port-namespace--name.domain
			header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-default--test-sandbox.example.com"}

			status := filter.DecodeHeaders(header, true)

			// Verify - should return LocalReply with 502
			assert.Equal(t, api.LocalReply, status)
			assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
			assert.Equal(t, 502, mockCallbacks.decoderCallbacks.replyStatusCode)
			assert.Contains(t, mockCallbacks.decoderCallbacks.replyBody, "healthy sandbox not found")
			assert.Equal(t, "sandbox_not_running", mockCallbacks.decoderCallbacks.replyDetails)
		})
	}
}

// TestDecodeHeadersSandboxRunning tests the successful case when sandbox is running
func TestDecodeHeadersSandboxRunning(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--running-sandbox", proxy.Route{
		IP:              "10.0.0.5",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with sandbox-id
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, "default--running-sandbox")

	status := filter.DecodeHeaders(header, true)

	// Verify - should continue and set upstream host
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.5:49983", metadata["host"])
}

// TestDecodeHeadersSandboxRunningHostFallback tests successful case via host header
func TestDecodeHeadersSandboxRunningHostFallback(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--running-sandbox", proxy.Route{
		IP:              "10.0.0.5",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with host in format: port-namespace--name.domain
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-default--running-sandbox.example.com"}

	status := filter.DecodeHeaders(header, true)

	// Verify - should continue and set upstream host
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly with port from host
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.5:8080", metadata["host"])
}

// TestDecodeHeadersWithCustomPort tests the case when a custom port is specified via sandbox header
func TestDecodeHeadersWithCustomPort(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--port-sandbox", proxy.Route{
		IP:              "10.0.0.6",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with sandbox-id and custom port
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, "default--port-sandbox")
	header.Set(DefaultSandboxPortHeader, "8080")

	status := filter.DecodeHeaders(header, true)

	// Verify - should continue and set upstream host with custom port
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly with custom port
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.6:8080", metadata["host"])
}

// TestDecodeHeadersWithIPv6 tests the case when sandbox has IPv6 address
func TestDecodeHeadersWithIPv6(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--ipv6-sandbox", proxy.Route{
		IP:              "2001:db8::1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with sandbox-id
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, "default--ipv6-sandbox")

	status := filter.DecodeHeaders(header, true)

	// Verify
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "2001:db8::1:49983", metadata["host"])
}

// TestDecodeHeadersWithIPv6HostFallback tests IPv6 via host header
func TestDecodeHeadersWithIPv6HostFallback(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--ipv6-sandbox", proxy.Route{
		IP:              "2001:db8::1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with host in format: port-namespace--name.domain
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-default--ipv6-sandbox.example.com"}

	status := filter.DecodeHeaders(header, true)

	// Verify
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "2001:db8::1:8080", metadata["host"])
}

// TestDecodeHeadersEmptySandboxID tests the case when sandbox-id header is empty string
func TestDecodeHeadersEmptySandboxID(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with empty sandbox-id
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, "")

	status := filter.DecodeHeaders(header, true)

	// Verify - should continue without any side effects (empty string is treated as missing)
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

// TestDecodeHeadersInvalidHostFormat tests the case when host header has invalid format
func TestDecodeHeadersInvalidHostFormat(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with invalid host format (no port prefix)
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "invalid-host-format.example.com"}

	status := filter.DecodeHeaders(header, true)

	// Verify - when parsing fails, continue to allow normal routing (pass-through)
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

// TestDecodeHeadersRegistryInteraction tests the registry Get behavior
func TestDecodeHeadersRegistryInteraction(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", ResourceVersion: "1"})

	route, ok := r.Get("default--app1")
	if !ok || route.IP != "10.0.0.1" {
		t.Fatalf("expected 10.0.0.1, got %q", route.IP)
	}

	// Missing key returns not found
	_, ok = r.Get("default--nonexistent")
	if ok {
		t.Fatal("expected not found for missing sandbox")
	}
}

// TestFilterFactory tests the FilterFactory function
func TestFilterFactory(t *testing.T) {
	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := FilterFactory(cfg, mockCallbacks)

	// Verify the returned filter is a sandboxFilter
	sf, ok := filter.(*sandboxFilter)
	assert.True(t, ok)
	assert.Equal(t, DefaultSandboxHeaderName, sf.config.SandboxHeaderName)
}

// TestDecodeHeadersMultipleRequests tests handling multiple sequential requests
func TestDecodeHeadersMultipleRequests(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	// Setup multiple sandboxes
	r.Update("ns1--sandbox1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})
	r.Update("ns2--sandbox2", proxy.Route{IP: "10.0.0.2", State: agentsv1alpha1.SandboxStateCreating, ResourceVersion: "1"})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}

	// First request - running sandbox via sandbox header
	mockCallbacks1 := newMockFilterCallbackHandler()
	filter1 := &sandboxFilter{callbacks: mockCallbacks1, config: cfg}
	header1 := newMockRequestHeaderMap()
	header1.Set(DefaultSandboxHeaderName, "ns1--sandbox1")

	status1 := filter1.DecodeHeaders(header1, true)
	assert.Equal(t, api.Continue, status1)
	assert.False(t, mockCallbacks1.decoderCallbacks.sendLocalReplyCalled)

	// Second request - non-running sandbox via sandbox header
	mockCallbacks2 := newMockFilterCallbackHandler()
	filter2 := &sandboxFilter{callbacks: mockCallbacks2, config: cfg}
	header2 := newMockRequestHeaderMap()
	header2.Set(DefaultSandboxHeaderName, "ns2--sandbox2")

	status2 := filter2.DecodeHeaders(header2, true)
	assert.Equal(t, api.LocalReply, status2)
	assert.True(t, mockCallbacks2.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks2.decoderCallbacks.replyStatusCode)

	// Third request - non-existent sandbox via sandbox header
	mockCallbacks3 := newMockFilterCallbackHandler()
	filter3 := &sandboxFilter{callbacks: mockCallbacks3, config: cfg}
	header3 := newMockRequestHeaderMap()
	header3.Set(DefaultSandboxHeaderName, "ns3--nonexistent")

	status3 := filter3.DecodeHeaders(header3, true)
	assert.Equal(t, api.LocalReply, status3)
	assert.True(t, mockCallbacks3.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks3.decoderCallbacks.replyStatusCode)
}

// TestDecodeHeadersMultipleRequestsHostFallback tests multiple requests via host header
func TestDecodeHeadersMultipleRequestsHostFallback(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	// Setup multiple sandboxes
	r.Update("ns1--sandbox1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})
	r.Update("ns2--sandbox2", proxy.Route{IP: "10.0.0.2", State: agentsv1alpha1.SandboxStateCreating, ResourceVersion: "1"})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}

	// First request - running sandbox via host header
	mockCallbacks1 := newMockFilterCallbackHandler()
	filter1 := &sandboxFilter{callbacks: mockCallbacks1, config: cfg}
	header1 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns1--sandbox1.example.com"}

	status1 := filter1.DecodeHeaders(header1, true)
	assert.Equal(t, api.Continue, status1)
	assert.False(t, mockCallbacks1.decoderCallbacks.sendLocalReplyCalled)

	// Second request - non-running sandbox via host header
	mockCallbacks2 := newMockFilterCallbackHandler()
	filter2 := &sandboxFilter{callbacks: mockCallbacks2, config: cfg}
	header2 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns2--sandbox2.example.com"}

	status2 := filter2.DecodeHeaders(header2, true)
	assert.Equal(t, api.LocalReply, status2)
	assert.True(t, mockCallbacks2.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks2.decoderCallbacks.replyStatusCode)

	// Third request - non-existent sandbox via host header
	mockCallbacks3 := newMockFilterCallbackHandler()
	filter3 := &sandboxFilter{callbacks: mockCallbacks3, config: cfg}
	header3 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns3--nonexistent.example.com"}

	status3 := filter3.DecodeHeaders(header3, true)
	assert.Equal(t, api.LocalReply, status3)
	assert.True(t, mockCallbacks3.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks3.decoderCallbacks.replyStatusCode)
}

// TestDecodeHeadersEndStreamFalse tests that endStream parameter doesn't affect the logic
func TestDecodeHeadersEndStreamFalse(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--test-sandbox", proxy.Route{
		IP:              "10.0.0.1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		SandboxHeaderName: DefaultSandboxHeaderName,
		SandboxPortHeader: DefaultSandboxPortHeader,
		HostHeaderName:    DefaultHostHeaderName,
		DefaultPort:       DefaultSandboxPort,
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, "default--test-sandbox")

	// Execute with endStream=false
	status := filter.DecodeHeaders(header, false)

	// Should still work correctly
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

func TestExtractHostInfo(t *testing.T) {
	tests := []struct {
		name        string
		headerValue string
		wantHostKey string
		wantPort    string
	}{
		{
			name:        "valid host format with port",
			headerValue: "8080-abc--def.example.com",
			wantHostKey: "abc--def",
			wantPort:    "8080",
		},
		{
			name:        "valid host format with different port",
			headerValue: "3000-myns--myservice.domain.com",
			wantHostKey: "myns--myservice",
			wantPort:    "3000",
		},
		{
			name:        "empty header value",
			headerValue: "",
			wantHostKey: "",
			wantPort:    "",
		},
		{
			name:        "invalid format - no dot",
			headerValue: "8080-abc--def",
			wantHostKey: "",
			wantPort:    "",
		},
		{
			name:        "invalid format - no port prefix",
			headerValue: "abc--def.example.com",
			wantHostKey: "",
			wantPort:    "",
		},
		{
			name:        "invalid format - no hyphen separator",
			headerValue: "8080abcdef.example.com",
			wantHostKey: "",
			wantPort:    "",
		},
		{
			name:        "valid format with multiple hyphens in name",
			headerValue: "443-ns--my-app-v2.domain.com",
			wantHostKey: "ns--my-app-v2",
			wantPort:    "443",
		},
	}

	cfg := &Config{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotHostKey, gotPort := cfg.ExtractHostInfo(tt.headerValue)
			if gotHostKey != tt.wantHostKey {
				t.Errorf("ExtractHostInfo() gotHostKey = %q, want %q", gotHostKey, tt.wantHostKey)
			}
			if gotPort != tt.wantPort {
				t.Errorf("ExtractHostInfo() gotPort = %q, want %q", gotPort, tt.wantPort)
			}
		})
	}
}
