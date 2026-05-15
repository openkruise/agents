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
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/envoyproxy/envoy/contrib/golang/common/go/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-gateway/wake"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
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

// mockDynamicMetadata implements api.DynamicMetadata for testing
type mockDynamicMetadata struct {
	mu   sync.Mutex
	data map[string]map[string]interface{}
}

func newMockDynamicMetadata() *mockDynamicMetadata {
	return &mockDynamicMetadata{data: make(map[string]map[string]interface{})}
}

func (m *mockDynamicMetadata) Get(filterName string) map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.data[filterName]
}

func (m *mockDynamicMetadata) Set(filterName string, key string, value interface{}) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[filterName] == nil {
		m.data[filterName] = make(map[string]interface{})
	}
	m.data[filterName][key] = value
}

func (m *mockDynamicMetadata) Value(filterName string, key string) interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.data[filterName] == nil {
		return nil
	}
	return m.data[filterName][key]
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
	mu                   sync.Mutex
	sendLocalReplyCalled bool
	replyStatusCode      int
	replyBody            string
	replyDetails         string
	replyHeaders         map[string][]string
	continueCalled       bool
	continueStatus       api.StatusType
}

func (m *mockDecoderFilterCallbacks) Continue(statusType api.StatusType) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.continueCalled = true
	m.continueStatus = statusType
}

func (m *mockDecoderFilterCallbacks) SendLocalReply(responseCode int, bodyText string, headers map[string][]string, _ int64, details string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sendLocalReplyCalled = true
	m.replyStatusCode = responseCode
	m.replyBody = bodyText
	m.replyDetails = details
	m.replyHeaders = headers
}

func (m *mockDecoderFilterCallbacks) HasContinued() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.continueCalled
}

func (m *mockDecoderFilterCallbacks) HasLocalReply() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sendLocalReplyCalled
}

func (m *mockDecoderFilterCallbacks) Snapshot() (bool, int, string, string, map[string][]string, bool, api.StatusType) {
	m.mu.Lock()
	defer m.mu.Unlock()
	headers := make(map[string][]string, len(m.replyHeaders))
	for k, values := range m.replyHeaders {
		headers[k] = append([]string(nil), values...)
	}
	return m.sendLocalReplyCalled, m.replyStatusCode, m.replyBody, m.replyDetails, headers, m.continueCalled, m.continueStatus
}

func (m *mockDecoderFilterCallbacks) RecoverPanic() {}

func (m *mockDecoderFilterCallbacks) AddData([]byte, bool) {}

func (m *mockDecoderFilterCallbacks) InjectData([]byte) {}

