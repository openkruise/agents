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
	sandboxPauseDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_pause_duration_seconds",
			Help:        "Duration of sandbox pause operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// sandboxPauseResponses tracks total pause requests and their results
	sandboxPauseResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_pause_responses",
			Help:        "Total number of sandbox pause requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxPauseMaxDuration tracks the maximum pause operation duration observed
	sandboxPauseMaxDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_pause_max_duration_seconds",
			Help:        "Maximum duration of sandbox pause operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace"},
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

	// sandboxResumeResponses tracks total resume requests and their results
	sandboxResumeResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_resume_responses",
			Help:        "Total number of sandbox resume requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxResumeMaxDuration tracks the maximum resume operation duration observed
	sandboxResumeMaxDuration = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_resume_max_duration_seconds",
			Help:        "Maximum duration of sandbox resume operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace"},
	)

	// sandboxDeleteResponses tracks total delete requests and their results
	sandboxDeleteResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_delete_responses",
			Help:        "Total number of sandbox delete requests and their results",
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

	// sandboxRecycleResponses tracks total recycle requests and their results
	sandboxRecycleResponses = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_recycle_responses",
			Help:        "Total number of sandbox recycle requests and their results",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "result"},
	)

	// sandboxRecycleDuration tracks the time of sandbox recycle trigger operations
	sandboxRecycleDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_recycle_duration_seconds",
			Help:        "Duration of sandbox recycle trigger operations in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// --- Claim metrics ---

	// sandboxClaimCreationResponses tracks total requests and failures
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

	// sandboxClaimStageDuration tracks claim operation stage durations
	sandboxClaimStageDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_stage_duration_seconds",
			Help:        "Duration of each claim stage in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.005, 2, 12), // 5ms -> ~10s
		},
		[]string{"namespace", "stage"},
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

	// sandboxCloneDuration tracks the total clone operation duration
	sandboxCloneDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_clone_duration_seconds",
			Help:        "Total clone operation duration in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.02, 2, 12), // 20ms -> ~41s
		},
		[]string{"namespace"},
	)

	// sandboxCloneStageDuration tracks clone operation stage durations
	sandboxCloneStageDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_clone_stage_duration_seconds",
			Help:        "Duration of each clone stage in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
			Buckets:     prometheus.ExponentialBuckets(0.005, 2, 12), // 5ms -> ~10s
		},
		[]string{"namespace", "stage"},
	)

	// sandboxCloneTotal tracks total clone operations by result
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

	// sandboxRouteSyncTotal tracks total route sync operations by type and result
	sandboxRouteSyncTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name:        "sandbox_route_sync_total",
			Help:        "Total number of route sync operations",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace", "type", "result"},
	)

	// sandboxRouteSyncDelay tracks current route synchronization delay
	sandboxRouteSyncDelay = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_route_sync_delay_seconds",
			Help:        "Current routing synchronization delay in seconds",
			ConstLabels: prometheus.Labels{"source": "e2b"},
		},
		[]string{"namespace"},
	)
)

func init() {
	// Register custom metrics with the global prometheus registry
	metrics.Registry.MustRegister(sandboxClaimCreationResponses,
		sandboxPauseDuration, sandboxPauseResponses, sandboxPauseMaxDuration,
		sandboxResumeDuration, sandboxResumeResponses, sandboxResumeMaxDuration,
		sandboxDeleteResponses, sandboxRecycleResponses,
		// Claim
		sandboxClaimDuration, sandboxClaimStageDuration, sandboxClaimTotal, sandboxClaimRetries,
		// Clone
		sandboxCloneDuration, sandboxCloneStageDuration, sandboxCloneTotal,
		// Delete & Recycle duration
		sandboxDeleteDuration, sandboxRecycleDuration,
		// Route sync
		sandboxRouteSyncDuration, sandboxRouteSyncTotal, sandboxRouteSyncDelay,
	)
}
