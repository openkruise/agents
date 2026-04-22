package sandbox_manager // Shared with api.go

import (
	"github.com/prometheus/client_golang/prometheus"
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

	// SandboxPauseLatency tracks the time of sandbox pause operations
	SandboxPauseLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_pause_latency_ms",
			Help:    "Latency of sandbox pause operations in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	// SandboxPauseResponses tracks total pause requests and their results
	SandboxPauseResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_pause_responses",
			Help: "Total number of sandbox pause requests and their results",
		},
		[]string{"result"},
	)

	// SandboxResumeLatency tracks the time of sandbox resume operations
	SandboxResumeLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_resume_latency_ms",
			Help:    "Latency of sandbox resume operations in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	// SandboxResumeResponses tracks total resume requests and their results
	SandboxResumeResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_resume_responses",
			Help: "Total number of sandbox resume requests and their results",
		},
		[]string{"result"},
	)

	// SandboxDeleteResponses tracks total delete requests and their results
	SandboxDeleteResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_delete_responses",
			Help: "Total number of sandbox delete requests and their results",
		},
		[]string{"result"},
	)

	// --- Claim metrics ---

	// SandboxClaimDuration tracks the total claim operation latency
	SandboxClaimDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_claim_duration_ms",
			Help:    "Total claim operation latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	// SandboxClaimStageDuration tracks the duration of each claim stage
	SandboxClaimStageDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_claim_stage_duration_ms",
			Help:    "Duration of each claim stage in milliseconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		},
		[]string{"stage"},
	)

	// SandboxClaimTotal tracks total claim operations by result and lock type
	SandboxClaimTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_claim_total",
			Help: "Total number of claim operations",
		},
		[]string{"result", "lock_type"},
	)

	// SandboxClaimRetries tracks the number of retries per claim operation
	SandboxClaimRetries = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_claim_retries",
			Help:    "Number of retries per claim operation",
			Buckets: prometheus.LinearBuckets(0, 1, 11), // 0 to 10 retries
		},
	)

	// --- Clone metrics ---

	// SandboxCloneDuration tracks the total clone operation latency
	SandboxCloneDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_clone_duration_ms",
			Help:    "Total clone operation latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	// SandboxCloneStageDuration tracks the duration of each clone stage
	SandboxCloneStageDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_clone_stage_duration_ms",
			Help:    "Duration of each clone stage in milliseconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 12),
		},
		[]string{"stage"},
	)

	// SandboxCloneTotal tracks total clone operations by result
	SandboxCloneTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_clone_total",
			Help: "Total number of clone operations",
		},
		[]string{"result"},
	)

	// --- Delete latency ---

	// SandboxDeleteLatency tracks the time of sandbox delete operations
	SandboxDeleteLatency = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_delete_latency_ms",
			Help:    "Latency of sandbox delete operations in milliseconds",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10),
		},
	)

	// --- Route sync metrics ---

	// SandboxRouteSyncDuration tracks route synchronization latency
	SandboxRouteSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_route_sync_duration_ms",
			Help:    "Route synchronization latency in milliseconds",
			Buckets: prometheus.ExponentialBuckets(1, 2, 10),
		},
		[]string{"type"},
	)

	// SandboxRouteSyncTotal tracks total route sync operations by type and result
	SandboxRouteSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_route_sync_total",
			Help: "Total number of route sync operations",
		},
		[]string{"type", "result"},
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(SandboxCreationLatency, SandboxCreationResponses,
		SandboxPauseLatency, SandboxPauseResponses,
		SandboxResumeLatency, SandboxResumeResponses,
		SandboxDeleteResponses,
		// Claim
		SandboxClaimDuration, SandboxClaimStageDuration, SandboxClaimTotal, SandboxClaimRetries,
		// Clone
		SandboxCloneDuration, SandboxCloneStageDuration, SandboxCloneTotal,
		// Delete latency
		SandboxDeleteLatency,
		// Route sync
		SandboxRouteSyncDuration, SandboxRouteSyncTotal,
	)
}