func (m *mockDecoderFilterCallbacks) SetUpstreamOverrideHost(string, bool) error {
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

func (m *mockFilterCallbackHandler) Log(api.LogType, string) {}

func (m *mockFilterCallbackHandler) LogLevel() api.LogType { return api.Info }

func (m *mockFilterCallbackHandler) GetProperty(string) (string, error) {
	return "", nil
}

func (m *mockFilterCallbackHandler) SecretManager() api.SecretManager { return nil }

func (m *mockFilterCallbackHandler) DecoderFilterCallbacks() api.DecoderFilterCallbacks {
	return m.decoderCallbacks
}

func (m *mockFilterCallbackHandler) EncoderFilterCallbacks() api.EncoderFilterCallbacks {
	return nil
}

type fakeWakeModule struct {
	err        error
	panic      bool
	calls      atomic.Int32
	onCall     func(ctx context.Context, sandboxID string) error
	called     chan struct{}
	calledOnce sync.Once
	release    chan struct{}
}

func (m *fakeWakeModule) WakeAndWait(ctx context.Context, sandboxID string) error {
	m.calls.Add(1)
	if m.called != nil {
		m.calledOnce.Do(func() {
			close(m.called)
		})
	}
	if m.panic {
		panic("wake panic")
	}
	if m.onCall != nil {
		return m.onCall(ctx, sandboxID)
	}
	if m.release != nil {
		select {
		case <-m.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return m.err
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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

	// Create header map with sandbox-id that doesn't exist
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, "nonexistent-sandbox")

	status := filter.DecodeHeaders(header, true)

	// Verify - should return LocalReply with 502
	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks.decoderCallbacks.replyStatusCode)
	assert.Contains(t, mockCallbacks.decoderCallbacks.replyBody, "nonexistent-sandbox")
	assert.Equal(t, "sandbox_not_found", mockCallbacks.decoderCallbacks.replyDetails)
}

// TestDecodeHeadersSandboxNotFoundHostFallback tests sandbox not found via host header
func TestDecodeHeadersSandboxNotFoundHostFallback(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

	// Create header map with host in format: port-namespace--name.domain
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-nonexistent--sandbox.example.com"}

	status := filter.DecodeHeaders(header, true)

	// Verify - should return LocalReply with 502
	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks.decoderCallbacks.replyStatusCode)
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

			cfg := DefaultConfig()
			mockCallbacks := newMockFilterCallbackHandler()
			filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

			cfg := DefaultConfig()
			mockCallbacks := newMockFilterCallbackHandler()
			filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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
	assert.Equal(t, "[2001:db8::1]:49983", metadata["host"])
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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

	// Create header map with host in format: port-namespace--name.domain
	header := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-default--ipv6-sandbox.example.com"}

	status := filter.DecodeHeaders(header, true)

	// Verify
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set correctly
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "[2001:db8::1]:8080", metadata["host"])
}

