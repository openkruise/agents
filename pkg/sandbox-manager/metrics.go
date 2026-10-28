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

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// Values of the shared result label on the sandbox-manager operation counters.
const (
	metricResultSuccess = "success"
	metricResultFailure = "failure"
)

var (
	// sandboxPauseDuration tracks the time of sandbox pause operations
	sandboxPauseDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_pause_duration_seconds",
			Help:        "Duration in seconds of sandbox-manager pause API calls; keep source when aggregating because source=k8s measures CR condition transitions",
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
			Help:        "Duration in seconds of sandbox-manager resume API calls; keep source when aggregating because source=k8s measures CR condition transitions",
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
			Help:        "Duration in seconds of successful sandbox-manager Kill calls; keep source when aggregating because source=k8s measures DeletionTimestamp to informer Delete",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// --- Claim metrics ---

	// sandboxClaimCreationResponses is the legacy claim request counter without lock_type.
	// Deprecated: exposed only when the compatibility switch is enabled; it counts requests,
	// unlike the instance-level sandbox_claim_total.
	sandboxClaimCreationResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_claim_creation_responses",
			Help: "Deprecated request-level claim results: one final result per sandbox-manager Claim call, " +
				"including failures before any sandbox is bound (invalid parameters, missing template, admission, lock conflicts). " +
				"Use sandbox_claim_total for bound instance claim processes",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"}, // "success" or "failure"
	)

	// sandboxClaimDuration records the wall-clock duration of each bound instance claim process.
	sandboxClaimDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "sandbox_claim_duration_seconds",
			Help: "Wall-clock duration in seconds of each instance claim process run by sandbox-manager, " +
				"from attempt start to the end of post-claim processing, for both success and failure. " +
				"type=hot-start: a pooled sandbox is claimed, including one still creating; " +
				"type=cold-start: a new sandbox is created and claimed because the pool is short. " +
				"Failures before any sandbox is bound are not observed",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     append(prometheus.ExponentialBuckets(0.01, 2, 16), 600, 1200),
		},
		[]string{"namespace", "result", "type"},
	)

	// sandboxClaimTotal counts bound instance claim processes by result, claim method and lock type.
	sandboxClaimTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_claim_total",
			Help: "Total number of instance claim processes run by sandbox-manager, counted once per sandbox bound to a claim; " +
				"batch claims and retries that bind different sandboxes count each sandbox. " +
				"type=hot-start (lock_type=update|speculate): a pooled sandbox is claimed, including one still creating; " +
				"type=cold-start (lock_type=create): a new sandbox is created and claimed because the pool is short. " +
				"result=failure means post-claim processing failed after binding; " +
				"failures before any sandbox is bound (invalid parameters, missing template, admission, lock conflicts) are not counted. " +
				"Attribution is exclusive and counts may be summed across source",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result", "type", "lock_type"},
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

	// sandboxCreateTotal counts newly created instances observed Ready or Failed before readiness.
	sandboxCreateTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_create_total",
			Help: "Total number of new sandbox instances created by sandbox-manager, counted once per sandbox UID; " +
				"covers Claim cold-start on pool shortage and Clone. " +
				"result=success on first Ready, result=failure when Failed is observed before first Ready. " +
				"Claiming an existing pooled sandbox, Resume, repeated Ready and pod rebuilds are not counted, " +
				"and delivery failures after Ready do not revoke success. Attribution is exclusive and counts may be summed across source",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxCreateDuration records only trusted creation-to-first-Ready durations of successful creations.
	sandboxCreateDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "sandbox_create_duration_seconds",
			Help: "Duration in seconds from creationTimestamp to first Ready of new sandboxes created by sandbox-manager " +
				"(Claim cold-start and Clone); recorded only for successful creations with trusted timestamps, " +
				"not API call latency",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12),
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
	sandboxClaimCreationResponses,
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

// incClaimRequestResult keeps the legacy request-level scope of one sample per outer call,
// including failures that happen before any sandbox is bound.
func incClaimRequestResult(namespace, result string) {
	if deprecatedResponseMetricsEnabled.Load() {
		sandboxClaimCreationResponses.WithLabelValues(namespace, result).Inc()
	}
}

// recordSandboxClaims deduplicates every claim result actually bound across retries
// by sandbox UID and claim cycle ID.
func recordSandboxClaims(results []infra.SandboxClaimResult) {
	type cycleKey struct{ uid, id string }
	seen := make(map[cycleKey]struct{}, len(results))
	for _, result := range results {
		if result.UID == "" || result.ID == "" || result.Method == "" ||
			(result.Result != infra.ClaimResultSuccess && result.Result != infra.ClaimResultFailure) ||
			result.Duration < 0 {
			continue
		}
		// Reuse the claim-method value observed by Infra instead of renaming hot-start/cold-start.
		claimType := result.Method
		switch result.LockType {
		case infra.LockTypeCreate, infra.LockTypeUpdate, infra.LockTypeSpeculate:
		default:
			continue
		}
		key := cycleKey{result.UID, result.ID}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		sandboxClaimTotal.WithLabelValues(result.Namespace, result.Result, claimType, string(result.LockType)).Inc()
		sandboxClaimDuration.WithLabelValues(result.Namespace, result.Result, claimType).Observe(result.Duration.Seconds())
	}
}

// recordSandboxCreations deduplicates instance UIDs across all attempts of one Claim/Clone call.
// The backend returns only observed creation outcomes; delivery failures do not change them.
func recordSandboxCreations(results []infra.SandboxCreationResult) {
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		if result.UID == "" {
			continue
		}
		if _, exists := seen[result.UID]; exists {
			continue
		}
		seen[result.UID] = struct{}{}
		outcome := metricResultFailure
		if result.Ready {
			outcome = metricResultSuccess
		}
		sandboxCreateTotal.WithLabelValues(result.Namespace, outcome).Inc()
		if result.Ready && result.Duration != nil && *result.Duration >= 0 {
			sandboxCreateDuration.WithLabelValues(result.Namespace).Observe(result.Duration.Seconds())
		}
	}
}

func init() {
	// Register custom metrics with the global prometheus registry. The deprecated
	// *_responses counters are registered separately by registerDeprecatedResponseMetrics.
	metrics.Registry.MustRegister(sandboxCreateTotal, sandboxCreateDuration,
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
