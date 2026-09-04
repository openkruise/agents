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
	"context"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestRecordSandboxUpdateOpsStatusMetrics(t *testing.T) {
	ns, name := "metrics-status", "update-a"
	deleteSandboxUpdateOpsMetrics(&agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	})

	recordSandboxUpdateOpsStatusMetrics(ns, name, &agentsv1alpha1.SandboxUpdateOpsStatus{
		Phase:            agentsv1alpha1.SandboxUpdateOpsPending,
		Replicas:         3,
		UpdatedReplicas:  1,
		UpdatingReplicas: 2,
		FailedReplicas:   0,
	})
	recordSandboxUpdateOpsStatusMetrics(ns, name, &agentsv1alpha1.SandboxUpdateOpsStatus{
		Phase:            agentsv1alpha1.SandboxUpdateOpsUpdating,
		Replicas:         4,
		UpdatedReplicas:  2,
		UpdatingReplicas: 1,
		FailedReplicas:   1,
	})

	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxUpdateOpsPending))))
	assert.Equal(t, float64(1), testutil.ToFloat64(sandboxUpdateOpsStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxUpdateOpsUpdating))))
	assert.Equal(t, float64(4), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeTotal)))
	assert.Equal(t, float64(2), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeUpdated)))
	assert.Equal(t, float64(1), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeUpdating)))
	assert.Equal(t, float64(1), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeFailed)))

	deleteSandboxUpdateOpsMetrics(&agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	})
}

func TestDeleteSandboxUpdateOpsMetrics(t *testing.T) {
	ns, name := "metrics-delete", "update-a"
	recordSandboxUpdateOpsStatusMetrics(ns, name, &agentsv1alpha1.SandboxUpdateOpsStatus{
		Phase:            agentsv1alpha1.SandboxUpdateOpsUpdating,
		Replicas:         5,
		UpdatedReplicas:  3,
		UpdatingReplicas: 1,
		FailedReplicas:   1,
	})

	deleteSandboxUpdateOpsMetrics(&agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
	})

	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxUpdateOpsUpdating))))
	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeTotal)))
	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeUpdated)))
	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeUpdating)))
	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeFailed)))
}

func TestUpdateStatusRecordsTerminalResultOnce(t *testing.T) {
	tests := []struct {
		name      string
		newStatus agentsv1alpha1.SandboxUpdateOpsStatus
		result    string
	}{
		{
			name: "completed",
			newStatus: agentsv1alpha1.SandboxUpdateOpsStatus{
				Phase:           agentsv1alpha1.SandboxUpdateOpsCompleted,
				Replicas:        2,
				UpdatedReplicas: 2,
			},
			result: updateOpsResultCompleted,
		},
		{
			name: "zero target",
			newStatus: agentsv1alpha1.SandboxUpdateOpsStatus{
				Phase: agentsv1alpha1.SandboxUpdateOpsCompleted,
			},
			result: updateOpsResultZeroTarget,
		},
		{
			name: "failed",
			newStatus: agentsv1alpha1.SandboxUpdateOpsStatus{
				Phase:          agentsv1alpha1.SandboxUpdateOpsFailed,
				Replicas:       1,
				FailedReplicas: 1,
			},
			result: updateOpsResultFailed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ns := "metrics-terminal-" + tt.result
			name := "update-a"
			ops := newSandboxUpdateOps(name, ns, agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
			ops.UID = types.UID("uid-" + tt.result)
			observedUpdateOpsResults.Delete(updateOpsResultKey(ops))
			deleteSandboxUpdateOpsMetrics(ops)
			before := testutil.ToFloat64(sandboxUpdateOpsTotal.WithLabelValues(ns, tt.result))

			r := newTestReconciler(ops)
			newStatus := tt.newStatus.DeepCopy()
			err := r.updateStatus(context.Background(), ops, newStatus)
			assert.NoError(t, err)
			err = r.updateStatus(context.Background(), ops, newStatus)
			assert.NoError(t, err)

			assert.Equal(t, before+1, testutil.ToFloat64(sandboxUpdateOpsTotal.WithLabelValues(ns, tt.result)))
			assert.Equal(t, float64(1), testutil.ToFloat64(sandboxUpdateOpsStatusPhase.WithLabelValues(ns, name, string(tt.newStatus.Phase))))
			assert.Equal(t, float64(tt.newStatus.Replicas), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeTotal)))
			observedUpdateOpsResults.Delete(updateOpsResultKey(ops))
			deleteSandboxUpdateOpsMetrics(ops)
		})
	}
}

