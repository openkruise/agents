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
	"sync/atomic"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/prometheus/client_golang/prometheus/testutil"
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
		Namespace:       "ns",
		Name:            id,
		UID:             types.UID("uid-" + id),
		ResourceVersion: resourceVersion,
	}
}

// ---- SetRoute tests ----

// The full resource-version ordering and validation matrices are authoritative
// in pkg/sandboxroute; this table covers the Server wrapper delegation, load
// visibility, and gauge maintenance only.
func TestSetRoute(t *testing.T) {
	invalid := testProxyRoute("partial", "1.1.1.1", "1")
	invalid.Name = ""

	tests := []struct {
		name         string
		arrange      []sandboxroute.Route
		incoming     sandboxroute.Route
		expectResult sandboxroute.EventResult
		expectIP     string
		expectGauge  float64
	}{
		{
			name:         "first write applied",
			incoming:     testProxyRoute("sb-1", "1.2.3.4", "1"),
			expectResult: sandboxroute.EventResultApplied,
			expectIP:     "1.2.3.4",
			expectGauge:  1,
		},
		{
			name:         "newer version overwrites",
			arrange:      []sandboxroute.Route{testProxyRoute("sb-1", "1.2.3.4", "1")},
			incoming:     testProxyRoute("sb-1", "5.6.7.8", "2"),
			expectResult: sandboxroute.EventResultApplied,
			expectIP:     "5.6.7.8",
			expectGauge:  1,
		},
		{
			name:         "older version ignored",
			arrange:      []sandboxroute.Route{testProxyRoute("sb-1", "5.6.7.8", "5")},
			incoming:     testProxyRoute("sb-1", "1.1.1.1", "3"),
			expectResult: sandboxroute.EventResultIgnored,
			expectIP:     "5.6.7.8",
			expectGauge:  1,
		},
		{
			name:         "equal version ignored",
			arrange:      []sandboxroute.Route{testProxyRoute("sb-1", "5.6.7.8", "2")},
			incoming:     testProxyRoute("sb-1", "1.1.1.1", "2"),
			expectResult: sandboxroute.EventResultIgnored,
			expectIP:     "5.6.7.8",
			expectGauge:  1,
		},
		{
			name:         "invalid route rejected and not stored",
			incoming:     invalid,
			expectResult: sandboxroute.EventResultInvalid,
			expectGauge:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(nil)
			routeCount.Set(-1)
			for _, route := range tt.arrange {
				s.SetRoute(route)
			}

			result := s.SetRoute(tt.incoming)

			assert.Equal(t, tt.expectResult, result.Result)
			if tt.expectResult == sandboxroute.EventResultInvalid {
				assert.Equal(t, sandboxroute.ReasonInvalidRoute, result.Reason)
			}
			got, stored := s.LoadRoute(tt.incoming.ID)
			assert.Equal(t, tt.expectIP != "", stored)
			if tt.expectIP != "" {
				assert.Equal(t, tt.expectIP, got.IP)
			}
			assert.Equal(t, tt.expectGauge, testutil.ToFloat64(routeCount))
		})
	}
}

// ---- Delete tests ----

func TestDelete(t *testing.T) {
	tests := []struct {
		name         string
		arrange      []sandboxroute.Route
		deletion     sandboxroute.Route
		expectResult sandboxroute.EventResult
		expectStored bool
		expectGauge  float64
	}{
		{
			name:         "existing route deleted",
			arrange:      []sandboxroute.Route{testProxyRoute("sb-1", "1.2.3.4", "1")},
			deletion:     testProxyRoute("sb-1", "", "2"),
			expectResult: sandboxroute.EventResultApplied,
			expectGauge:  0,
		},
		{
			name:         "missing route delete keeps the gauge at store length",
			deletion:     testProxyRoute("sb-1", "", "1"),
			expectResult: sandboxroute.EventResultApplied,
			expectGauge:  0,
		},
		{
			name:         "invalid delete leaves the store and refreshes the gauge",
			arrange:      []sandboxroute.Route{testProxyRoute("sb-1", "1.2.3.4", "1")},
			deletion:     sandboxroute.Route{ID: "sb-1", Namespace: "ns", ResourceVersion: "2"},
			expectResult: sandboxroute.EventResultInvalid,
			expectStored: true,
			expectGauge:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(nil)
			for _, route := range tt.arrange {
				s.SetRoute(route)
			}
			routeCount.Set(-1)

			result := s.Delete(tt.deletion)

			assert.Equal(t, tt.expectResult, result.Result)
			_, stored := s.LoadRoute("sb-1")
			assert.Equal(t, tt.expectStored, stored)
			assert.Equal(t, tt.expectGauge, testutil.ToFloat64(routeCount))
		})
	}
}

