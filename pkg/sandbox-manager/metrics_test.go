package sandbox_manager

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
)

func TestObserveMax(t *testing.T) {
	g := prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "test_observe_max",
		Help: "test gauge",
	})

	// Initial value should be 0
	assert.Equal(t, float64(0), testutil.ToFloat64(g))

	// Set to 100
	observeMax(g, 100)
	assert.Equal(t, float64(100), testutil.ToFloat64(g))

	// Lower value should not update
	observeMax(g, 50)
	assert.Equal(t, float64(100), testutil.ToFloat64(g))

	// Higher value should update
	observeMax(g, 200)
	assert.Equal(t, float64(200), testutil.ToFloat64(g))

	// Equal value should not change
	observeMax(g, 200)
	assert.Equal(t, float64(200), testutil.ToFloat64(g))
}

func TestMetricsRegistered(t *testing.T) {
	// Verify all metrics are non-nil (init() registered them)
	assert.NotNil(t, SandboxCreationLatency)
	assert.NotNil(t, SandboxCreationResponses)
	assert.NotNil(t, SandboxPauseDuration)
	assert.NotNil(t, SandboxPauseMaxDuration)
	assert.NotNil(t, SandboxPauseTotal)
	assert.NotNil(t, SandboxResumeDuration)
	assert.NotNil(t, SandboxResumeMaxDuration)
	assert.NotNil(t, SandboxResumeTotal)
	assert.NotNil(t, SandboxClaimDuration)
	assert.NotNil(t, SandboxClaimStageDuration)
	assert.NotNil(t, SandboxClaimTotal)
	assert.NotNil(t, SandboxClaimRetries)
	assert.NotNil(t, SandboxCloneDuration)
	assert.NotNil(t, SandboxCloneStageDuration)
	assert.NotNil(t, SandboxCloneTotal)
	assert.NotNil(t, RouteSyncDuration)
	assert.NotNil(t, RouteSyncTotal)
	assert.NotNil(t, RouteSyncDelay)
}
