package proxy

import (
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/sandbox-manager/config"
)

func TestRoutesTotal_IncrementOnNewRoute(t *testing.T) {
	s := NewServer(nil, nil, config.SandboxManagerOptions{})
	ctx := context.Background()

	before := testutil.ToFloat64(RoutesTotal)
	s.SetRoute(ctx, Route{ID: "sb-1", IP: "1.2.3.4", UID: types.UID("uid-1"), ResourceVersion: "1"})
	after := testutil.ToFloat64(RoutesTotal)

	assert.Equal(t, float64(1), after-before, "RoutesTotal should increment by 1 on new route")
}

func TestRoutesTotal_NoIncrementOnUpdate(t *testing.T) {
	s := NewServer(nil, nil, config.SandboxManagerOptions{})
	ctx := context.Background()

	s.SetRoute(ctx, Route{ID: "sb-2", IP: "1.2.3.4", UID: types.UID("uid-2"), ResourceVersion: "1"})
	before := testutil.ToFloat64(RoutesTotal)

	// Update same route with newer version
	s.SetRoute(ctx, Route{ID: "sb-2", IP: "5.6.7.8", UID: types.UID("uid-2"), ResourceVersion: "2"})
	after := testutil.ToFloat64(RoutesTotal)

	assert.Equal(t, float64(0), after-before, "RoutesTotal should not increment on route update")
}

func TestRoutesTotal_DecrementOnDelete(t *testing.T) {
	s := NewServer(nil, nil, config.SandboxManagerOptions{})
	ctx := context.Background()

	s.SetRoute(ctx, Route{ID: "sb-3", IP: "1.2.3.4", UID: types.UID("uid-3"), ResourceVersion: "1"})
	before := testutil.ToFloat64(RoutesTotal)

	s.DeleteRoute("sb-3")
	after := testutil.ToFloat64(RoutesTotal)

	assert.Equal(t, float64(-1), after-before, "RoutesTotal should decrement by 1 on delete")
}

func TestRoutesTotal_NoDecrementOnDeleteNonExistent(t *testing.T) {
	s := NewServer(nil, nil, config.SandboxManagerOptions{})

	before := testutil.ToFloat64(RoutesTotal)
	s.DeleteRoute("non-existent")
	after := testutil.ToFloat64(RoutesTotal)

	assert.Equal(t, float64(0), after-before, "RoutesTotal should not change when deleting non-existent route")
}

func TestPeersTotal_SetOnSyncRouteWithPeers(t *testing.T) {
	mp := newMockPeers()
	s := NewServer(nil, mp, config.SandboxManagerOptions{})

	// No peers
	_ = s.SyncRouteWithPeers(Route{ID: "sb-1", IP: "1.2.3.4", ResourceVersion: "1"})
	assert.Equal(t, float64(0), testutil.ToFloat64(PeersTotal))
}
