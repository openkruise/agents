package sandbox_manager // Shared with api.go

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// SandboxClaimCreationDuration tracks the time from request to return
	SandboxClaimCreationDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_creation_duration_seconds",
			Help:        "Duration of sandbox creation in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms -> 40s
		},
	)

	// SandboxClaimCreationResponses tracks total requests and failures
	SandboxClaimCreationResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_creation_responses",
			Help:        "Total number of sandbox creation requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"}, // "success" or "failure"
	)

	// SandboxClaimPauseDuration tracks the time of sandbox pause operations
	SandboxClaimPauseDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_pause_duration_seconds",
			Help:        "Duration of sandbox pause operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms -> 40s
		},
	)

	// SandboxClaimPauseResponses tracks total pause requests and their results
	SandboxClaimPauseResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_pause_responses",
			Help:        "Total number of sandbox pause requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// SandboxClaimResumeDuration tracks the time of sandbox resume operations
	SandboxClaimResumeDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_resume_duration_seconds",
			Help:        "Duration of sandbox resume operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms -> 40s
		},
	)

	// SandboxClaimResumeResponses tracks total resume requests and their results
	SandboxClaimResumeResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_resume_responses",
			Help:        "Total number of sandbox resume requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// SandboxClaimDeleteResponses tracks total delete requests and their results
	SandboxClaimDeleteResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_delete_responses",
			Help:        "Total number of sandbox delete requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// --- Claim metrics ---

	// SandboxClaimDuration tracks the total claim operation duration
	SandboxClaimDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_duration_seconds",
			Help:        "Total claim operation duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10),
		},
	)

	// SandboxClaimTotal tracks total claim operations by result and lock type
	SandboxClaimTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_total",
			Help:        "Total number of claim operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result", "lock_type"},
	)

	// SandboxClaimRetries tracks the number of retries per claim operation
	SandboxClaimRetries = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_retries",
			Help:        "Number of retries per claim operation",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.LinearBuckets(0, 1, 11), // 0 to 10 retries
		},
	)

	// --- Clone metrics ---

	// SandboxClaimCloneDuration tracks the total clone operation duration
	SandboxClaimCloneDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_clone_duration_seconds",
			Help:        "Total clone operation duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10),
		},
	)

	// SandboxClaimCloneTotal tracks total clone operations by result
	SandboxClaimCloneTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_clone_total",
			Help:        "Total number of clone operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// --- Delete duration ---

	// SandboxClaimDeleteDuration tracks the time of sandbox delete operations
	SandboxClaimDeleteDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_delete_duration_seconds",
			Help:        "Duration of sandbox delete operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.01, 2, 10), // 10ms -> 40s
		},
	)

	// --- Route sync metrics ---

	// SandboxClaimRouteSyncDuration tracks route synchronization duration
	SandboxClaimRouteSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_route_sync_duration_seconds",
			Help:        "Route synchronization duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.001, 2, 10),
		},
		[]string{"type"},
	)

	// SandboxClaimRouteSyncTotal tracks total route sync operations by type and result
	SandboxClaimRouteSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_route_sync_total",
			Help:        "Total number of route sync operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"type", "result"},
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(SandboxClaimCreationDuration, SandboxClaimCreationResponses,
		SandboxClaimPauseDuration, SandboxClaimPauseResponses,
		SandboxClaimResumeDuration, SandboxClaimResumeResponses,
		SandboxClaimDeleteResponses,
		// Claim
		SandboxClaimDuration, SandboxClaimTotal, SandboxClaimRetries,
		// Clone
		SandboxClaimCloneDuration, SandboxClaimCloneTotal,
		// Delete duration
		SandboxClaimDeleteDuration,
		// Route sync
		SandboxClaimRouteSyncDuration, SandboxClaimRouteSyncTotal,
	)
}