// TestDecodeHeadersEmptySandboxID tests the case when sandbox-id header is empty string
func TestDecodeHeadersEmptySandboxID(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("default--app1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

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
	cfg := NewFilterConfig(DefaultConfig())
	mockCallbacks := newMockFilterCallbackHandler()
	filter := FilterFactory(cfg, mockCallbacks)

	// Verify the returned filter is a sandboxFilter
	sf, ok := filter.(*sandboxFilter)
	assert.True(t, ok)
	assert.Equal(t, DefaultSandboxHeaderName, sf.config.SandboxHeaderName)
	assert.NotNil(t, sf.adapter)
}

// TestDecodeHeadersMultipleRequests tests handling multiple sequential requests
func TestDecodeHeadersMultipleRequests(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	// Setup multiple sandboxes
	r.Update("ns1--sandbox1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})
	r.Update("ns2--sandbox2", proxy.Route{IP: "10.0.0.2", State: agentsv1alpha1.SandboxStateCreating, ResourceVersion: "1"})

	cfg := DefaultConfig()

	// First request - running sandbox via sandbox header
	mockCallbacks1 := newMockFilterCallbackHandler()
	filter1 := &sandboxFilter{callbacks: mockCallbacks1, config: cfg, adapter: defaultTestAdapter()}
	header1 := newMockRequestHeaderMap()
	header1.Set(DefaultSandboxHeaderName, "ns1--sandbox1")

	status1 := filter1.DecodeHeaders(header1, true)
	assert.Equal(t, api.Continue, status1)
	assert.False(t, mockCallbacks1.decoderCallbacks.sendLocalReplyCalled)

	// Second request - non-running sandbox via sandbox header
	mockCallbacks2 := newMockFilterCallbackHandler()
	filter2 := &sandboxFilter{callbacks: mockCallbacks2, config: cfg, adapter: defaultTestAdapter()}
	header2 := newMockRequestHeaderMap()
	header2.Set(DefaultSandboxHeaderName, "ns2--sandbox2")

	status2 := filter2.DecodeHeaders(header2, true)
	assert.Equal(t, api.LocalReply, status2)
	assert.True(t, mockCallbacks2.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks2.decoderCallbacks.replyStatusCode)

	// Third request - non-existent sandbox via sandbox header
	mockCallbacks3 := newMockFilterCallbackHandler()
	filter3 := &sandboxFilter{callbacks: mockCallbacks3, config: cfg, adapter: defaultTestAdapter()}
	header3 := newMockRequestHeaderMap()
	header3.Set(DefaultSandboxHeaderName, "ns3--nonexistent")

	status3 := filter3.DecodeHeaders(header3, true)
	assert.Equal(t, api.LocalReply, status3)
	assert.True(t, mockCallbacks3.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks3.decoderCallbacks.replyStatusCode)
}

// TestDecodeHeadersMultipleRequestsHostFallback tests multiple requests via host header
func TestDecodeHeadersMultipleRequestsHostFallback(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	// Setup multiple sandboxes
	r.Update("ns1--sandbox1", proxy.Route{IP: "10.0.0.1", State: agentsv1alpha1.SandboxStateRunning, ResourceVersion: "1"})
	r.Update("ns2--sandbox2", proxy.Route{IP: "10.0.0.2", State: agentsv1alpha1.SandboxStateCreating, ResourceVersion: "1"})

	cfg := DefaultConfig()

	// First request - running sandbox via host header
	mockCallbacks1 := newMockFilterCallbackHandler()
	filter1 := &sandboxFilter{callbacks: mockCallbacks1, config: cfg, adapter: defaultTestAdapter()}
	header1 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns1--sandbox1.example.com"}

	status1 := filter1.DecodeHeaders(header1, true)
	assert.Equal(t, api.Continue, status1)
	assert.False(t, mockCallbacks1.decoderCallbacks.sendLocalReplyCalled)

	// Second request - non-running sandbox via host header
	mockCallbacks2 := newMockFilterCallbackHandler()
	filter2 := &sandboxFilter{callbacks: mockCallbacks2, config: cfg, adapter: defaultTestAdapter()}
	header2 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns2--sandbox2.example.com"}

	status2 := filter2.DecodeHeaders(header2, true)
	assert.Equal(t, api.LocalReply, status2)
	assert.True(t, mockCallbacks2.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks2.decoderCallbacks.replyStatusCode)

	// Third request - non-existent sandbox via host header
	mockCallbacks3 := newMockFilterCallbackHandler()
	filter3 := &sandboxFilter{callbacks: mockCallbacks3, config: cfg, adapter: defaultTestAdapter()}
	header3 := &mockRequestHeaderMapWithHost{mockRequestHeaderMap: *newMockRequestHeaderMap(), hostValue: "8080-ns3--nonexistent.example.com"}

	status3 := filter3.DecodeHeaders(header3, true)
	assert.Equal(t, api.LocalReply, status3)
	assert.True(t, mockCallbacks3.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks3.decoderCallbacks.replyStatusCode)
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

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, "default--test-sandbox")

	// Execute with endStream=false
	status := filter.DecodeHeaders(header, false)

	// Should still work correctly
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

// TestDecodeHeadersKruiseCustomProtocol tests kruise custom protocol routing via path-based adapter
func TestDecodeHeadersKruiseCustomProtocol(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	r.Update("ns--mysandbox", proxy.Route{
		IP:              "10.0.0.10",
		State:           agentsv1alpha1.SandboxStateRunning,
		ResourceVersion: "1",
	})

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

	// Create header with kruise custom protocol path
	header := &mockRequestHeaderMapCustom{
		mockRequestHeaderMap: *newMockRequestHeaderMap(),
		pathValue:            "/kruise/ns--mysandbox/3000/api/v1/data",
	}

	status := filter.DecodeHeaders(header, true)

	// Verify - should route to sandbox with :path rewritten
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)

	// Verify dynamic metadata was set with correct port
	metadata := mockCallbacks.streamInfo.dynamicMetadata.data["envoy.lb.original_dst"]
	assert.NotNil(t, metadata)
	assert.Equal(t, "10.0.0.10:3000", metadata["host"])

	// Verify :path was rewritten by the adapter
	assert.Equal(t, "/api/v1/data", header.headers[":path"])
}

// TestDecodeHeadersKruiseCustomProtocolNotFound tests kruise routing when sandbox not in registry
func TestDecodeHeadersKruiseCustomProtocolNotFound(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

	header := &mockRequestHeaderMapCustom{
		mockRequestHeaderMap: *newMockRequestHeaderMap(),
		pathValue:            "/kruise/nonexistent--sandbox/3000/api/data",
	}

	status := filter.DecodeHeaders(header, true)

	assert.Equal(t, api.LocalReply, status)
	assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
	assert.Equal(t, 502, mockCallbacks.decoderCallbacks.replyStatusCode)
}

// TestDecodeHeadersKruiseCustomProtocolInvalidPath tests kruise routing with invalid path
func TestDecodeHeadersKruiseCustomProtocolInvalidPath(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()

	cfg := DefaultConfig()
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: cfg, adapter: defaultTestAdapter()}

	// Invalid kruise path (missing port segment)
	header := &mockRequestHeaderMapCustom{
		mockRequestHeaderMap: *newMockRequestHeaderMap(),
		pathValue:            "/kruise/sandbox1234",
	}

	status := filter.DecodeHeaders(header, true)

	// Should pass through since adapter returns error
	assert.Equal(t, api.Continue, status)
	assert.False(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
}

func TestDecodeHeadersWakeGateMatrix(t *testing.T) {
	tests := []struct {
		name          string
		state         string
		pausable      bool
		wakeOnTraffic string
		wantStatus    api.StatusType
		wantWakeCall  bool
	}{
		{
			name:          "paused pausable with wake policy starts async wake",
			state:         agentsv1alpha1.SandboxStatePaused,
			pausable:      true,
			wakeOnTraffic: "timeout:5m",
			wantStatus:    api.Running,
			wantWakeCall:  true,
		},
		{
			name:          "paused but not pausable stays local reply",
			state:         agentsv1alpha1.SandboxStatePaused,
			pausable:      false,
			wakeOnTraffic: "timeout:5m",
			wantStatus:    api.LocalReply,
		},
		{
			name:         "paused without wake policy stays local reply",
			state:        agentsv1alpha1.SandboxStatePaused,
			pausable:     true,
			wantStatus:   api.LocalReply,
			wantWakeCall: false,
		},
		{
			name:          "non-paused state stays local reply",
			state:         agentsv1alpha1.SandboxStateCreating,
			pausable:      true,
			wakeOnTraffic: "timeout:5m",
			wantStatus:    api.LocalReply,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := registry.GetRegistry()
			defer r.Clear()
			sandboxID := "default--wake-gate"
			r.Update(sandboxID, proxy.Route{
				IP:              "10.0.0.9",
				State:           tt.state,
				ResourceVersion: "1",
				Pausable:        tt.pausable,
				WakeOnTraffic:   tt.wakeOnTraffic,
			})

			wakeClient := &fakeWakeModule{
				onCall: func(ctx context.Context, sandboxID string) error {
					r.Update(sandboxID, proxy.Route{
						IP:              "10.0.0.10",
						State:           agentsv1alpha1.SandboxStateRunning,
						ResourceVersion: "2",
					})
					return nil
				},
			}
			mockCallbacks := newMockFilterCallbackHandler()
			filter := &sandboxFilter{callbacks: mockCallbacks, config: DefaultConfig(), adapter: defaultTestAdapter(), wake: wakeClient}
			header := newMockRequestHeaderMap()
			header.Set(DefaultSandboxHeaderName, sandboxID)

			status := filter.DecodeHeaders(header, true)
			assert.Equal(t, tt.wantStatus, status)

			if tt.wantWakeCall {
				require.Eventually(t, func() bool {
					return mockCallbacks.decoderCallbacks.HasContinued()
				}, time.Second, 10*time.Millisecond)
				assert.Equal(t, int32(1), wakeClient.calls.Load())
				sendLocalReplyCalled, _, _, _, _, _, _ := mockCallbacks.decoderCallbacks.Snapshot()
				assert.False(t, sendLocalReplyCalled)
				assert.Equal(t, "10.0.0.10:49983", mockCallbacks.streamInfo.dynamicMetadata.Value("envoy.lb.original_dst", "host"))
				return
			}

			assert.True(t, mockCallbacks.decoderCallbacks.sendLocalReplyCalled)
			assert.Equal(t, 502, mockCallbacks.decoderCallbacks.replyStatusCode)
			assert.Equal(t, "sandbox_not_running", mockCallbacks.decoderCallbacks.replyDetails)
			assert.Equal(t, int32(0), wakeClient.calls.Load())
		})
	}
}

