package filter

import (
	"testing"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

const (
	headerSandboxID   = "e2b-sandbox-id"
	headerSandboxPort = "e2b-sandbox-port"
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

// TestDecodeHeadersMissingSandboxID_SandboxPolicy tests the case when sandbox-id header is missing (sandbox policy)
func TestDecodeHeadersMissingSandboxID_SandboxPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", ResourceVersion: "1"})

	// Create filter with mock callbacks and sandbox policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map without sandbox-id
	header := newMockRequestHeaderMap()

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - should continue without any side effects
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

// TestDecodeHeadersMissingHost_HostPolicy tests the case when host header has invalid format (host policy)
// When ExtractHostInfo fails to parse, it returns empty sandboxID which results in 404
func TestDecodeHeadersMissingHost_HostPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", ResourceVersion: "1"})

	// Create filter with mock callbacks and host policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with invalid host format that can't be parsed
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "invalid-format"}

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - when parsing fails, sandboxID is empty and returns 404
	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks.decoderCallbacks.replyStatusCode)
}

// TestDecodeHeadersSandboxNotFound_SandboxPolicy tests the case when sandbox is not found in registry (sandbox policy)
func TestDecodeHeadersSandboxNotFound_SandboxPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()

	// Create filter with mock callbacks and sandbox policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with sandbox-id that doesn't exist
	header := newMockRequestHeaderMap()
	header.Set(headerSandboxID, "nonexistent-sandbox")

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - should return LocalReply with 404
	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks.decoderCallbacks.replyStatusCode)
	assert.Contains(t, mockCallbacks.decoderCallbacks.replyBody, "nonexistent-sandbox")
	assert.Equal(t, "sandbox_not_found", mockCallbacks.decoderCallbacks.replyDetails)
}

// TestDecodeHeadersSandboxNotFound_HostPolicy tests the case when sandbox is not found in registry (host policy)
func TestDecodeHeadersSandboxNotFound_HostPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()

	// Create filter with mock callbacks and host policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with host in format: port-namespace--name.domain
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-nonexistent--sandbox.example.com"}

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - should return LocalReply with 404
	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks.decoderCallbacks.replyStatusCode)
	assert.Contains(t, mockCallbacks.decoderCallbacks.replyBody, "nonexistent--sandbox")
	assert.Equal(t, "sandbox_not_found", mockCallbacks.decoderCallbacks.replyDetails)
}

// TestDecodeHeadersSandboxNotRunning_SandboxPolicy tests the case when sandbox exists but is not in running state (sandbox policy)
func TestDecodeHeadersSandboxNotRunning_SandboxPolicy(t *testing.T) {
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
			// Setup
			r := registry.GetRegistry()
			defer r.Clear()
			r.Update("default--test-sandbox", proxy.Route{
				IP:              "10.0.0.1",
				State:           tt.state,
				ResourceVersion: "1",
			})

			// Create filter with mock callbacks and sandbox policy
			mockCallbacks := newMockFilterCallbackHandler()
			cfg := &Config{
				HeaderMatchPolicy: HeaderMatchPolicySandbox,
				HeaderMatchName:   "e2b-sandbox-id",
				DefaultPort:       "80",
			}
			filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

			// Create header map with sandbox-id
			header := newMockRequestHeaderMap()
			header.Set(headerSandboxID, "default--test-sandbox")

			// Execute
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

// TestDecodeHeadersSandboxNotRunning_HostPolicy tests the case when sandbox exists but is not in running state (host policy)
func TestDecodeHeadersSandboxNotRunning_HostPolicy(t *testing.T) {
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
			// Setup
			r := registry.GetRegistry()
			defer r.Clear()
			r.Update("default--test-sandbox", proxy.Route{
				IP:              "10.0.0.1",
				State:           tt.state,
				ResourceVersion: "1",
			})

			// Create filter with mock callbacks and host policy
			mockCallbacks := newMockFilterCallbackHandler()
			cfg := &Config{
				HeaderMatchPolicy: HeaderMatchPolicyHost,
				DefaultPort:       "80",
			}
			filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

			// Create header map with host in format: port-namespace--name.domain
			header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-default--test-sandbox.example.com"}

			// Execute
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

// TestDecodeHeadersSandboxRunning_SandboxPolicy tests the successful case when sandbox is running (sandbox policy)
func TestDecodeHeadersSandboxRunning_SandboxPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--running-sandbox", proxy.Route{
		IP:              "10.0.0.5",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	// Create filter with mock callbacks and sandbox policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with sandbox-id
	header := newMockRequestHeaderMap()
	header.Set(headerSandboxID, "default--running-sandbox")

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - should continue and set upstream host
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.5:80", metadata["host"])
}

// TestDecodeHeadersSandboxRunning_HostPolicy tests the successful case when sandbox is running (host policy)
func TestDecodeHeadersSandboxRunning_HostPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--running-sandbox", proxy.Route{
		IP:              "10.0.0.5",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	// Create filter with mock callbacks and host policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with host in format: port-namespace--name.domain
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-default--running-sandbox.example.com"}

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - should continue and set upstream host
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly with port from host
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.5:8080", metadata["host"])
}