func TestUpdateStatusRecordsObservedTerminalResultOnce(t *testing.T) {
	ns, name := "metrics-terminal-old", "update-a"
	ops := newSandboxUpdateOps(name, ns, agentsv1alpha1.SandboxUpdateOpsCompleted, false, nil)
	ops.UID = types.UID("uid-terminal-old")
	ops.Status.Replicas = 1
	ops.Status.UpdatedReplicas = 1
	observedUpdateOpsResults.Delete(updateOpsResultKey(ops))
	before := testutil.ToFloat64(sandboxUpdateOpsTotal.WithLabelValues(ns, updateOpsResultCompleted))

	r := newTestReconciler(ops)
	newStatus := ops.Status.DeepCopy()
	newStatus.ObservedGeneration = 2
	err := r.updateStatus(context.Background(), ops, newStatus)
	assert.NoError(t, err)
	err = r.updateStatus(context.Background(), ops, newStatus)

	assert.NoError(t, err)
	assert.Equal(t, before+1, testutil.ToFloat64(sandboxUpdateOpsTotal.WithLabelValues(ns, updateOpsResultCompleted)))
	observedUpdateOpsResults.Delete(updateOpsResultKey(ops))
	deleteSandboxUpdateOpsMetrics(ops)
}

func TestReconcileRecordsObservedTerminalResult(t *testing.T) {
	ns, name := "metrics-observed-terminal", "update-a"
	ops := newSandboxUpdateOps(name, ns, agentsv1alpha1.SandboxUpdateOpsCompleted, false, nil)
	ops.UID = types.UID("uid-observed-terminal")
	ops.Status.Replicas = 1
	ops.Status.UpdatedReplicas = 1
	observedUpdateOpsResults.Delete(updateOpsResultKey(ops))
	deleteSandboxUpdateOpsMetrics(ops)
	before := testutil.ToFloat64(sandboxUpdateOpsTotal.WithLabelValues(ns, updateOpsResultCompleted))

	r := newTestReconciler(ops)
	result, err := r.Reconcile(context.Background(), ctrlRequest(ns, name))
	assert.NoError(t, err)
	assert.Equal(t, ctrlResultZero(), result)
	result, err = r.Reconcile(context.Background(), ctrlRequest(ns, name))
	assert.NoError(t, err)
	assert.Equal(t, ctrlResultZero(), result)

	assert.Equal(t, before+1, testutil.ToFloat64(sandboxUpdateOpsTotal.WithLabelValues(ns, updateOpsResultCompleted)))
	assert.Equal(t, float64(1), testutil.ToFloat64(sandboxUpdateOpsStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxUpdateOpsCompleted))))
	observedUpdateOpsResults.Delete(updateOpsResultKey(ops))
	deleteSandboxUpdateOpsMetrics(ops)
}

func TestHandleDeletionDeletesSandboxUpdateOpsGaugeMetrics(t *testing.T) {
	ns, name := "metrics-handle-delete", "update-a"
	ops := newSandboxUpdateOps(name, ns, agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.UID = types.UID("uid-handle-delete")
	ops.Finalizers = []string{finalizerName}
	observedUpdateOpsResults.Store(updateOpsResultKey(ops), true)
	recordSandboxUpdateOpsStatusMetrics(ns, name, &agentsv1alpha1.SandboxUpdateOpsStatus{
		Phase:            agentsv1alpha1.SandboxUpdateOpsUpdating,
		Replicas:         1,
		UpdatingReplicas: 1,
	})

	r := newTestReconciler(ops)
	err := r.Delete(context.Background(), ops)
	assert.NoError(t, err)

	result, err := r.Reconcile(context.Background(), ctrlRequest(ns, name))

	assert.NoError(t, err)
	assert.Equal(t, ctrlResultZero(), result)
	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxUpdateOpsUpdating))))
	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeTotal)))
	_, ok := observedUpdateOpsResults.Load(updateOpsResultKey(ops))
	assert.False(t, ok)
}

func TestReconcileDeletesSandboxUpdateOpsGaugeMetricsOnNotFound(t *testing.T) {
	ns, name := "metrics-not-found", "update-a"
	ops := newSandboxUpdateOps(name, ns, agentsv1alpha1.SandboxUpdateOpsCompleted, false, nil)
	ops.UID = types.UID("uid-not-found")
	observedUpdateOpsResults.Store(updateOpsResultKey(ops), true)
	recordSandboxUpdateOpsStatusMetrics(ns, name, &agentsv1alpha1.SandboxUpdateOpsStatus{
		Phase:           agentsv1alpha1.SandboxUpdateOpsCompleted,
		Replicas:        2,
		UpdatedReplicas: 2,
	})

	r := newTestReconciler()
	result, err := r.Reconcile(context.Background(), ctrlRequest(ns, name))

	assert.NoError(t, err)
	assert.Equal(t, ctrlResultZero(), result)
	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsStatusPhase.WithLabelValues(ns, name, string(agentsv1alpha1.SandboxUpdateOpsCompleted))))
	assert.Equal(t, float64(0), testutil.ToFloat64(sandboxUpdateOpsReplicas.WithLabelValues(ns, name, updateOpsReplicaTypeTotal)))
	_, ok := observedUpdateOpsResults.Load(updateOpsResultKey(ops))
	assert.True(t, ok)
	observedUpdateOpsResults.Delete(updateOpsResultKey(ops))
}

func ctrlRequest(namespace, name string) ctrl.Request {
	return ctrl.Request{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}
}

func ctrlResultZero() ctrl.Result {
	return ctrl.Result{}
}