func TestDecodeHeadersWakeErrorReplies(t *testing.T) {
	tests := []struct {
		name           string
		err            error
		wantStatus     int
		wantRetryAfter string
		wantDetails    string
	}{
		{
			name:        "auto resume disabled",
			err:         wake.ErrAutoResumeDisabled,
			wantStatus:  http.StatusBadGateway,
			wantDetails: "sandbox_not_running",
		},
		{
			name:           "invalid policy",
			err:            wake.ErrInvalidPolicy,
			wantStatus:     http.StatusServiceUnavailable,
			wantRetryAfter: "0",
			wantDetails:    "sandbox_wake_invalid_policy",
		},
		{
			name:           "pausing",
			err:            wake.ErrPausing,
			wantStatus:     http.StatusServiceUnavailable,
			wantRetryAfter: "5",
			wantDetails:    "sandbox_wake_pausing",
		},
		{
			name:           "bad state",
			err:            wake.ErrBadState,
			wantStatus:     http.StatusServiceUnavailable,
			wantRetryAfter: "15",
			wantDetails:    "sandbox_wake_bad_state",
		},
		{
			name:        "gone",
			err:         wake.ErrGone,
			wantStatus:  http.StatusBadGateway,
			wantDetails: "sandbox_not_found",
		},
		{
			name:           "transport",
			err:            wake.ErrTransport,
			wantStatus:     http.StatusServiceUnavailable,
			wantRetryAfter: "5",
			wantDetails:    "sandbox_wake_failed",
		},
		{
			name:           "unknown error",
			err:            errors.New("boom"),
			wantStatus:     http.StatusServiceUnavailable,
			wantRetryAfter: "5",
			wantDetails:    "sandbox_wake_failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := registry.GetRegistry()
			defer r.Clear()
			sandboxID := "default--wake-error"
			r.Update(sandboxID, proxy.Route{
				IP:              "10.0.0.9",
				State:           agentsv1alpha1.SandboxStatePaused,
				ResourceVersion: "1",
				Pausable:        true,
				WakeOnTraffic:   "timeout:5m",
			})

			mockCallbacks := newMockFilterCallbackHandler()
			filter := &sandboxFilter{
				callbacks: mockCallbacks,
				config:    DefaultConfig(),
				adapter:   defaultTestAdapter(),
				wake:      &fakeWakeModule{err: tt.err},
			}
			header := newMockRequestHeaderMap()
			header.Set(DefaultSandboxHeaderName, sandboxID)

			status := filter.DecodeHeaders(header, true)
			require.Equal(t, api.Running, status)
			require.Eventually(t, func() bool {
				return mockCallbacks.decoderCallbacks.HasLocalReply()
			}, time.Second, 10*time.Millisecond)
			_, replyStatusCode, _, replyDetails, replyHeaders, _, _ := mockCallbacks.decoderCallbacks.Snapshot()
			assert.Equal(t, tt.wantStatus, replyStatusCode)
			assert.Equal(t, tt.wantDetails, replyDetails)
			if tt.wantRetryAfter == "" {
				assert.Empty(t, replyHeaders)
				return
			}
			assert.Equal(t, []string{tt.wantRetryAfter}, replyHeaders["Retry-After"])
		})
	}
}

