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

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
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

	// claimMetricStates maps a claim's types.NamespacedName to the claimMetricState
	// last recorded for it. Reconciles of one key are serialized by the workqueue,
	// so each entry is only read and written by a single worker at a time.
	claimMetricStates sync.Map
)

// claimMetricState remembers the labels and resourceVersion last recorded for a
// claim, so that recording can skip unchanged claims and deletion can remove
// series by exact labels. DeleteLabelValues always takes the vector's write lock
// and DeletePartialMatch also scans every series, which serializes reconciles
// when there are many claims.
type claimMetricState struct {
	uid             types.UID
	templateName    string
	resourceVersion string
	phase           agentsv1alpha1.SandboxClaimPhase
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
	)
}

// recordSandboxClaimMetrics updates all sandbox claim lifecycle metrics based on the current claim state.
func recordSandboxClaimMetrics(claim *agentsv1alpha1.SandboxClaim) {
	namespace := claim.Namespace
	name := claim.Name
	key := types.NamespacedName{Namespace: namespace, Name: name}

	var prev *claimMetricState
	if v, ok := claimMetricStates.Load(key); ok {
		state := v.(claimMetricState)
		prev = &state
	}
	// Every metric is derived from the claim object, so an unchanged
	// resourceVersion means there is nothing to update.
	if prev != nil && prev.uid == claim.UID && prev.resourceVersion == claim.ResourceVersion {
		return
	}
	// The name was reused by a new claim; drop every series of the old one.
	if prev != nil && prev.uid != claim.UID {
		deleteClaimSeries(namespace, name, *prev)
		prev = nil
	}

	// sandbox_claim_info
	if prev != nil && prev.templateName != claim.Spec.TemplateName {
		sandboxClaimInfo.DeleteLabelValues(namespace, name, prev.templateName, string(prev.uid))
	}
	sandboxClaimInfo.WithLabelValues(namespace, name, claim.Spec.TemplateName, string(claim.UID)).Set(1)

	// sandbox_claim_created
	sandboxClaimCreated.WithLabelValues(namespace, name).Set(float64(claim.CreationTimestamp.Unix()))

	// sandbox_claim_status_phase: Only emit the current phase (value=1); delete the
	// previous phase series only when the phase actually changes.
	currentPhase := claim.Status.Phase
	if prev != nil && prev.phase != "" && prev.phase != currentPhase {
		sandboxClaimStatusPhase.DeleteLabelValues(namespace, name, string(prev.phase))
	}
	if currentPhase != "" {
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

	claimMetricStates.Store(key, claimMetricState{
		uid:             claim.UID,
		templateName:    claim.Spec.TemplateName,
		resourceVersion: claim.ResourceVersion,
		phase:           currentPhase,
	})
}

// deleteSandboxClaimMetrics removes all metrics for a sandbox claim that has been deleted.
func deleteSandboxClaimMetrics(namespace, name string) {
	v, ok := claimMetricStates.LoadAndDelete(types.NamespacedName{Namespace: namespace, Name: name})
	if !ok {
		// Series are only created by recordSandboxClaimMetrics, which always
		// stores a state, so this process holds no series for the claim.
		return
	}
	deleteClaimSeries(namespace, name, v.(claimMetricState))
}

// deleteClaimSeries removes the series recorded for state by exact label values.
func deleteClaimSeries(namespace, name string, state claimMetricState) {
	sandboxClaimInfo.DeleteLabelValues(namespace, name, state.templateName, string(state.uid))
	sandboxClaimCreated.DeleteLabelValues(namespace, name)
	if state.phase != "" {
		sandboxClaimStatusPhase.DeleteLabelValues(namespace, name, string(state.phase))
	}
	sandboxClaimClaimStartTime.DeleteLabelValues(namespace, name)
	sandboxClaimCompletionTime.DeleteLabelValues(namespace, name)
	sandboxClaimClaimedReplicas.DeleteLabelValues(namespace, name)
	sandboxClaimDesiredReplicas.DeleteLabelValues(namespace, name)
}