// ---- ListPeers tests ----

func TestListPeers(t *testing.T) {
	tests := []struct {
		name    string
		manager peers.Peers
		expect  int
	}{
		{name: "nil manager"},
		{
			name: "with peers",
			manager: newMockPeers(
				peers.Peer{IP: "10.0.0.1", Name: "node-1"},
				peers.Peer{IP: "10.0.0.2", Name: "node-2"},
			),
			expect: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(tt.manager)
			assert.Len(t, s.ListPeers(), tt.expect)
		})
	}
}

type recordingPeer struct {
	server   *httptest.Server
	received []sandboxroute.Route
	mu       sync.Mutex
}

func newRecordingPeer() *recordingPeer {
	rp := &recordingPeer{}
	mux := http.NewServeMux()
	mux.HandleFunc(refresh.Path, func(w http.ResponseWriter, r *http.Request) {
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

// overridePeerTransport points the global requestPeerClient at a muxRoundTripper
// for the test's lifetime and restores the original client on cleanup.
func overridePeerTransport(t *testing.T, routes map[string]string, timeout time.Duration) {
	t.Helper()
	origClient := requestPeerClient
	requestPeerClient = &http.Client{Timeout: timeout, Transport: &muxRoundTripper{routes: routes}}
	t.Cleanup(func() { requestPeerClient = origClient })
}

func peerAddr(ip string) string {
	return fmt.Sprintf("%s:%d", ip, refresh.DefaultPort)
}

func TestSyncRouteWithPeers_NoDelivery(t *testing.T) {
	tests := []struct {
		name    string
		manager peers.Peers
	}{
		{name: "no peers", manager: newMockPeers()},
		{name: "nil peers manager"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(tt.manager)
			assert.NoError(t, s.SyncRouteWithPeers(t.Context(), testProxyRoute("sb-1", "1.2.3.4", "1")))
		})
	}
}

func TestSyncRouteWithPeers_TwoNodes_Success(t *testing.T) {
	// Start two recording peer HTTP servers
	peer1 := newRecordingPeer()
	defer peer1.close()
	peer2 := newRecordingPeer()
	defer peer2.close()

	overridePeerTransport(t, map[string]string{
		peerAddr("127.0.0.1"): peer1.server.URL[7:], // strip "http://"
		peerAddr("127.0.0.2"): peer2.server.URL[7:],
	}, 5*time.Second)

	pm := newMockPeers(
		peers.Peer{IP: "127.0.0.1", Name: "node-1"},
		peers.Peer{IP: "127.0.0.2", Name: "node-2"},
	)
	s := newTestServer(pm)

	route := testProxyRoute("sb-test", "10.0.0.5", "1")
	err := s.SyncRouteWithPeers(t.Context(), route)
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

	overridePeerTransport(t, map[string]string{
		peerAddr("127.0.0.1"): peer1.server.URL[7:],
		// 127.0.0.2 has no mapping, will fail to connect
	}, 200*time.Millisecond)

	pm := newMockPeers(
		peers.Peer{IP: "127.0.0.1", Name: "node-1"},
		peers.Peer{IP: "127.0.0.2", Name: "node-2"},
	)
	s := newTestServer(pm)

	route := testProxyRoute("sb-fail", "1.2.3.4", "1")
	err := s.SyncRouteWithPeers(t.Context(), route)
	assert.Error(t, err, "should return error when one peer fails")

	// peer1 should still have received the route
	assert.Eventually(t, func() bool {
		return len(peer1.getReceived()) >= 1
	}, 3*time.Second, 50*time.Millisecond)
}

func TestSyncRouteWithPeers_RejectedNotRetried(t *testing.T) {
	var requestCount atomic.Int64
	rejecting := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		http.Error(w, "invalid route refresh payload", http.StatusBadRequest)
	}))
	defer rejecting.Close()

	overridePeerTransport(t, map[string]string{
		peerAddr("127.0.0.1"): rejecting.URL[7:],
	}, 5*time.Second)

	pm := newMockPeers(peers.Peer{IP: "127.0.0.1", Name: "node-1"})
	s := newTestServer(pm)

	err := s.SyncRouteWithPeers(t.Context(), testProxyRoute("sb-reject", "1.2.3.4", "1"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status code: 400")
	assert.Equal(t, int64(1), requestCount.Load(), "a 4xx peer response must not be retried")
}

func TestSyncRouteWithPeers_5xxRetried(t *testing.T) {
	var requestCount atomic.Int64
	flaky := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requestCount.Add(1) <= 2 {
			http.Error(w, "transient failure", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer flaky.Close()

	overridePeerTransport(t, map[string]string{
		peerAddr("127.0.0.1"): flaky.URL[7:],
	}, 5*time.Second)

	pm := newMockPeers(peers.Peer{IP: "127.0.0.1", Name: "node-1"})
	s := newTestServer(pm)

	err := s.SyncRouteWithPeers(t.Context(), testProxyRoute("sb-5xx", "1.2.3.4", "1"))
	require.NoError(t, err)
	assert.Equal(t, int64(3), requestCount.Load(), "5xx peer responses must be retried until success")
}

func TestSyncRouteWithPeers_CancelledContextStopsRetries(t *testing.T) {
	var requestCount atomic.Int64
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount.Add(1)
		http.Error(w, "transient failure", http.StatusInternalServerError)
	}))
	defer failing.Close()

	overridePeerTransport(t, map[string]string{
		peerAddr("127.0.0.1"): failing.URL[7:],
	}, 5*time.Second)

	pm := newMockPeers(peers.Peer{IP: "127.0.0.1", Name: "node-1"})
	s := newTestServer(pm)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := s.SyncRouteWithPeers(ctx, testProxyRoute("sb-cancel", "1.2.3.4", "1"))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, int64(0), requestCount.Load(), "a cancelled ctx must not send peer requests")
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
	mux1.HandleFunc(refresh.Path, func(w http.ResponseWriter, r *http.Request) {
		var route sandboxroute.Route
		_ = json.NewDecoder(r.Body).Decode(&route)
		server1.SetRoute(route)
		w.WriteHeader(http.StatusNoContent)
	})
	mux2 := http.NewServeMux()
	mux2.HandleFunc(refresh.Path, func(w http.ResponseWriter, r *http.Request) {
		var route sandboxroute.Route
		_ = json.NewDecoder(r.Body).Decode(&route)
		server2.SetRoute(route)
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

	// Build transport that maps each peer's memberlist IP:7789 -> test server
	members1 := ml1.peer.GetAllMembers()
	members2 := ml2.peer.GetAllMembers()
	require.NotEmpty(t, members1)
	require.NotEmpty(t, members2)
	peer1IP := members1[0].IP
	peer2IP := members2[0].IP

	overridePeerTransport(t, map[string]string{
		peerAddr(peer1IP): hs1.Listener.Addr().String(),
		peerAddr(peer2IP): hs2.Listener.Addr().String(),
	}, 5*time.Second)

	// Use ml1 as the peers manager for server1
	server1.peersManager = ml1.peer

	route := testProxyRoute("sb-ml", "192.168.1.100", "1")
	err := server1.SyncRouteWithPeers(t.Context(), route)
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
