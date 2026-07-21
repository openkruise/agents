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
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/metrics"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// mockPeers is a simple in-memory Peers implementation for testing
type mockPeers struct {
	mu      sync.RWMutex
	members []peers.Peer
}

func newMockPeers(members ...peers.Peer) *mockPeers {
	return &mockPeers{members: members}
}

func (m *mockPeers) Start(_ context.Context, _ int) error { return nil }
func (m *mockPeers) Stop() error                          { return nil }
func (m *mockPeers) GetPeers() []peers.Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]peers.Peer, len(m.members))
	copy(result, m.members)
	return result
}
func (m *mockPeers) GetAllMembers() []peers.Peer                 { return m.GetPeers() }
func (m *mockPeers) WaitForPeers(_ context.Context, _ int) error { return nil }
func (m *mockPeers) LocalAddr() net.IP                           { return net.ParseIP("127.0.0.1") }
func (m *mockPeers) LocalPort() int                              { return 0 }

func (m *mockPeers) SetMembers(members ...peers.Peer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.members = members
}

// newTestServer creates a Server instance for testing (no HTTP/gRPC started)
func newTestServer(pm peers.Peers) *Server {
	server := NewServer(config.SandboxManagerOptions{})
	server.SetPeersManager(pm)
	return server
}

func testProxyRoute(id, ip, resourceVersion string) sandboxroute.Route {
	return sandboxroute.Route{
		ID:              id,
		IP:              ip,
		UID:             types.UID("uid-" + id),
		ResourceVersion: resourceVersion,
	}
}

// ---- SetRoute tests ----

func TestSetRoute_FirstWrite(t *testing.T) {
	s := newTestServer(nil)
	route := testProxyRoute("sb-1", "1.2.3.4", "1")

	s.SetRoute(context.Background(), route)

	got, ok := s.LoadRoute("sb-1")
	require.True(t, ok)
	assert.Equal(t, route, got)
}

func TestSetRoute_NewerVersionOverwrites(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()
	old := testProxyRoute("sb-1", "1.2.3.4", "1")
	newer := testProxyRoute("sb-1", "5.6.7.8", "2")

	s.SetRoute(ctx, old)
	s.SetRoute(ctx, newer)

	got, _ := s.LoadRoute("sb-1")
	assert.Equal(t, "5.6.7.8", got.IP)
}

func TestSetRoute_OlderVersionSkipped(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()
	current := testProxyRoute("sb-1", "5.6.7.8", "5")
	older := testProxyRoute("sb-1", "1.1.1.1", "3")

	s.SetRoute(ctx, current)
	s.SetRoute(ctx, older)

	got, _ := s.LoadRoute("sb-1")
	assert.Equal(t, "5.6.7.8", got.IP, "older version should not overwrite current")
}

func TestSetRoute_ConcurrentWrites(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()
	const n = 100

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		rv := fmt.Sprintf("%d", i)
		ip := fmt.Sprintf("10.0.0.%d", i)
		go func() {
			defer wg.Done()
			s.SetRoute(ctx, testProxyRoute("sb-1", ip, rv))
		}()
	}
	wg.Wait()

	// After concurrent writes, route should exist and have a valid resource version
	got, ok := s.LoadRoute("sb-1")
	require.True(t, ok)
	assert.NotEmpty(t, got.IP)
}

func TestSetRouteValidationAndShapeDispatch(t *testing.T) {
	validIDOnly := testProxyRoute("id-only", "1.1.1.1", "1")
	validFull := testProxyRoute("full", "2.2.2.2", "1")
	validFull.Namespace = "ns"
	validFull.Name = "full"

	tests := []struct {
		name         string
		route        sandboxroute.Route
		expectResult sandboxroute.EventResult
		expectStored bool
	}{
		{name: "ID-only route", route: validIDOnly, expectResult: sandboxroute.EventResultApplied, expectStored: true},
		{name: "full route", route: validFull, expectResult: sandboxroute.EventResultApplied, expectStored: true},
		{name: "missing ID", route: sandboxroute.Route{UID: "uid", ResourceVersion: "1"}, expectResult: sandboxroute.EventResultInvalid},
		{name: "missing UID", route: sandboxroute.Route{ID: "id", ResourceVersion: "1"}, expectResult: sandboxroute.EventResultInvalid},
		{name: "missing resource version", route: sandboxroute.Route{ID: "id", UID: "uid"}, expectResult: sandboxroute.EventResultInvalid},
		{name: "partial ObjectKey", route: sandboxroute.Route{ID: "id", Namespace: "ns", UID: "uid", ResourceVersion: "1"}, expectResult: sandboxroute.EventResultInvalid},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(nil)
			enqueued := 0
			s.SetRepairEnqueuer(func(sandboxroute.MutationResult) { enqueued++ })

			result := s.SetRoute(t.Context(), tt.route)

			assert.Equal(t, tt.expectResult, result.Result)
			_, stored := s.LoadRoute(tt.route.ID)
			assert.Equal(t, tt.expectStored, stored)
			if tt.expectResult == sandboxroute.EventResultInvalid {
				assert.Equal(t, sandboxroute.ReasonInvalidRoute, result.Reason)
				assert.Zero(t, enqueued)
			}
		})
	}
}

