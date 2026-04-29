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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// sandboxPauseDuration tracks the time of sandbox pause operations
	sandboxPauseDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_pause_duration_seconds",
			Help:        "Duration of sandbox pause operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// sandboxPauseResponses tracks total pause requests and their results
	sandboxPauseResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_pause_responses",
			Help:        "Total number of sandbox pause requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// sandboxResumeDuration tracks the time of sandbox resume operations
	sandboxResumeDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_resume_duration_seconds",
			Help:        "Duration of sandbox resume operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// sandboxResumeResponses tracks total resume requests and their results
	sandboxResumeResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_resume_responses",
			Help:        "Total number of sandbox resume requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// sandboxDeleteResponses tracks total delete requests and their results
	sandboxDeleteResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_delete_responses",
			Help:        "Total number of sandbox delete requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
	)

	// sandboxDeleteDuration tracks the time of sandbox delete operations
	sandboxDeleteDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_delete_duration_seconds",
			Help:        "Duration of sandbox delete operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// --- Claim metrics ---

	// sandboxClaimCreationResponses tracks total requests and failures
	sandboxClaimCreationResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_creation_responses",
			Help:        "Total number of sandbox creation requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"}, // "success" or "failure"
	)

	// sandboxClaimDuration tracks the total claim operation duration
	sandboxClaimDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_duration_seconds",
			Help:        "Total claim operation duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// sandboxClaimTotal tracks total claim operations by result and lock type
	sandboxClaimTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_claim_total",
			Help:        "Total number of claim operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result", "lock_type"},
	)

	// sandboxClaimRetries tracks the number of retries per claim operation
	sandboxClaimRetries = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_retries",
			Help:        "Number of retries per claim operation",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.LinearBuckets(0, 1, 11), // 0 to 10 bigger step for retries
		},
	)

	// --- Clone metrics ---

	// SandboxCloneDuration tracks the total clone operation duration
	sandboxCloneDuration = prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:        "sandbox_clone_duration_seconds",
			Help:        "Total clone operation duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
	)

	// SandboxCloneTotal tracks total clone operations by result
	sandboxCloneTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_clone_total",
			Help:        "Total number of clone operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"result"},
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
		[]string{"type"},
	)

	// SandboxRouteSyncTotal tracks total route sync operations by type and result
	sandboxRouteSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_route_sync_total",
			Help:        "Total number of route sync operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"type", "result"},
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(sandboxClaimCreationResponses,
		sandboxPauseDuration, sandboxPauseResponses,
		sandboxResumeDuration, sandboxResumeResponses,
		sandboxDeleteResponses,
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
