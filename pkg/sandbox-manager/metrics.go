package sandbox_manager // Shared with api.go

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SandboxCreationDuration tracks the time from request to return
	SandboxCreationDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_creation_duration_seconds",
			Help:        "Duration of sandbox creation in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms to ~5.12s
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

	// SandboxPauseDuration tracks the time of sandbox pause operations
	SandboxPauseDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_pause_duration_seconds",
			Help:        "Duration of sandbox pause operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10),
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

	// SandboxResumeDuration tracks the time of sandbox resume operations
	SandboxResumeDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_resume_duration_seconds",
			Help:        "Duration of sandbox resume operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10),
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

	// SandboxClaimDuration tracks the total claim operation duration
	SandboxClaimDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_claim_duration_seconds",
			Help:    "Total claim operation duration in seconds",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		},
	)

	// SandboxClaimStageDuration tracks the duration of each claim stage
	SandboxClaimStageDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_claim_stage_duration_seconds",
			Help:    "Duration of each claim stage in seconds",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
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

	// SandboxCloneDuration tracks the total clone operation duration
	SandboxCloneDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "sandbox_clone_duration_seconds",
			Help:    "Total clone operation duration in seconds",
			Buckets: prometheus.ExponentialBuckets(0.01, 2, 10),
		},
	)

	// SandboxCloneStageDuration tracks the duration of each clone stage
	SandboxCloneStageDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_clone_stage_duration_seconds",
			Help:    "Duration of each clone stage in seconds",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 12),
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

	// --- Delete duration ---

	// SandboxDeleteDuration tracks the time of sandbox delete operations
	SandboxDeleteDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_delete_duration_seconds",
			Help:        "Duration of sandbox delete operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10),
		},
	)

	// --- Route sync metrics ---

	// SandboxRouteSyncDuration tracks route synchronization duration
	SandboxRouteSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "sandbox_route_sync_duration_seconds",
			Help:    "Route synchronization duration in seconds",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 10),
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
	metrics.Registry.MustRegister(SandboxCreationDuration, SandboxCreationResponses,
		SandboxPauseDuration, SandboxPauseResponses,
		SandboxResumeDuration, SandboxResumeResponses,
		SandboxDeleteResponses,
		// Claim
		SandboxClaimDuration, SandboxClaimStageDuration, SandboxClaimTotal, SandboxClaimRetries,
		// Clone
		SandboxCloneDuration, SandboxCloneStageDuration, SandboxCloneTotal,
		// Delete duration
		SandboxDeleteDuration,
		// Route sync
		SandboxRouteSyncDuration, SandboxRouteSyncTotal,
	)
}