func TestSetRouteInvalidMetric(t *testing.T) {
	tests := []struct {
		name  string
		route sandboxroute.Route
	}{
		{name: "decoded invalid route", route: sandboxroute.Route{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(nil)
			labels := routeInvalidLabels()
			before := proxyCounterValue(t, "sandbox_route_invalid_total", labels)

			result := s.SetRoute(t.Context(), tt.route)

			assert.Equal(t, sandboxroute.EventResultInvalid, result.Result)
			assert.Equal(t, before+1, proxyCounterValue(t, "sandbox_route_invalid_total", labels))
		})
	}
}

func proxyCounterValue(t *testing.T, name string, expectedLabels map[string]string) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics.RegisterSandboxRoute(registry)
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if proxyMetricLabelsMatch(metric, expectedLabels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func proxyMetricLabelsMatch(metric *dto.Metric, expected map[string]string) bool {
	if len(metric.Label) != len(expected) {
		return false
	}
	for _, label := range metric.Label {
		if expected[label.GetName()] != label.GetValue() {
			return false
		}
	}
	return true
}

func routeInvalidLabels() map[string]string {
	return map[string]string{}
}

// ---- LoadRoute tests ----

func TestLoadRoute_NotFound(t *testing.T) {
	s := newTestServer(nil)
	_, ok := s.LoadRoute("nonexistent")
	assert.False(t, ok)
}

func TestLoadRoute_Found(t *testing.T) {
	s := newTestServer(nil)
	route := testProxyRoute("sb-2", "9.9.9.9", "1")
	s.SetRoute(context.Background(), route)

	got, ok := s.LoadRoute("sb-2")
	require.True(t, ok)
	assert.Equal(t, route, got)
}

// ---- ListRoutes tests ----

func TestListRoutes_Empty(t *testing.T) {
	s := newTestServer(nil)
	assert.Empty(t, s.ListRoutes())
}

func TestListRoutes_MultipleRoutes(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()
	s.SetRoute(ctx, testProxyRoute("sb-1", "1.1.1.1", "1"))
	s.SetRoute(ctx, testProxyRoute("sb-2", "2.2.2.2", "1"))
	s.SetRoute(ctx, testProxyRoute("sb-3", "3.3.3.3", "1"))

	routes := s.ListRoutes()
	assert.Len(t, routes, 3)
}

// ---- DeleteRoute tests ----

func TestDeleteRoute(t *testing.T) {
	idOnly := testProxyRoute("id-only", "1.1.1.1", "1")
	full := testProxyRoute("full", "2.2.2.2", "1")
	full.Namespace = "ns"
	full.Name = "full"

	tests := []struct {
		name     string
		route    *sandboxroute.Route
		deleteID string
	}{
		{name: "ID-only route", route: &idOnly, deleteID: idOnly.ID},
		{name: "full route", route: &full, deleteID: full.ID},
		{name: "absent route", deleteID: "nonexistent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(nil)
			if tt.route != nil {
				result := s.SetRoute(t.Context(), *tt.route)
				require.Equal(t, sandboxroute.EventResultApplied, result.Result)
			}

			s.DeleteRoute(tt.deleteID)

			_, ok := s.LoadRoute(tt.deleteID)
			assert.False(t, ok)
		})
	}
}

// ---- ListPeers tests ----

func TestListPeers_NilManager(t *testing.T) {
	s := newTestServer(nil)
	assert.Nil(t, s.ListPeers())
}

func TestListPeers_WithPeers(t *testing.T) {
	pm := newMockPeers(
		peers.Peer{IP: "10.0.0.1", Name: "node-1"},
		peers.Peer{IP: "10.0.0.2", Name: "node-2"},
	)
	s := newTestServer(pm)

	got := s.ListPeers()
	assert.Len(t, got, 2)
}

// ---- SyncRouteWithPeers tests ----

// startPeerHTTPServer starts a real HTTP server acting as a peer node.
// It listens on SystemPort (7789) equivalent but returns the actual port for injection.
// Since requestPeer uses SystemPort, we override the global client and use a custom peer IP
// that routes to a httptest server bound to the correct path.
//
// Strategy: use net.Listen on a free port, serve /refresh there, then inject the peer as
// "127.0.0.1" with a custom port override via a test-only round-tripper.
//
// Simpler approach: start httptest.Server, record received routes, and replace requestPeerClient
// with custom transport that rewrites the port.

