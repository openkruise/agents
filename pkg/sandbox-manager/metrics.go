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

package sandbox_manager // Shared with api.go

import (
	"sync"
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// sandboxPauseDuration tracks the time of sandbox pause operations
	sandboxPauseDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_pause_duration_seconds",
			Help:        "Duration of sandbox pause operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// sandboxPauseTotal tracks total pause requests and their results
	sandboxPauseTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_pause_total",
			Help:        "Total number of sandbox pause requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxPauseResponses tracks total pause requests and their results.
	//
	// Deprecated: migrated to sandbox_pause_total. Only recorded and exposed when
	// --enable-deprecated-response-metrics is set; will be removed in a future release.
	sandboxPauseResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_pause_responses",
			Help:        "Deprecated: use sandbox_pause_total. Total number of sandbox pause requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxResumeDuration tracks the time of sandbox resume operations
	sandboxResumeDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_resume_duration_seconds",
			Help:        "Duration of sandbox resume operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// sandboxResumeTotal tracks total resume requests and their results
	sandboxResumeTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_resume_total",
			Help:        "Total number of sandbox resume requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxResumeResponses tracks total resume requests and their results.
	//
	// Deprecated: migrated to sandbox_resume_total. Only recorded and exposed when
	// --enable-deprecated-response-metrics is set; will be removed in a future release.
	sandboxResumeResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_resume_responses",
			Help:        "Deprecated: use sandbox_resume_total. Total number of sandbox resume requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxDeleteTotal tracks total delete requests and their results
	sandboxDeleteTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_delete_total",
			Help:        "Total number of sandbox delete requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxDeleteResponses tracks total delete requests and their results.
	//
	// Deprecated: migrated to sandbox_delete_total. Only recorded and exposed when
	// --enable-deprecated-response-metrics is set; will be removed in a future release.
	sandboxDeleteResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_delete_responses",
			Help:        "Deprecated: use sandbox_delete_total. Total number of sandbox delete requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxDeleteDuration tracks the time of sandbox delete operations
	sandboxDeleteDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_delete_duration_seconds",
			Help:        "Duration of sandbox delete operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// --- Claim metrics ---

	// sandboxClaimCreationResponses tracks claim request results. It keeps its
	// existing name and is always exposed: a claim may hand out a Sandbox that
	// already exists in the pool, which is not the same as creating a Sandbox CR.
	sandboxClaimCreationResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_creation_responses",
			Help:        "Total number of sandbox creation requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"}, // "success" or "failure"
	)

	// sandboxClaimDuration tracks the total claim operation duration
	sandboxClaimDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_duration_seconds",
			Help:        "Total claim operation duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// sandboxClaimTotal tracks total claim operations by result and lock type
	sandboxClaimTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_total",
			Help:        "Total number of claim operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result", "lock_type"},
	)

	// sandboxClaimRetries tracks the number of retries per claim operation
	sandboxClaimRetries = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_retries",
			Help:        "Number of retries per claim operation",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.LinearBuckets(0, 1, 11), // 0 to 10 bigger step for retries
		},
		[]string{"namespace"},
	)

	// --- Clone metrics ---

	// SandboxCloneDuration tracks the total clone operation duration
	sandboxCloneDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_clone_duration_seconds",
			Help:        "Total clone operation duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// SandboxCloneTotal tracks total clone operations by result
	sandboxCloneTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_clone_total",
			Help:        "Total number of clone operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// --- Route sync metrics ---

	// sandboxRouteSyncDuration tracks route synchronization duration
	sandboxRouteSyncDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_route_sync_duration_seconds",
			Help:        "Route synchronization duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace", "type"},
	)

	// SandboxRouteSyncTotal tracks total route sync operations by type and result
	sandboxRouteSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_route_sync_total",
			Help:        "Total number of route sync operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "type", "result"},
	)
)

// deprecatedResponseCounters lists the legacy *_responses counters. They are
// only registered, and only incremented, once registerDeprecatedResponseMetrics
// has enabled them.
var deprecatedResponseCounters = []prometheus.Collector{
	sandboxPauseResponses,
	sandboxResumeResponses,
	sandboxDeleteResponses,
}

// registerDeprecatedOnce guards the process-wide registry: a builder may be
// built more than once in a process, but a collector can be registered only once.
var registerDeprecatedOnce sync.Once

// deprecatedResponseMetricsEnabled reports whether the deprecated *_responses
// counters are exported. It gates incResult so that a disabled legacy counter
// costs nothing: no label lookup, no increment, and no child series kept alive
// for a metric nobody can scrape. The scope is process-global for the same
// reason the registration is: the /metrics registry is process-global.
var deprecatedResponseMetricsEnabled atomic.Bool

// registerDeprecatedResponseMetrics exposes the deprecated *_responses counters
// on the controller-runtime registry. Registration is process-global because the
// /metrics registry is, so the first caller wins for the lifetime of the process.
func registerDeprecatedResponseMetrics() {
	registerDeprecatedOnce.Do(func() {
		if err := registerCollectors(metrics.Registry, deprecatedResponseCounters...); err != nil {
			klog.ErrorS(err, "failed to register deprecated sandbox *_responses metrics")
			return
		}
		deprecatedResponseMetricsEnabled.Store(true)
	})
}

// registerCollectors registers every collector and returns the first error.
func registerCollectors(reg prometheus.Registerer, collectors ...prometheus.Collector) error {
	for _, c := range collectors {
		if err := reg.Register(c); err != nil {
			return err
		}
	}
	return nil
}

// incResult increments the canonical *_total counter for one operation result.
// The deprecated *_responses alias is only incremented while it is exported;
// disabling it stops the duplicate bookkeeping, not the operation count.
func incResult(total, deprecated *prometheus.CounterVec, namespace, result string) {
	total.WithLabelValues(namespace, result).Inc()
	if deprecatedResponseMetricsEnabled.Load() {
		deprecated.WithLabelValues(namespace, result).Inc()
	}
}

func init() {
	// Register custom metrics with the global prometheus registry. The deprecated
	// *_responses counters are registered separately by registerDeprecatedResponseMetrics.
	metrics.Registry.MustRegister(sandboxClaimCreationResponses,
		sandboxPauseDuration, sandboxPauseTotal,
		sandboxResumeDuration, sandboxResumeTotal,
		sandboxDeleteTotal,
		// Claim
		sandboxClaimDuration, sandboxClaimTotal, sandboxClaimRetries,
		// Clone
		sandboxCloneDuration, sandboxCloneTotal,
		// Delete duration
		sandboxDeleteDuration,
		// Route sync
		sandboxRouteSyncDuration, sandboxRouteSyncTotal,
	)
}