// TestDecodeHeadersWithCustomPort_SandboxPolicy tests the case when a custom port is specified (sandbox policy)
func TestDecodeHeadersWithCustomPort_SandboxPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--port-sandbox", proxy.Route{
		IP:              "10.0.0.6",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	// Create filter with mock callbacks and sandbox policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		HeaderSandboxPort: "e2b-sandbox-port",
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with sandbox-id and custom port
	header := newMockRequestHeaderMap()
	header.Set(headerSandboxID, "default--port-sandbox")
	header.Set(headerSandboxPort, "8080")

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - should continue and set upstream host with custom port
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly with custom port
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.6:8080", metadata["host"])
}

// TestDecodeHeadersWithCustomPort_HostPolicy tests the case when a custom port is specified via host header (host policy)
func TestDecodeHeadersWithCustomPort_HostPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--port-sandbox", proxy.Route{
		IP:              "10.0.0.6",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	// Create filter with mock callbacks and host policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with host in format: port-namespace--name.domain (using port 9090)
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "9090-default--port-sandbox.example.com"}

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - should continue and set upstream host with port from host header
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly with custom port from host
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.6:9090", metadata["host"])
}

// TestDecodeHeadersWithIPv6_SandboxPolicy tests the case when sandbox has IPv6 address (sandbox policy)
func TestDecodeHeadersWithIPv6_SandboxPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--ipv6-sandbox", proxy.Route{
		IP:              "2001:db8::1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	// Create filter with mock callbacks and sandbox policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with sandbox-id
	header := newMockRequestHeaderMap()
	header.Set(headerSandboxID, "default--ipv6-sandbox")

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "2001:db8::1:80", metadata["host"])
}

// TestDecodeHeadersWithIPv6_HostPolicy tests the case when sandbox has IPv6 address (host policy)
func TestDecodeHeadersWithIPv6_HostPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--ipv6-sandbox", proxy.Route{
		IP:              "2001:db8::1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	// Create filter with mock callbacks and host policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with host in format: port-namespace--name.domain
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-default--ipv6-sandbox.example.com"}

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "2001:db8::1:8080", metadata["host"])
}

// TestDecodeHeadersEmptySandboxID_SandboxPolicy tests the case when sandbox-id header is empty string (sandbox policy)
func TestDecodeHeadersEmptySandboxID_SandboxPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})

	// Create filter with mock callbacks and sandbox policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with empty sandbox-id
	header := newMockRequestHeaderMap()
	header.Set(headerSandboxID, "")

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - should continue without any side effects (empty string is treated as missing)
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

// TestDecodeHeadersInvalidHostFormat_HostPolicy tests the case when host header has invalid format (host policy)
// When ExtractHostInfo fails to parse, it returns empty sandboxID which results in 404
func TestDecodeHeadersInvalidHostFormat_HostPolicy(t *testing.T) {
	// Setup
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})

	// Create filter with mock callbacks and host policy
	mockCallbacks := newMockFilterCallbackHandler()
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}

	// Create header map with invalid host format (no port prefix)
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "invalid-host-format.example.com"}

	// Execute
	status := filter.DecodeHeaders(header, true)

	// Verify - when parsing fails, sandboxID is empty and returns 404
	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks.decoderCallbacks.replyStatusCode)
}