type recordingPeer struct {
	server   *httptest.Server
	received []sandboxroute.Route
	mu       sync.Mutex
}

func newRecordingPeer() *recordingPeer {
	rp := &recordingPeer{}
	mux := http.NewServeMux()
	mux.HandleFunc(RefreshAPI, func(w http.ResponseWriter, r *http.Request) {
		var route sandboxroute.Route
		if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rp.mu.Lock()
		rp.received = append(rp.received, route)
		rp.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	rp.server = httptest.NewServer(mux)
	return rp
}

func (rp *recordingPeer) close() {
	rp.server.Close()
}

func (rp *recordingPeer) getReceived() []sandboxroute.Route {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	result := make([]sandboxroute.Route, len(rp.received))
	copy(result, rp.received)
	return result
}

func TestSyncRouteWithPeers_NoPeers(t *testing.T) {
	s := newTestServer(newMockPeers())

	route := testProxyRoute("sb-1", "1.2.3.4", "1")
	err := s.SyncRouteWithPeers(route)
	assert.NoError(t, err)
}

func TestSyncRouteWithPeers_NilPeersManager(t *testing.T) {
	s := newTestServer(nil)

	route := testProxyRoute("sb-1", "1.2.3.4", "1")
	err := s.SyncRouteWithPeers(route)
	assert.NoError(t, err)
}

func TestSyncRouteWithPeers_TwoNodes_Success(t *testing.T) {
	// Start two recording peer HTTP servers
	peer1 := newRecordingPeer()
	defer peer1.close()
	peer2 := newRecordingPeer()
	defer peer2.close()

	// Extract host and port from each test server
	_, peer1Port, _ := net.SplitHostPort(peer1.server.Listener.Addr().String())
	_, peer2Port, _ := net.SplitHostPort(peer2.server.Listener.Addr().String())

	// Override the global requestPeerClient to rewrite ports per-request.
	// We use custom transport that maps "127.0.0.1:7789" -> actual test port.
	// To support two different peers, we build a mux transport.
	muxTransport := &muxRoundTripper{
		routes: map[string]string{
			fmt.Sprintf("127.0.0.1:%d", SystemPort): peer1.server.URL[7:], // strip "http://"
			fmt.Sprintf("127.0.0.2:%d", SystemPort): peer2.server.URL[7:],
		},
	}
	origClient := requestPeerClient
	requestPeerClient = &http.Client{Timeout: 5 * time.Second, Transport: muxTransport}
	defer func() { requestPeerClient = origClient }()

	_ = peer1Port
	_ = peer2Port

	pm := newMockPeers(
		peers.Peer{IP: "127.0.0.1", Name: "node-1"},
		peers.Peer{IP: "127.0.0.2", Name: "node-2"},
	)
	s := newTestServer(pm)

	route := testProxyRoute("sb-test", "10.0.0.5", "1")
	err := s.SyncRouteWithPeers(route)
	require.NoError(t, err)

	// Both peers should have received the route
	require.Eventually(t, func() bool {
		return len(peer1.getReceived()) == 1 && len(peer2.getReceived()) == 1
	}, 3*time.Second, 50*time.Millisecond)

	assert.Equal(t, route.ID, peer1.getReceived()[0].ID)
	assert.Equal(t, route.ID, peer2.getReceived()[0].ID)
}

func TestSyncRouteWithPeers_TwoNodes_OneFails(t *testing.T) {
	// peer1 is a real server; peer2 points to a non-existent address
	peer1 := newRecordingPeer()
	defer peer1.close()

	muxTransport := &muxRoundTripper{
		routes: map[string]string{
			fmt.Sprintf("127.0.0.1:%d", SystemPort): peer1.server.URL[7:],
			// 127.0.0.2 has no mapping, will fail to connect
		},
	}
	origClient := requestPeerClient
	requestPeerClient = &http.Client{Timeout: 200 * time.Millisecond, Transport: muxTransport}
	defer func() { requestPeerClient = origClient }()

	pm := newMockPeers(
		peers.Peer{IP: "127.0.0.1", Name: "node-1"},
		peers.Peer{IP: "127.0.0.2", Name: "node-2"},
	)
	s := newTestServer(pm)

	route := testProxyRoute("sb-fail", "1.2.3.4", "1")
	err := s.SyncRouteWithPeers(route)
	assert.Error(t, err, "should return error when one peer fails")

	// peer1 should still have received the route
	assert.Eventually(t, func() bool {
		return len(peer1.getReceived()) >= 1
	}, 3*time.Second, 50*time.Millisecond)
}

// muxRoundTripper routes requests to different backends based on request host
type muxRoundTripper struct {
	routes map[string]string // original host:port -> target host:port
}

func (m *muxRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	target, ok := m.routes[req.URL.Host]
	if !ok {
		// No route found, fail with connection refused
		return nil, fmt.Errorf("no route for %s", req.URL.Host)
	}
	cloned := req.Clone(req.Context())
	cloned.URL.Host = target
	return http.DefaultTransport.RoundTrip(cloned)
}

// ---- Two-node memberlist integration test for SyncRouteWithPeers ----

func TestSyncRouteWithPeers_TwoNodes_Memberlist(t *testing.T) {
	// Start two real HTTP servers (acting as proxy system servers on dynamic ports)
	server1 := NewServer(config.SandboxManagerOptions{})
	server2 := NewServer(config.SandboxManagerOptions{})

	// Set up HTTP handlers for /refresh on both servers
	mux1 := http.NewServeMux()
	mux1.HandleFunc(RefreshAPI, func(w http.ResponseWriter, r *http.Request) {
		var route sandboxroute.Route
		_ = json.NewDecoder(r.Body).Decode(&route)
		server1.SetRoute(r.Context(), route)
		w.WriteHeader(http.StatusNoContent)
	})
	mux2 := http.NewServeMux()
	mux2.HandleFunc(RefreshAPI, func(w http.ResponseWriter, r *http.Request) {
		var route sandboxroute.Route
		_ = json.NewDecoder(r.Body).Decode(&route)
		server2.SetRoute(r.Context(), route)
		w.WriteHeader(http.StatusNoContent)
	})

	hs1 := httptest.NewServer(mux1)
	defer hs1.Close()
	hs2 := httptest.NewServer(mux2)
	defer hs2.Close()

	// Build memberlist for two nodes
	fc := fake.NewClientBuilder().WithStatusSubresource(&corev1.Pod{}).Build()
	ml1 := newMemberlistPeerForTest(t, fc, "ml-node-1")
	ml2 := newMemberlistPeerForTest(t, fc, "ml-node-2")

	ctx := context.Background()
	port1, port2 := ml1.port, ml2.port

	require.NoError(t, ml1.peer.Start(ctx, port1))
	require.NoError(t, ml2.peer.Start(ctx, port2))

	defer func() {
		_ = ml1.peer.Stop()
		_ = ml2.peer.Stop()
	}()

	// Wait for mutual discovery
	require.Eventually(t, func() bool {
		return len(ml1.peer.GetPeers()) == 1 && len(ml2.peer.GetPeers()) == 1
	}, 5*time.Second, 100*time.Millisecond, "two nodes should discover each other")

	// Inject peer IPs matching test servers via transport override
	_, rawPort1, _ := net.SplitHostPort(hs1.Listener.Addr().String())
	_, rawPort2, _ := net.SplitHostPort(hs2.Listener.Addr().String())

	_ = rawPort1
	_ = rawPort2

	// Build transport that maps each peer's memberlist IP:7789 -> test server
	members1 := ml1.peer.GetAllMembers()
	members2 := ml2.peer.GetAllMembers()
	require.NotEmpty(t, members1)
	require.NotEmpty(t, members2)
	peer1IP := members1[0].IP
	peer2IP := members2[0].IP

	muxTransport := &muxRoundTripper{
		routes: map[string]string{
			fmt.Sprintf("%s:%d", peer1IP, SystemPort): hs1.Listener.Addr().String(),
			fmt.Sprintf("%s:%d", peer2IP, SystemPort): hs2.Listener.Addr().String(),
		},
	}
	origClient := requestPeerClient
	requestPeerClient = &http.Client{Timeout: 5 * time.Second, Transport: muxTransport}
	defer func() { requestPeerClient = origClient }()

	// Use ml1 as the peers manager for server1
	server1.peersManager = ml1.peer

	route := testProxyRoute("sb-ml", "192.168.1.100", "1")
	err := server1.SyncRouteWithPeers(route)
	require.NoError(t, err)

	// server2 should have received and stored the route
	require.Eventually(t, func() bool {
		_, ok := server2.LoadRoute("sb-ml")
		return ok
	}, 3*time.Second, 50*time.Millisecond, "server2 should receive the synced route")

	got, ok := server2.LoadRoute("sb-ml")
	require.True(t, ok)
	assert.Equal(t, "192.168.1.100", got.IP)
}

// memberlistPeerHandle holds a MemberlistPeers and its bound port
type memberlistPeerHandle struct {
	peer *peers.MemberlistPeers
	port int
}

// newMemberlistPeerForTest creates a MemberlistPeers with a free port
func newMemberlistPeerForTest(t *testing.T, c client.Client, name string) *memberlistPeerHandle {
	t.Helper()
	peer, port, err := peers.CreateTestPeer(t.Context(), c, name)
	require.NoError(t, err)
	return &memberlistPeerHandle{
		peer: peer,
		port: port,
	}
}
