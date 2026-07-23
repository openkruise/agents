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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"

	"github.com/openkruise/agents/pkg/peers"
)

func TestRoutesTotal_IncrementOnNewRoute(t *testing.T) {
	s := newTestServer(nil)
	routeCount.Set(0)

	before := testutil.ToFloat64(routeCount)
	s.SetRoute(context.Background(), testProxyRoute("metrics-1", "1.2.3.4", "1"))
	after := testutil.ToFloat64(routeCount)

	assert.Equal(t, float64(1), after-before)
}

func TestRoutesTotal_NoIncrementOnUpdate(t *testing.T) {
	s := newTestServer(nil)
	ctx := context.Background()
	routeCount.Set(0)

	s.SetRoute(ctx, testProxyRoute("metrics-2", "1.2.3.4", "1"))
	before := testutil.ToFloat64(routeCount)
	s.SetRoute(ctx, testProxyRoute("metrics-2", "5.6.7.8", "2"))
	after := testutil.ToFloat64(routeCount)

	assert.Equal(t, float64(0), after-before)
}

func TestRoutesTotal_DecrementOnDelete(t *testing.T) {
	s := newTestServer(nil)
	routeCount.Set(0)
	s.SetRoute(context.Background(), testProxyRoute("metrics-3", "1.2.3.4", "1"))

	before := testutil.ToFloat64(routeCount)
	s.DeleteRoute("metrics-3")
	after := testutil.ToFloat64(routeCount)

	assert.Equal(t, float64(-1), after-before)
}

func TestRoutesTotal_NoDecrementOnDeleteNonExistent(t *testing.T) {
	s := newTestServer(nil)
	routeCount.Set(0)

	before := testutil.ToFloat64(routeCount)
	s.DeleteRoute("non-existent-route")
	after := testutil.ToFloat64(routeCount)

	assert.Equal(t, float64(0), after-before)
}

func TestPeersTotal_SetOnSyncRouteWithPeers(t *testing.T) {
	pm := newMockPeers(
		peers.Peer{IP: "10.0.0.1", Name: "node-1"},
		peers.Peer{IP: "10.0.0.2", Name: "node-2"},
		peers.Peer{IP: "10.0.0.3", Name: "node-3"},
	)
	s := newTestServer(pm)

	// SyncRouteWithPeers will fail on actual HTTP calls, but peerCount should still be set
	_ = s.SyncRouteWithPeers(testProxyRoute("metrics-peers", "1.2.3.4", "1"))

	assert.Equal(t, float64(3), testutil.ToFloat64(peerCount))
}
