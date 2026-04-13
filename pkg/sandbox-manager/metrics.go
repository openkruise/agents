package sandbox_manager // Shared with api.go

import (
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SandboxCreationLatency tracks the time from request to return
	SandboxCreationLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_creation_latency_ms",
			Help:    "Latency of sandbox creation in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10), // 10ms to ~10s
		},
	)

	// SandboxCreationResponses tracks total requests and failures
	SandboxCreationResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_creation_responses",
			Help: "Total number of sandbox creation requests and their results",
		},
		[]string{"result"}, // "success" or "failure"
	)

	// --- Pause metrics ---

	SandboxPauseDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_pause_duration_ms",
			Help:    "Pause operation latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	SandboxPauseMaxDuration = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "sandboxmanager_pause_max_duration_ms",
			Help: "Maximum pause operation duration observed",
		},
	)

	SandboxPauseTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandboxmanager_pause_total",
			Help: "Total number of pause operations",
		},
		[]string{"status"},
	)

	// --- Resume metrics ---

	SandboxResumeDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_resume_duration_ms",
			Help:    "Resume operation latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	SandboxResumeMaxDuration = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "sandboxmanager_resume_max_duration_ms",
			Help: "Maximum resume operation duration observed",
		},
	)

	SandboxResumeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandboxmanager_resume_total",
			Help: "Total number of resume operations",
		},
		[]string{"status"},
	)

	// --- Claim metrics ---

	SandboxClaimDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_claim_duration_ms",
			Help:    "Total claim operation latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	SandboxClaimStageDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_claim_stage_duration_ms",
			Help:    "Duration of each claim stage in milliseconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		},
		[]string{"stage"},
	)

	SandboxClaimTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandboxmanager_claim_total",
			Help: "Total number of claim operations",
		},
		[]string{"status", "lock_type"},
	)

	SandboxClaimRetries = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_claim_retries",
			Help:    "Number of retries per claim operation",
			Buckets: prometheus.LinearBuckets(0, 1, 11),
		},
	)

	// --- Clone metrics ---

	SandboxCloneDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_clone_duration_ms",
			Help:    "Total clone operation latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	SandboxCloneStageDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_clone_stage_duration_ms",
			Help:    "Duration of each clone stage in milliseconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		},
		[]string{"stage"},
	)

	SandboxCloneTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandboxmanager_clone_total",
			Help: "Total number of clone operations",
		},
		[]string{"status"},
	)

	// --- Route sync metrics ---

	RouteSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandboxmanager_route_sync_duration_ms",
			Help:    "Route synchronization latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10),
		},
		[]string{"type"},
	)

	RouteSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandboxmanager_route_sync_total",
			Help: "Total number of route sync operations",
		},
		[]string{"type", "status"},
	)

	RouteSyncDelay = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "sandboxmanager_route_sync_delay_ms",
			Help: "Current routing synchronization delay in milliseconds",
		},
	)
)

// observeMax updates a Gauge to the maximum of its current value and the new value.
func observeMax(g prometheus.Gauge, val float64) {
	m := &dto.Metric{}
	_ = g.Write(m)
	if val > m.GetGauge().GetValue() {
		g.Set(val)
	}
}

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(
		SandboxCreationLatency,
		SandboxCreationResponses,
		// Pause
		SandboxPauseDuration,
		SandboxPauseMaxDuration,
		SandboxPauseTotal,
		// Resume
		SandboxResumeDuration,
		SandboxResumeMaxDuration,
		SandboxResumeTotal,
		// Claim
		SandboxClaimDuration,
		SandboxClaimStageDuration,
		SandboxClaimTotal,
		SandboxClaimRetries,
		// Clone
		SandboxCloneDuration,
		SandboxCloneStageDuration,
		SandboxCloneTotal,
		// Route sync
		RouteSyncDuration,
		RouteSyncTotal,
		RouteSyncDelay,
	)
}