// TestDecodeHeadersRegistryInteraction tests the registry Get behavior
func TestDecodeHeadersRegistryInteraction(t *testing.T) {
	// When sandbox-id header is missing, the filter should return Continue (pass-through).
	// We can't easily test the full Envoy filter interface without the Envoy runtime,
	// but we can test the registry logic that the filter depends on.
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

// TestFilterFactory tests the FilterFactory function with sandbox policy
func TestFilterFactory_SandboxPolicy(t *testing.T) {
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		DefaultPort:       "80",
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := FilterFactory(cfg, mockCallbacks)

	// Verify the returned filter is a sandboxFilter
	sf, ok := filter.(*sandboxFilter)
	assert.True(t, ok)
	assert.Equal(t, HeaderMatchPolicySandbox, sf.config.HeaderMatchPolicy)
}

// TestFilterFactory_HostPolicy tests the FilterFactory function with host policy
func TestFilterFactory_HostPolicy(t *testing.T) {
	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := FilterFactory(cfg, mockCallbacks)

	// Verify the returned filter is a sandboxFilter
	sf, ok := filter.(*sandboxFilter)
	assert.True(t, ok)
	assert.Equal(t, HeaderMatchPolicyHost, sf.config.HeaderMatchPolicy)
}

// TestDecodeHeadersMultipleRequests_SandboxPolicy tests handling multiple sequential requests (sandbox policy)
func TestDecodeHeadersMultipleRequests_SandboxPolicy(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	// Setup multiple sandboxes
	r.Update("ns1--sandbox1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})
	r.Update("ns2--sandbox2", proxy.Route{IP: "10.0.0.2", State: agentsv1alpha1.SandboxStateCreating, ResourceVersion: "1"})

	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		DefaultPort:       "80",
	}

	// First request - running sandbox
	mockCallbacks1 := newMockFilterCallbackHandler()
	filter1 := &sandboxFilter{callbacks: mockCallbacks1, config: cfg}
	header1 := newMockRequestHeaderMap()
	header1.Set(headerSandboxID, "ns1--sandbox1")

	status1 := filter1.DecodeHeaders(header1, true)
	assert.Equal(t, api.Continue, status1)
	assert.False(t, mockCallbacks1.decoderCallbacks.sendLocalReplyCalled)

	// Second request - non-running sandbox
	mockCallbacks2 := newMockFilterCallbackHandler()
	filter2 := &sandboxFilter{callbacks: mockCallbacks2, config: cfg}
	header2 := newMockRequestHeaderMap()
	header2.Set(headerSandboxID, "ns2--sandbox2")

	status2 := filter2.DecodeHeaders(header2, true)
	assert.Equal(t, api.LocalReply, status2)
	assert.True(t, mockCallbacks2.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks2.decoderCallbacks.replyStatusCode)

	// Third request - non-existent sandbox
	mockCallbacks3 := newMockFilterCallbackHandler()
	filter3 := &sandboxFilter{callbacks: mockCallbacks3, config: cfg}
	header3 := newMockRequestHeaderMap()
	header3.Set(headerSandboxID, "ns3--nonexistent")

	status3 := filter3.DecodeHeaders(header3, true)
	assert.Equal(t, api.LocalReply, status3)
	assert.True(t, mockCallbacks3.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks3.decoderCallbacks.replyStatusCode)
}

// TestDecodeHeadersMultipleRequests_HostPolicy tests handling multiple sequential requests (host policy)
func TestDecodeHeadersMultipleRequests_HostPolicy(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	// Setup multiple sandboxes
	r.Update("ns1--sandbox1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})
	r.Update("ns2--sandbox2", proxy.Route{IP: "10.0.0.2", State: agentsv1alpha1.SandboxStateCreating, ResourceVersion: "1"})

	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}

	// First request - running sandbox
	mockCallbacks1 := newMockFilterCallbackHandler()
	filter1 := &sandboxFilter{callbacks: mockCallbacks1, config: cfg}
	header1 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns1--sandbox1.example.com"}

	status1 := filter1.DecodeHeaders(header1, true)
	assert.Equal(t, api.Continue, status1)
	assert.False(t, mockCallbacks1.decoderCallbacks.sendLocalReplyCalled)

	// Second request - non-running sandbox
	mockCallbacks2 := newMockFilterCallbackHandler()
	filter2 := &sandboxFilter{callbacks: mockCallbacks2, config: cfg}
	header2 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns2--sandbox2.example.com"}

	status2 := filter2.DecodeHeaders(header2, true)
	assert.Equal(t, api.LocalReply, status2)
	assert.True(t, mockCallbacks2.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks2.decoderCallbacks.replyStatusCode)

	// Third request - non-existent sandbox
	mockCallbacks3 := newMockFilterCallbackHandler()
	filter3 := &sandboxFilter{callbacks: mockCallbacks3, config: cfg}
	header3 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns3--nonexistent.example.com"}

	status3 := filter3.DecodeHeaders(header3, true)
	assert.Equal(t, api.LocalReply, status3)
	assert.True(t, mockCallbacks3.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 404, mockCallbacks3.decoderCallbacks.replyStatusCode)
}

// TestDecodeHeadersEndStreamFalse_SandboxPolicy tests that endStream parameter doesn't affect the logic (sandbox policy)
func TestDecodeHeadersEndStreamFalse_SandboxPolicy(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--test-sandbox", proxy.Route{
		IP:              "10.0.0.1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicySandbox,
		HeaderMatchName:   "e2b-sandbox-id",
		DefaultPort:       "80",
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}
	header := newMockRequestHeaderMap()
	header.Set(headerSandboxID, "default--test-sandbox")

	// Execute with endStream=false
	status := filter.DecodeHeaders(header, false)

	// Should still work correctly
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

// TestDecodeHeadersEndStreamFalse_HostPolicy tests that endStream parameter doesn't affect the logic (host policy)
func TestDecodeHeadersEndStreamFalse_HostPolicy(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--test-sandbox", proxy.Route{
		IP:              "10.0.0.1",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := &Config{
		HeaderMatchPolicy: HeaderMatchPolicyHost,
		DefaultPort:       "80",
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg}
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-default--test-sandbox.example.com"}

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