func TestDecodeHeadersWakeOnDestroyCancelsWithoutReply(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	sandboxID := "default--wake-destroy"
	r.Update(sandboxID, proxy.Route{
		IP:              "10.0.0.9",
		State:           agentsv1alpha1.SandboxStatePaused,
		ResourceVersion: "1",
		Pausable:        true,
		WakeOnTraffic:   "timeout:5m",
	})

	called := make(chan struct{})
	wakeClient := &fakeWakeModule{
		called: called,
		onCall: func(ctx context.Context, sandboxID string) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: DefaultConfig(), adapter: defaultTestAdapter(), wake: wakeClient}
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, sandboxID)

	status := filter.DecodeHeaders(header, true)
	require.Equal(t, api.Running, status)
	<-called

	require.Never(t, func() bool {
		return mockCallbacks.decoderCallbacks.HasContinued() || mockCallbacks.decoderCallbacks.HasLocalReply()
	}, 150*time.Millisecond, 10*time.Millisecond)

	filter.OnDestroy(api.Terminate)
	require.Never(t, func() bool {
		return mockCallbacks.decoderCallbacks.HasContinued() || mockCallbacks.decoderCallbacks.HasLocalReply()
	}, 150*time.Millisecond, 10*time.Millisecond)
}

func TestDecodeHeadersWakePanicRecovered(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	sandboxID := "default--wake-panic"
	r.Update(sandboxID, proxy.Route{
		IP:              "10.0.0.9",
		State:           agentsv1alpha1.SandboxStatePaused,
		ResourceVersion: "1",
		Pausable:        true,
		WakeOnTraffic:   "timeout:5m",
	})

	var wakeCtx context.Context
	wakeClient := &fakeWakeModule{
		onCall: func(ctx context.Context, sandboxID string) error {
			wakeCtx = ctx
			panic("wake panic")
		},
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: DefaultConfig(), adapter: defaultTestAdapter(), wake: wakeClient}
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, sandboxID)

	status := filter.DecodeHeaders(header, true)
	require.Equal(t, api.Running, status)
	require.Eventually(t, func() bool {
		return mockCallbacks.decoderCallbacks.HasLocalReply()
	}, time.Second, 10*time.Millisecond)
	_, replyStatusCode, _, replyDetails, _, _, _ := mockCallbacks.decoderCallbacks.Snapshot()
	assert.Equal(t, 500, replyStatusCode)
	assert.Equal(t, "sandbox_wake_panic", replyDetails)
	require.NotNil(t, wakeCtx)
	assert.ErrorIs(t, wakeCtx.Err(), context.Canceled)
}

