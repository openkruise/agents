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
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/openkruise/agents/pkg/peers"
)

func TestRoutesTotal(t *testing.T) {
	tests := []struct {
		name        string
		arrange     func(t *testing.T, s *Server)
		op          func(t *testing.T, s *Server)
		expectDelta float64
	}{
		{
			name:        "increment on new route",
			op:          func(_ *testing.T, s *Server) { s.SetRoute(testProxyRoute("metrics-1", "1.2.3.4", "1")) },
			expectDelta: 1,
		},
		{
			name:        "no increment on update",
			arrange:     func(_ *testing.T, s *Server) { s.SetRoute(testProxyRoute("metrics-2", "1.2.3.4", "1")) },
			op:          func(_ *testing.T, s *Server) { s.SetRoute(testProxyRoute("metrics-2", "5.6.7.8", "2")) },
			expectDelta: 0,
		},
		{
			name:        "decrement on delete",
			arrange:     func(_ *testing.T, s *Server) { s.SetRoute(testProxyRoute("metrics-3", "1.2.3.4", "1")) },
			op:          func(t *testing.T, s *Server) { s.Delete(testProxyRoute("metrics-3", "", "2")) },
			expectDelta: -1,
		},
		{
			name:        "no decrement on delete of missing route",
			op:          func(t *testing.T, s *Server) { s.Delete(testProxyRoute("non-existent-route", "", "1")) },
			expectDelta: 0,
		},
	}

	// The routeCount gauge is process-global: rows run serially and reset it.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServer(nil)
			routeCount.Set(0)
			if tt.arrange != nil {
				tt.arrange(t, s)
			}

			before := testutil.ToFloat64(routeCount)
			tt.op(t, s)
			after := testutil.ToFloat64(routeCount)

			assert.Equal(t, tt.expectDelta, after-before)
		})
	}
}

func TestPeersTotal_SetOnSyncRouteWithPeers(t *testing.T) {
	// No transport mappings: every peer request fails fast without real dials.
	overridePeerTransport(t, nil, time.Second)
	peerCount.Set(0)
	pm := newMockPeers(
		peers.Peer{IP: "10.0.0.1", Name: "node-1"},
		peers.Peer{IP: "10.0.0.2", Name: "node-2"},
		peers.Peer{IP: "10.0.0.3", Name: "node-3"},
	)
	s := newTestServer(pm)

	// SyncRouteWithPeers fails on the HTTP calls, but peerCount must still be set.
	_ = s.SyncRouteWithPeers(t.Context(), testProxyRoute("metrics-peers", "1.2.3.4", "1"))

	assert.Equal(t, float64(3), testutil.ToFloat64(peerCount))
}
