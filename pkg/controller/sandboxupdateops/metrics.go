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

package sandboxupdateops

import (
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	updateOpsReplicaTypeTotal    = "total"
	updateOpsReplicaTypeUpdated  = "updated"
	updateOpsReplicaTypeUpdating = "updating"
	updateOpsReplicaTypeFailed   = "failed"

	updateOpsResultCompleted  = "completed"
	updateOpsResultFailed     = "failed"
	updateOpsResultZeroTarget = "zero_target"
)

var (
	sandboxUpdateOpsStatusPhase = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_updateops_status_phase",
			Help: "The current phase of the SandboxUpdateOps (1 for active phase)",
		},
		[]string{"namespace", "name", "phase"},
	)

	sandboxUpdateOpsReplicas = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "sandbox_updateops_replicas",
			Help: "Current SandboxUpdateOps replica counts by type",
		},
		[]string{"namespace", "name", "type"},
	)

	sandboxUpdateOpsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "sandbox_updateops_total",
			Help: "Total number of SandboxUpdateOps terminal results",
		},
		[]string{"namespace", "result"},
	)

	allUpdateOpsPhases = []agentsv1alpha1.SandboxUpdateOpsPhase{
		agentsv1alpha1.SandboxUpdateOpsPending,
		agentsv1alpha1.SandboxUpdateOpsUpdating,
		agentsv1alpha1.SandboxUpdateOpsCompleted,
		agentsv1alpha1.SandboxUpdateOpsFailed,
	}

	observedUpdateOpsResults sync.Map
)

func init() {
	metrics.Registry.MustRegister(
		sandboxUpdateOpsStatusPhase,
		sandboxUpdateOpsReplicas,
		sandboxUpdateOpsTotal,
	)
}

func recordSandboxUpdateOpsMetrics(ops *agentsv1alpha1.SandboxUpdateOps) {
	recordSandboxUpdateOpsStatusMetrics(ops.Namespace, ops.Name, &ops.Status)
	recordSandboxUpdateOpsTerminalResult(ops, &ops.Status)
}

func recordSandboxUpdateOpsStatusMetrics(namespace, name string, status *agentsv1alpha1.SandboxUpdateOpsStatus) {
	if status.Phase != "" {
		for _, phase := range allUpdateOpsPhases {
			if phase != status.Phase {
				sandboxUpdateOpsStatusPhase.DeleteLabelValues(namespace, name, string(phase))
			}
		}
		sandboxUpdateOpsStatusPhase.WithLabelValues(namespace, name, string(status.Phase)).Set(1)
	}

	sandboxUpdateOpsReplicas.WithLabelValues(namespace, name, updateOpsReplicaTypeTotal).Set(float64(status.Replicas))
	sandboxUpdateOpsReplicas.WithLabelValues(namespace, name, updateOpsReplicaTypeUpdated).Set(float64(status.UpdatedReplicas))
	sandboxUpdateOpsReplicas.WithLabelValues(namespace, name, updateOpsReplicaTypeUpdating).Set(float64(status.UpdatingReplicas))
	sandboxUpdateOpsReplicas.WithLabelValues(namespace, name, updateOpsReplicaTypeFailed).Set(float64(status.FailedReplicas))
}

func recordSandboxUpdateOpsTerminalResult(ops *agentsv1alpha1.SandboxUpdateOps, status *agentsv1alpha1.SandboxUpdateOpsStatus) {
	if !isUpdateOpsTerminalPhase(status.Phase) {
		return
	}

	key := updateOpsResultKey(ops)
	if _, loaded := observedUpdateOpsResults.LoadOrStore(key, true); loaded {
		return
	}
	sandboxUpdateOpsTotal.WithLabelValues(ops.Namespace, updateOpsResultFromStatus(status)).Inc()
}

func deleteSandboxUpdateOpsMetrics(ops *agentsv1alpha1.SandboxUpdateOps) {
	deleteSandboxUpdateOpsGaugeMetrics(ops.Namespace, ops.Name)
	observedUpdateOpsResults.Delete(updateOpsResultKey(ops))
}

func deleteSandboxUpdateOpsGaugeMetrics(namespace, name string) {
	for _, phase := range allUpdateOpsPhases {
		sandboxUpdateOpsStatusPhase.DeleteLabelValues(namespace, name, string(phase))
	}
	sandboxUpdateOpsReplicas.DeleteLabelValues(namespace, name, updateOpsReplicaTypeTotal)
	sandboxUpdateOpsReplicas.DeleteLabelValues(namespace, name, updateOpsReplicaTypeUpdated)
	sandboxUpdateOpsReplicas.DeleteLabelValues(namespace, name, updateOpsReplicaTypeUpdating)
	sandboxUpdateOpsReplicas.DeleteLabelValues(namespace, name, updateOpsReplicaTypeFailed)
}

func isUpdateOpsTerminalPhase(phase agentsv1alpha1.SandboxUpdateOpsPhase) bool {
	return phase == agentsv1alpha1.SandboxUpdateOpsCompleted || phase == agentsv1alpha1.SandboxUpdateOpsFailed
}

func updateOpsResultFromStatus(status *agentsv1alpha1.SandboxUpdateOpsStatus) string {
	if status.Phase == agentsv1alpha1.SandboxUpdateOpsFailed {
		return updateOpsResultFailed
	}
	if status.Replicas == 0 {
		return updateOpsResultZeroTarget
	}
	return updateOpsResultCompleted
}

func updateOpsResultKey(ops *agentsv1alpha1.SandboxUpdateOps) string {
	if ops.UID != "" {
		return fmt.Sprintf("%s/%s/%s", ops.Namespace, ops.Name, ops.UID)
	}
	return types.NamespacedName{Namespace: ops.Namespace, Name: ops.Name}.String()
}