func TestDecodeHeadersWakeContinuationWithIPv6(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	sandboxID := "default--wake-ipv6"
	r.Update(sandboxID, proxy.Route{
		IP:              "2001:db8::1",
		State:           agentsv1alpha1.SandboxStatePaused,
		ResourceVersion: "1",
		Pausable:        true,
		WakeOnTraffic:   "timeout:5m",
	})

	wakeClient := &fakeWakeModule{
		onCall: func(ctx context.Context, sandboxID string) error {
			r.Update(sandboxID, proxy.Route{
				IP:              "2001:db8::2",
				State:           agentsv1alpha1.SandboxStateRunning,
				ResourceVersion: "2",
			})
			return nil
		},
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: DefaultConfig(), adapter: defaultTestAdapter(), wake: wakeClient}
	header := newMockRequestHeaderMap()
	header.Set(DefaultSandboxHeaderName, sandboxID)
	header.Set(DefaultSandboxPortHeader, "8080")

	status := filter.DecodeHeaders(header, true)
	require.Equal(t, api.Running, status)
	require.Eventually(t, func() bool {
		return mockCallbacks.decoderCallbacks.HasContinued()
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, "[2001:db8::2]:8080", mockCallbacks.streamInfo.dynamicMetadata.Value("envoy.lb.original_dst", "host"))
}

func TestDecodeHeadersWakeCustomProtocolAppliesPathRewriteBeforeAsyncWait(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	sandboxID := "ns--wake-path"
	r.Update(sandboxID, proxy.Route{
		IP:              "10.0.0.9",
		State:           agentsv1alpha1.SandboxStatePaused,
		ResourceVersion: "1",
		Pausable:        true,
		WakeOnTraffic:   "timeout:5m",
	})

	called := make(chan struct{})
	wakeClient := &fakeWakeModule{
		called: called,
		onCall: func(ctx context.Context, sandboxID string) error {
			r.Update(sandboxID, proxy.Route{
				IP:              "10.0.0.21",
				State:           agentsv1alpha1.SandboxStateRunning,
				ResourceVersion: "2",
			})
			return nil
		},
	}
	mockCallbacks := newMockFilterCallbackHandler()
	filter := &sandboxFilter{callbacks: mockCallbacks, config: DefaultConfig(), adapter: defaultTestAdapter(), wake: wakeClient}
	header := &mockRequestHeaderMapCustom{
		mockRequestHeaderMap: *newMockRequestHeaderMap(),
		pathValue:            "/kruise/ns--wake-path/3000/api/v1/data",
	}

	status := filter.DecodeHeaders(header, true)
	require.Equal(t, api.Running, status)
	assert.Equal(t, "/api/v1/data", header.headers[":path"])
	<-called
	require.Eventually(t, func() bool {
		return mockCallbacks.decoderCallbacks.HasContinued()
	}, time.Second, 10*time.Millisecond)
	assert.Equal(t, "10.0.0.21:3000", mockCallbacks.streamInfo.dynamicMetadata.Value("envoy.lb.original_dst", "host"))
}

func TestDecodeHeadersWakeSingleflightContinuesBothStreams(t *testing.T) {
	r := registry.GetRegistry()
	defer r.Clear()
	sandboxID := "default--wake-shared"
	r.Update(sandboxID, proxy.Route{
		IP:              "10.0.0.9",
		State:           agentsv1alpha1.SandboxStatePaused,
		ResourceVersion: "1",
		Pausable:        true,
		WakeOnTraffic:   "timeout:5m",
	})

	var posts atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		<-release
		registry.GetRegistry().Update(sandboxID, proxy.Route{
			IP:              "10.0.0.20",
			State:           agentsv1alpha1.SandboxStateRunning,
			ResourceVersion: "2",
		})
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	wakeClient := wake.NewModuleWithClient(server.URL, server.Client())
	mockCallbacks1 := newMockFilterCallbackHandler()
	mockCallbacks2 := newMockFilterCallbackHandler()
	filter1 := &sandboxFilter{callbacks: mockCallbacks1, config: DefaultConfig(), adapter: defaultTestAdapter(), wake: wakeClient}
	filter2 := &sandboxFilter{callbacks: mockCallbacks2, config: DefaultConfig(), adapter: defaultTestAdapter(), wake: wakeClient}
	header1 := newMockRequestHeaderMap()
	header1.Set(DefaultSandboxHeaderName, sandboxID)
	header2 := newMockRequestHeaderMap()
	header2.Set(DefaultSandboxHeaderName, sandboxID)

	status1 := filter1.DecodeHeaders(header1, true)
	status2 := filter2.DecodeHeaders(header2, true)
	require.Equal(t, api.Running, status1)
	require.Equal(t, api.Running, status2)
	require.Eventually(t, func() bool {
		return posts.Load() == 1
	}, time.Second, 10*time.Millisecond)
	require.Never(t, func() bool {
		return posts.Load() > 1
	}, 50*time.Millisecond, 10*time.Millisecond)
	close(release)

	require.Eventually(t, func() bool {
		return mockCallbacks1.decoderCallbacks.HasContinued() && mockCallbacks2.decoderCallbacks.HasContinued()
	}, time.Second, 10*time.Millisecond)
	sendLocalReplyCalled1, _, _, _, _, _, _ := mockCallbacks1.decoderCallbacks.Snapshot()
	sendLocalReplyCalled2, _, _, _, _, _, _ := mockCallbacks2.decoderCallbacks.Snapshot()
	assert.False(t, sendLocalReplyCalled1)
	assert.False(t, sendLocalReplyCalled2)
	assert.Equal(t, "10.0.0.20:49983", mockCallbacks1.streamInfo.dynamicMetadata.Value("envoy.lb.original_dst", "host"))
	assert.Equal(t, "10.0.0.20:49983", mockCallbacks2.streamInfo.dynamicMetadata.Value("envoy.lb.original_dst", "host"))
}
