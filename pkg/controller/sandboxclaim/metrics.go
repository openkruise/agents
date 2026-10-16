/*
Copyright 2025.

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

package sandboxclaim

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var (
	// sandboxClaimInfo records sandbox claim metadata as metric labels.
	sandboxClaimInfo = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_claim_info",
			Help:        "Information about the sandbox claim",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "name", "template_name", "uid"},
	)

	// sandboxClaimCreated records the creation timestamp of a sandbox claim.
	sandboxClaimCreated = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_claim_created",
			Help:        "Unix creation timestamp of the sandbox claim",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "name"},
	)

	// sandboxClaimStatusPhase represents the current phase of a sandbox claim.
	sandboxClaimStatusPhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_claim_status_phase",
			Help:        "The current phase of the sandbox claim (1 for active phase)",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "name", "phase"},
	)

	// sandboxClaimClaimStartTime records the timestamp when claiming started.
	sandboxClaimClaimStartTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_claim_start_time",
			Help:        "Unix timestamp when the sandbox claim started claiming",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "name"},
	)

	// sandboxClaimCompletionTime records the timestamp when the claim completed.
	sandboxClaimCompletionTime = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_claim_completion_time",
			Help:        "Unix timestamp when the sandbox claim completed",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "name"},
	)

	// sandboxClaimClaimedReplicas tracks the number of claimed replicas.
	sandboxClaimClaimedReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_claim_claimed_replicas",
			Help:        "Current number of claimed replicas in the sandbox claim",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "name"},
	)

	// sandboxClaimDesiredReplicas tracks the desired number of replicas.
	sandboxClaimDesiredReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name:        "sandbox_claim_desired_replicas",
			Help:        "Desired number of replicas in the sandbox claim",
			ConstLabels: prometheus.Labels{"source": "k8s"},
		},
		[]string{"namespace", "name"},
	)

	// sandboxClaimClaimDuration records the histogram of sandbox claim durations
	// (from ClaimStartTime to CompletionTime) in seconds.
	// This is observed exactly once per claim when it first reaches Completed phase.
	sandboxClaimClaimDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:        "sandbox_claim_duration_seconds",
			Help:        "Duration of sandbox claim operations from start to completion in seconds",
			ConstLabels: prometheus.Labels{"source": "k8s"},
			// Exponential 10ms -> 327.68s covers warm-pool sub-100ms claims and cold
			// starts; 600s and 1200s tail buckets cover large user-specified
			// ClaimTimeout values instead of dumping them all into +Inf.
			Buckets: append(prometheus.ExponentialBuckets(0.01, 2, 16), 600, 1200),
		},
		[]string{"namespace"},
	)

	// allClaimPhases enumerates all possible sandbox claim phases for metric cleanup.
	allClaimPhases = []agentsv1alpha1.SandboxClaimPhase{
		agentsv1alpha1.SandboxClaimPhaseClaiming,
		agentsv1alpha1.SandboxClaimPhaseCompleted,
	}
)

// observedClaimDurations tracks which claims have already had their duration
// observed in the histogram, preventing duplicate observations on re-reconcile.
var observedClaimDurations sync.Map

// preciseClaimStartTimes and preciseClaimCompletionTimes keep the in-memory
// precise timestamps this process wrote for a claim. Values that round-tripped
// through the API server are second-truncated, so durations computed from the
// cached claim alone are quantized to whole seconds. They are keyed by claim
// UID because claim names are reused: a new claim with the same name must never
// inherit a deleted claim's timestamps.
var preciseClaimStartTimes sync.Map
var preciseClaimCompletionTimes sync.Map

// preciseClaimUIDs maps "namespace/name" to the UID whose precise timestamps
// are stored for that claim, so deleteSandboxClaimMetrics — which only receives
// namespace and name — can find and remove them.
var preciseClaimUIDs sync.Map

// storePreciseTime stores t under key, keeping the first stored value and
// replacing a second-truncated stored value with a precise one from the same
// Unix second (a truncated cache value losing the race with an in-memory write).
func storePreciseTime(m *sync.Map, key string, t time.Time) {
	stored, loaded := m.LoadOrStore(key, t)
	if !loaded {
		return
	}
	prev := stored.(time.Time)
	if prev.Nanosecond() == 0 && t.Nanosecond() != 0 && prev.Unix() == t.Unix() {
		m.Store(key, t)
	}
}

func init() {
	metrics.Registry.MustRegister(
		sandboxClaimInfo,
		sandboxClaimCreated,
		sandboxClaimStatusPhase,
		sandboxClaimClaimStartTime,
		sandboxClaimCompletionTime,
		sandboxClaimClaimedReplicas,
		sandboxClaimDesiredReplicas,
		sandboxClaimClaimDuration,
	)
}

// recordSandboxClaimMetrics updates all sandbox claim lifecycle metrics based on the current claim state.
func recordSandboxClaimMetrics(claim *agentsv1alpha1.SandboxClaim) {
	namespace := claim.Namespace
	name := claim.Name

	// sandbox_claim_info
	sandboxClaimInfo.WithLabelValues(namespace, name, claim.Spec.TemplateName, string(claim.UID)).Set(1)

	// sandbox_claim_created
	sandboxClaimCreated.WithLabelValues(namespace, name).Set(float64(claim.CreationTimestamp.Unix()))

	// sandbox_claim_status_phase: Only emit the current phase (value=1), delete stale phase series to reduce cardinality.
	currentPhase := claim.Status.Phase
	if currentPhase != "" {
		for _, p := range allClaimPhases {
			if p != currentPhase {
				sandboxClaimStatusPhase.DeleteLabelValues(namespace, name, string(p))
			}
		}
		sandboxClaimStatusPhase.WithLabelValues(namespace, name, string(currentPhase)).Set(1)
	}

	// sandbox_claim_start_time
	if claim.Status.ClaimStartTime != nil {
		sandboxClaimClaimStartTime.WithLabelValues(namespace, name).Set(float64(claim.Status.ClaimStartTime.Unix()))
	}

	// sandbox_claim_completion_time
	if claim.Status.CompletionTime != nil {
		sandboxClaimCompletionTime.WithLabelValues(namespace, name).Set(float64(claim.Status.CompletionTime.Unix()))
	}

	// sandbox_claim_claimed_replicas
	sandboxClaimClaimedReplicas.WithLabelValues(namespace, name).Set(float64(claim.Status.ClaimedReplicas))

	// sandbox_claim_desired_replicas
	if claim.Spec.Replicas != nil {
		sandboxClaimDesiredReplicas.WithLabelValues(namespace, name).Set(float64(*claim.Spec.Replicas))
	}

	// sandbox_claim_duration_seconds - observe once when claim completes
	if claim.Status.Phase == agentsv1alpha1.SandboxClaimPhaseCompleted &&
		claim.Status.ClaimStartTime != nil && claim.Status.CompletionTime != nil {
		key := namespace + "/" + name
		if _, loaded := observedClaimDurations.LoadOrStore(key, true); !loaded {
			start := claim.Status.ClaimStartTime.Time
			done := claim.Status.CompletionTime.Time
			// Prefer the precise timestamps this process wrote for this claim (keyed
			// by UID so a same-named successor claim cannot contribute its values).
			if t, ok := preciseClaimStartTimes.Load(string(claim.UID)); ok {
				start = t.(time.Time)
			}
			if t, ok := preciseClaimCompletionTimes.Load(string(claim.UID)); ok {
				done = t.(time.Time)
			}
			// Ignore an ambiguous cycle instead of recording a bogus duration, e.g. a
			// claim that completed before this process observed its start after a restart.
			if done.After(start) {
				sandboxClaimClaimDuration.WithLabelValues(namespace).Observe(done.Sub(start).Seconds())
			}
		}
	}
}

// deleteSandboxClaimMetrics removes all metrics for a sandbox claim that has been deleted.
func deleteSandboxClaimMetrics(namespace, name string) {
	sandboxClaimInfo.DeletePartialMatch(prometheus.Labels{"namespace": namespace, "name": name})
	sandboxClaimCreated.DeleteLabelValues(namespace, name)
	for _, phase := range allClaimPhases {
		sandboxClaimStatusPhase.DeleteLabelValues(namespace, name, string(phase))
	}
	sandboxClaimClaimStartTime.DeleteLabelValues(namespace, name)
	sandboxClaimCompletionTime.DeleteLabelValues(namespace, name)
	sandboxClaimClaimedReplicas.DeleteLabelValues(namespace, name)
	sandboxClaimDesiredReplicas.DeleteLabelValues(namespace, name)
	observedClaimDurations.Delete(namespace + "/" + name)
	if uid, ok := preciseClaimUIDs.LoadAndDelete(namespace + "/" + name); ok {
		preciseClaimStartTimes.Delete(uid)
		preciseClaimCompletionTimes.Delete(uid)
	}
}
