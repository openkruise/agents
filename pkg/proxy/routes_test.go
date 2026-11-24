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
	"net"
	"sync"
	"testing"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"
)

// mockPeers is a simple in-memory Peers implementation for testing
type mockPeers struct {
	mu      sync.RWMutex
	members []peers.Peer
}

func newMockPeers(members ...peers.Peer) *mockPeers {
	return &mockPeers{members: members}
}

func (m *mockPeers) Start(_ context.Context, _ string, _ int) error { return nil }
func (m *mockPeers) Stop(context.Context) error                     { return nil }
func (m *mockPeers) GetPeers() []peers.Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]peers.Peer, len(m.members))
	copy(result, m.members)
	return result
}
func (m *mockPeers) GetAllMembers() []peers.Peer { return m.GetPeers() }
func (m *mockPeers) LocalAddr() net.IP           { return net.ParseIP("127.0.0.1") }
func (m *mockPeers) LocalPort() int              { return 0 }

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

// 传输、重试与地址测试在 refresh；这里只验证公共发送入口的委托行为。
func TestSyncRouteWithPeers_NoDelivery(t *testing.T) {
	tests := []struct {
		name    string
		manager peers.Peers
		wantErr string
	}{
		{name: "no peers", manager: newMockPeers()},
		{name: "nil peers manager"},
		{name: "sender-only peers", manager: newMockPeers(peers.Peer{IP: "127.0.0.1", Name: "sc-controller"})},
		{name: "missing outbound is reported", manager: newMockPeers(peers.Peer{IP: "127.0.0.1", Name: "sm-manager"}), wantErr: "peer outbound client is not configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(tt.manager)
			s.peerOutbound = nil
			err := s.SyncRouteWithPeers(t.Context(), testProxyRoute("sb-1", "1.2.3.4", "1"))
			if tt.wantErr == "" {
				assert.NoError(t, err)
			} else {
				assert.ErrorContains(t, err, tt.wantErr)
			}
		})
	}
}
