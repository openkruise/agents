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

package sandboxupdateops

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

var testScheme *runtime.Scheme

func init() {
	testScheme = runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(testScheme)
	_ = corev1.AddToScheme(testScheme)
}

func newTestReconciler(objs ...runtime.Object) *Reconciler {
	clientObjs := make([]runtime.Object, 0, len(objs))
	statusObjs := make([]runtime.Object, 0)
	for _, o := range objs {
		clientObjs = append(clientObjs, o)
		switch o.(type) {
		case *agentsv1alpha1.SandboxUpdateOps:
			statusObjs = append(statusObjs, o)
		case *agentsv1alpha1.Sandbox:
			statusObjs = append(statusObjs, o)
		}
	}
	builder := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}, &agentsv1alpha1.Sandbox{}).
		WithRuntimeObjects(clientObjs...)
	fakeClient := builder.Build()
	return &Reconciler{
		Client:   fakeClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(100),
	}
}

func newSandboxUpdateOps(name, ns string, phase agentsv1alpha1.SandboxUpdateOpsPhase, paused bool, maxUnavailable *intstrutil.IntOrString) *agentsv1alpha1.SandboxUpdateOps {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			Paused: paused,
			UpdateStrategy: agentsv1alpha1.SandboxUpdateOpsStrategy{
				MaxUnavailable: maxUnavailable,
			},
		},
		Status: agentsv1alpha1.SandboxUpdateOpsStatus{
			Phase: phase,
		},
	}
	return ops
}

func newSandbox(name, ns, opsName string, phase agentsv1alpha1.SandboxPhase, conditions []metav1.Condition) *agentsv1alpha1.Sandbox {
	labels := map[string]string{
		"app":                                "test",
		agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
	}
	if opsName != "" {
		labels[agentsv1alpha1.LabelSandboxUpdateOps] = opsName
	}
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "busybox:1.0"},
						},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:      phase,
			Conditions: conditions,
		},
	}
}

func TestReconcile_NotFound(t *testing.T) {
	r := newTestReconciler()
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "not-exist", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestReconcile_TerminalPhase(t *testing.T) {
	tests := []struct {
		name  string
		phase agentsv1alpha1.SandboxUpdateOpsPhase
	}{
		{name: "Completed phase skips", phase: agentsv1alpha1.SandboxUpdateOpsCompleted},
		{name: "Failed phase skips", phase: agentsv1alpha1.SandboxUpdateOpsFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newSandboxUpdateOps("test-ops", "default", tt.phase, false, nil)
			r := newTestReconciler(ops)
			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
			})
			assert.NoError(t, err)
			assert.Equal(t, ctrl.Result{}, result)
		})
	}
}

func TestReconcile_PendingToUpdating(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, nil)
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	// Verify status was updated to Updating
	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsUpdating, updatedOps.Status.Phase)
}

func TestReconcile_EmptyPhaseToUpdating(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", "", false, nil)
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsUpdating, updatedOps.Status.Phase)
}

func TestReconcile_UpdatingAppliesPatchToCandidates(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	// Candidate sandbox (no ops label)
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	// Verify sandbox was patched with tracking label
	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Equal(t, "test-ops", updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
	// Verify image was updated
	assert.Equal(t, "busybox:2.0", updatedSbx.Spec.Template.Spec.Containers[0].Image)
	// Verify UpgradePolicy was set
	assert.NotNil(t, updatedSbx.Spec.UpgradePolicy)
	assert.Equal(t, agentsv1alpha1.SandboxUpgradePolicyRecreate, updatedSbx.Spec.UpgradePolicy.Type)
}

func TestReconcile_AllCompletedToCompleted(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	// All sandboxes are updated (ops label + Upgrading Condition Succeeded)
	sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
	})
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
}

func TestReconcile_SkipsSandboxWhoseTemplateAlreadyMatchesPatch(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	// sbx-1: normally upgraded (ops label + Upgrading Succeeded)
	sbx1 := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
	})
	sbx1.Spec.Template.Spec.Containers[0].Image = "busybox:2.0"

	// sbx-2: candidate whose template already matches the patch target — should be skipped entirely
	sbx2 := newSandbox("sbx-2", "default", "", agentsv1alpha1.SandboxRunning, nil)
	sbx2.Spec.Template.Spec.Containers[0].Image = "busybox:2.0"

	// sbx-3: already labeled by ops, Running, no condition, template matches (stuck scenario) — should be skipped
	sbx3 := newSandbox("sbx-3", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)
	sbx3.Spec.Template.Spec.Containers[0].Image = "busybox:2.0"

	r := newTestReconciler(ops, sbx1, sbx2, sbx3)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	// sbx-2 (candidate, template matches) and sbx-3 (stuck updating, template matches) are both skipped.
	// Only sbx-1 (genuinely updated) is counted.
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
	assert.Equal(t, int32(1), updatedOps.Status.Replicas)
	assert.Equal(t, int32(1), updatedOps.Status.UpdatedReplicas)
	assert.Equal(t, int32(0), updatedOps.Status.UpdatingReplicas)

	// sbx-2 should NOT have been patched (no ops label)
	updatedSbx2 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-2", Namespace: "default"}, updatedSbx2)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx2.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
}

func TestReconcile_UpdatingNotSkippedWhenGenerationMismatch(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	// ops label, template matches, but generation mismatch — genuinely updating
	sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)
	sbx.Spec.Template.Spec.Containers[0].Image = "busybox:2.0"
	sbx.Generation = 2
	sbx.Status.ObservedGeneration = 1
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), updatedOps.Status.UpdatingReplicas, "generation mismatch should count as updating")
}

func TestReconcile_UpdatingNotSkippedWhenConditionExists(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	// ops label, template matches, gen matches, but has Upgrading condition → classified as failed, not skipped
	sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, Status: metav1.ConditionFalse},
	})
	sbx.Spec.Template.Spec.Containers[0].Image = "busybox:2.0"
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), updatedOps.Status.FailedReplicas, "condition present should classify as failed, not skip")
	assert.Equal(t, int32(0), updatedOps.Status.UpdatingReplicas)
}

func TestReconcile_FailedSandboxLeadsToFailed(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	// All sandboxes done: one failed (via condition), one succeeded
	sbx1 := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, Status: metav1.ConditionFalse},
	})
	sbx2 := newSandbox("sbx-2", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
	})
	r := newTestReconciler(ops, sbx1, sbx2)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsFailed, updatedOps.Status.Phase)
}

func TestReconcile_PausedDoesNotStartNewUpgrades(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, true, nil)
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	// Sandbox should NOT have the ops label (not patched)
	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
}

func TestClassifySandbox(t *testing.T) {
	opsName := "test-ops"
	// ops with a patch that changes image to busybox:2.0
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: opsName},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
				},
			}),
		},
	}
	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected sandboxUpdateState
	}{
		{
			name: "no ops label, template differs, Running -> candidate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			},
			expected: sandboxCandidate,
		},
		{
			name: "no ops label, template matches, Running -> noNeedUpdate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			},
			expected: sandboxNoNeedUpdate,
		},
		{
			name: "no ops label, template matches, Running, generation mismatch -> candidate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}, Generation: 2},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning, ObservedGeneration: 1},
			},
			expected: sandboxCandidate,
		},
		{
			name: "no ops label, template matches, Upgrading -> candidate (recovery scenario)",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxUpgrading},
			},
			expected: sandboxCandidate,
		},
		{
			name: "different ops label, template differs, Running -> candidate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					agentsv1alpha1.LabelSandboxUpdateOps: "other-ops",
				}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			},
			expected: sandboxCandidate,
		},
		{
			name: "no ops label, template differs, Paused -> noNeedUpdate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPaused},
			},
			expected: sandboxNoNeedUpdate,
		},
		{
			name: "no ops label, template differs, Resuming -> noNeedUpdate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxResuming},
			},
			expected: sandboxNoNeedUpdate,
		},
		{
			name: "no ops label, template differs, Pending -> noNeedUpdate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
			},
			expected: sandboxNoNeedUpdate,
		},
		{
			name: "ops label + generation mismatch -> updating",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Generation: 2,
					Labels:     map[string]string{agentsv1alpha1.LabelSandboxUpdateOps: opsName},
				},
				Status: agentsv1alpha1.SandboxStatus{ObservedGeneration: 1},
			},
			expected: sandboxUpdating,
		},
		{
			name: "ops label + Succeeded condition -> updated",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					agentsv1alpha1.LabelSandboxUpdateOps: opsName,
				}},
				Status: agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
					},
				},
			},
			expected: sandboxUpdated,
		},
		{
			name: "ops label + PreUpgradeFailed condition -> failed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					agentsv1alpha1.LabelSandboxUpdateOps: opsName,
				}},
				Status: agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, Status: metav1.ConditionFalse},
					},
				},
			},
			expected: sandboxFailed,
		},
		{
			name: "ops label + no condition + template matches -> noNeedUpdate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					agentsv1alpha1.LabelSandboxUpdateOps: opsName,
				}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
							},
						},
					},
				},
			},
			expected: sandboxNoNeedUpdate,
		},
		{
			name: "ops label + no condition + template differs -> updating",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					agentsv1alpha1.LabelSandboxUpdateOps: opsName,
				}},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
			},
			expected: sandboxUpdating,
		},
		{
			name: "ops label + Pending phase (no condition, no template) -> updating (intermediate)",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					agentsv1alpha1.LabelSandboxUpdateOps: opsName,
				}},
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
			},
			expected: sandboxUpdating,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newTestReconciler()
			result := r.classifySandbox(context.Background(), tt.sandbox, ops)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestCalculateMaxUnavailable(t *testing.T) {
	tests := []struct {
		name           string
		maxUnavailable *intstrutil.IntOrString
		total          int32
		expected       int32
	}{
		{
			name:           "nil defaults to 1",
			maxUnavailable: nil,
			total:          10,
			expected:       1,
		},
		{
			name:           "absolute value 3",
			maxUnavailable: intOrStringPtr(intstrutil.FromInt32(3)),
			total:          10,
			expected:       3,
		},
		{
			name:           "percentage 50%",
			maxUnavailable: intOrStringPtr(intstrutil.FromString("50%")),
			total:          10,
			expected:       5,
		},
		{
			name:           "percentage rounds up",
			maxUnavailable: intOrStringPtr(intstrutil.FromString("30%")),
			total:          10,
			expected:       3,
		},
		{
			name:           "zero value defaults to 1",
			maxUnavailable: intOrStringPtr(intstrutil.FromInt32(0)),
			total:          10,
			expected:       1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := calculateMaxUnavailable(tt.maxUnavailable, tt.total)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestReconcile_InvalidLabelSelector(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ops",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Selector: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{
						Key:      "app",
						Operator: metav1.LabelSelectorOperator("InvalidOp"),
						Values:   []string{"test"},
					},
				},
			},
			UpdateStrategy: agentsv1alpha1.SandboxUpdateOpsStrategy{},
		},
		Status: agentsv1alpha1.SandboxUpdateOpsStatus{
			Phase: agentsv1alpha1.SandboxUpdateOpsUpdating,
		},
	}
	r := newTestReconciler(ops)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.Error(t, err)
}

func TestReconcile_SkipsDeletedAndTerminalSandboxes(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	// sbx1: Succeeded phase -> skipped
	sbx1 := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxSucceeded, nil)
	// sbx2: Failed phase -> skipped
	sbx2 := newSandbox("sbx-2", "default", "test-ops", agentsv1alpha1.SandboxFailed, nil)
	// sbx3: updated sandbox
	sbx3 := newSandbox("sbx-3", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
	})
	r := newTestReconciler(ops, sbx1, sbx2, sbx3)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	// sbx1 (Succeeded) and sbx2 (Failed) are skipped; only sbx3 remains (updated=1, total=1).
	// updated+failed == total -> transitions to Completed
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
}

func TestReconcile_MaxUnavailableLimitsConcurrency(t *testing.T) {
	maxUnavail := intstrutil.FromInt32(1)
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, &maxUnavail)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	// 1 sandbox already updating (Running phase, has ops label, generation mismatch), 2 candidates
	sbxUpdating := newSandbox("sbx-updating", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)
	sbxUpdating.Generation = 2
	sbxUpdating.Status.ObservedGeneration = 1
	sbxCandidate1 := newSandbox("sbx-candidate-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	sbxCandidate2 := newSandbox("sbx-candidate-2", "default", "", agentsv1alpha1.SandboxRunning, nil)
	r := newTestReconciler(ops, sbxUpdating, sbxCandidate1, sbxCandidate2)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	// With maxUnavailable=1 and 1 already updating, toUpgrade=0, so no candidates should be patched
	updatedSbx1 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-candidate-1", Namespace: "default"}, updatedSbx1)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx1.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
}

func TestClassifySandbox_FailedReasons(t *testing.T) {
	opsName := "test-ops"
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: opsName},
	}
	tests := []struct {
		name   string
		reason string
	}{
		{name: "PostUpgradeFailed", reason: agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed},
		{name: "UpgradePodFailed", reason: agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxUpdateOps: opsName,
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionUpgrading),
							Reason: tt.reason,
							Status: metav1.ConditionFalse,
						},
					},
				},
			}
			r := newTestReconciler()
			result := r.classifySandbox(context.Background(), sbx, ops)
			assert.Equal(t, sandboxFailed, result)
		})
	}
}

func TestClassifySandbox_OtherOpsLabel(t *testing.T) {
	opsName := "test-ops"
	otherOpsName := "other-ops"
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: opsName, Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
				},
			}),
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{agentsv1alpha1.LabelSandboxUpdateOps: otherOpsName},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
	}
	tests := []struct {
		name          string
		otherOpsPhase agentsv1alpha1.SandboxUpdateOpsPhase
		expected      sandboxUpdateState
	}{
		{
			name:          "other ops Pending -> noNeedUpdate",
			otherOpsPhase: agentsv1alpha1.SandboxUpdateOpsPending,
			expected:      sandboxNoNeedUpdate,
		},
		{
			name:          "other ops Updating -> noNeedUpdate",
			otherOpsPhase: agentsv1alpha1.SandboxUpdateOpsUpdating,
			expected:      sandboxNoNeedUpdate,
		},
		{
			name:          "other ops Completed -> candidate",
			otherOpsPhase: agentsv1alpha1.SandboxUpdateOpsCompleted,
			expected:      sandboxCandidate,
		},
		{
			name:          "other ops Failed -> candidate",
			otherOpsPhase: agentsv1alpha1.SandboxUpdateOpsFailed,
			expected:      sandboxCandidate,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			otherOps := &agentsv1alpha1.SandboxUpdateOps{
				ObjectMeta: metav1.ObjectMeta{Name: otherOpsName, Namespace: "default"},
				Status:     agentsv1alpha1.SandboxUpdateOpsStatus{Phase: tt.otherOpsPhase},
			}
			r := newTestReconciler(otherOps)
			result := r.classifySandbox(context.Background(), sbx, ops)
			assert.Equal(t, tt.expected, result)
		})
	}

	t.Run("other ops not found -> candidate", func(t *testing.T) {
		r := newTestReconciler()
		result := r.classifySandbox(context.Background(), sbx, ops)
		assert.Equal(t, sandboxCandidate, result)
	})
}

func TestIsSandboxTemplateMatchPatch(t *testing.T) {
	tests := []struct {
		name     string
		sbxImage string
		patch    corev1.PodTemplateSpec
		expected bool
	}{
		{
			name:     "template matches patch",
			sbxImage: "busybox:2.0",
			patch: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
				},
			},
			expected: true,
		},
		{
			name:     "template differs from patch",
			sbxImage: "busybox:1.0",
			patch: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
				},
			},
			expected: false,
		},
		{
			name:     "nil template returns false",
			sbxImage: "",
			patch: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
				},
			},
			expected: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := &agentsv1alpha1.SandboxUpdateOps{
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					Patch: mustMarshalPatch(tt.patch),
				},
			}
			sbx := &agentsv1alpha1.Sandbox{}
			if tt.sbxImage != "" {
				sbx.Spec.Template = &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: tt.sbxImage}},
					},
				}
			}
			assert.Equal(t, tt.expected, isSandboxTemplateMatchPatch(sbx, ops))
		})
	}

	t.Run("empty patch returns false", func(t *testing.T) {
		ops := &agentsv1alpha1.SandboxUpdateOps{}
		sbx := &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
						},
					},
				},
			},
		}
		assert.False(t, isSandboxTemplateMatchPatch(sbx, ops))
	})

	t.Run("partial patch matches subset of fields", func(t *testing.T) {
		ops := &agentsv1alpha1.SandboxUpdateOps{
			Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
				Patch: mustMarshalPatch(corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
					},
				}),
			},
		}
		sbx := &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "main",
								Image: "busybox:2.0",
								Env:   []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
							}},
						},
					},
				},
			},
		}
		assert.True(t, isSandboxTemplateMatchPatch(sbx, ops))
	})

	t.Run("invalid patch JSON returns false", func(t *testing.T) {
		ops := &agentsv1alpha1.SandboxUpdateOps{
			Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
				Patch: runtime.RawExtension{Raw: []byte(`not-valid-json`)},
			},
		}
		sbx := &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
						},
					},
				},
			},
		}
		assert.False(t, isSandboxTemplateMatchPatch(sbx, ops))
	})

	t.Run("partial patch differs", func(t *testing.T) {
		ops := &agentsv1alpha1.SandboxUpdateOps{
			Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
				Patch: mustMarshalPatch(corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
					},
				}),
			},
		}
		sbx := &agentsv1alpha1.Sandbox{
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{{
								Name:  "main",
								Image: "busybox:1.0",
								Env:   []corev1.EnvVar{{Name: "FOO", Value: "bar"}},
							}},
						},
					},
				},
			},
		}
		assert.False(t, isSandboxTemplateMatchPatch(sbx, ops))
	})
}

func intOrStringPtr(v intstrutil.IntOrString) *intstrutil.IntOrString {
	return &v
}

func mustMarshalPatch(tmpl corev1.PodTemplateSpec) runtime.RawExtension {
	data, err := json.Marshal(tmpl)
	if err != nil {
		panic(err)
	}
	return runtime.RawExtension{Raw: data}
}

func TestReconcile_SkipsSandboxSetControlledSandboxes(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})

	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sbs",
			Namespace: "default",
			UID:       types.UID("sbs-uid"),
		},
	}

	// sbx-pool: controlled by SandboxSet, should be skipped
	sbxPool := newSandbox("sbx-pool", "default", "", agentsv1alpha1.SandboxRunning, nil)
	sbxPool.OwnerReferences = []metav1.OwnerReference{
		*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind),
	}

	// sbx-standalone: not controlled by SandboxSet, should be processed
	sbxStandalone := newSandbox("sbx-standalone", "default", "", agentsv1alpha1.SandboxRunning, nil)
	// Remove the claimed label to verify it's no longer required
	delete(sbxStandalone.Labels, agentsv1alpha1.LabelSandboxIsClaimed)

	r := newTestReconciler(ops, sbxPool, sbxStandalone)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	// sbx-pool should NOT have been patched (still no ops label)
	updatedPool := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-pool", Namespace: "default"}, updatedPool)
	assert.NoError(t, err)
	assert.Empty(t, updatedPool.Labels[agentsv1alpha1.LabelSandboxUpdateOps],
		"pool sandbox should not be patched")

	// sbx-standalone should have been patched
	updatedStandalone := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-standalone", Namespace: "default"}, updatedStandalone)
	assert.NoError(t, err)
	assert.Equal(t, "test-ops", updatedStandalone.Labels[agentsv1alpha1.LabelSandboxUpdateOps],
		"standalone sandbox should be patched")

	// Verify status: only sbx-standalone counted (total=1)
	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), updatedOps.Status.Replicas, "only non-SandboxSet-controlled sandboxes should be counted")
}

func TestReconcile_ProcessesSandboxWithoutClaimedLabel(t *testing.T) {
	// Verify that sandboxes without the claimed label are still processed
	// as long as they are not controlled by a SandboxSet
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})

	sbx := newSandbox("sbx-no-claim", "default", "", agentsv1alpha1.SandboxRunning, nil)
	delete(sbx.Labels, agentsv1alpha1.LabelSandboxIsClaimed)

	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-no-claim", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Equal(t, "test-ops", updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps],
		"sandbox without claimed label should still be patched")
}

func TestReconcile_StatusUnchangedSkipsUpdate(t *testing.T) {
	// When status doesn't change, updateStatus should be a no-op
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsCompleted, false, nil)
	r := newTestReconciler(ops)

	// Terminal phase returns early, no status update needed
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestReconcile_PatchWithLifecycle(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	ops.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
		PreUpgrade:  &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"echo", "pre"}}},
		PostUpgrade: &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"echo", "post"}}},
	}
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Equal(t, "test-ops", updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
	assert.NotNil(t, updatedSbx.Spec.Lifecycle)
	assert.NotNil(t, updatedSbx.Spec.Lifecycle.PreUpgrade)
	assert.NotNil(t, updatedSbx.Spec.Lifecycle.PostUpgrade)
}

func TestReconcile_PatchWithNilLifecycleClearsExisting(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	// ops.Spec.Lifecycle is nil
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	// Give sandbox an existing lifecycle
	sbx.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
		PreUpgrade: &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"echo", "old"}}},
	}
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Nil(t, updatedSbx.Spec.Lifecycle)
}

// --- handleDeletion tests ---

func TestHandleDeletion_Success(t *testing.T) {
	// Create ops with finalizer and DeletionTimestamp
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}

	// Create 2 sandboxes with ops label
	sbx1 := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)
	sbx2 := newSandbox("sbx-2", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)

	r := newTestReconciler(ops, sbx1, sbx2)

	// Delete the ops to set DeletionTimestamp (fake client sets it when finalizers present)
	err := r.Delete(context.Background(), ops)
	assert.NoError(t, err)

	// Reconcile should call handleDeletion
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify sandbox labels were removed
	updatedSbx1 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx1)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx1.Labels[agentsv1alpha1.LabelSandboxUpdateOps])

	updatedSbx2 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-2", Namespace: "default"}, updatedSbx2)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx2.Labels[agentsv1alpha1.LabelSandboxUpdateOps])

	// After finalizer removal, fake client fully deletes the object
	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.True(t, errors.IsNotFound(err), "ops should be fully deleted after finalizer removal")
}

func TestHandleDeletion_NoFinalizer(t *testing.T) {
	// Create ops without finalizer
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	// No finalizers set

	r := newTestReconciler(ops)

	// Delete the ops - without finalizer, fake client may fully delete it
	err := r.Delete(context.Background(), ops)
	assert.NoError(t, err)

	// Reconcile should return no error (either not found or handleDeletion returns early)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)
}

func TestHandleDeletion_RemoveLabelFromMultipleSandboxes(t *testing.T) {
	// Create ops with finalizer
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}

	// Create 3 sandboxes with ops label
	sbx1 := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)
	sbx2 := newSandbox("sbx-2", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)
	sbx3 := newSandbox("sbx-3", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)

	r := newTestReconciler(ops, sbx1, sbx2, sbx3)

	// Delete the ops to set DeletionTimestamp
	err := r.Delete(context.Background(), ops)
	assert.NoError(t, err)

	// Reconcile
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify all 3 sandboxes had their labels removed
	for _, name := range []string{"sbx-1", "sbx-2", "sbx-3"} {
		updatedSbx := &agentsv1alpha1.Sandbox{}
		err = r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, updatedSbx)
		assert.NoError(t, err)
		assert.Empty(t, updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps],
			"sandbox %s should have ops label removed", name)
	}

	// After finalizer removal, fake client fully deletes the object
	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.True(t, errors.IsNotFound(err), "ops should be fully deleted after finalizer removal")
}

func TestReconcile_DeletionTimestamp_CallsHandleDeletion(t *testing.T) {
	// Create ops with finalizer and a sandbox with ops label
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}
	sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)

	r := newTestReconciler(ops, sbx)

	// Delete ops to set DeletionTimestamp
	err := r.Delete(context.Background(), ops)
	assert.NoError(t, err)

	// Reconcile should handle deletion
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify sandbox label was cleaned up
	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps])

	// After finalizer removal, fake client fully deletes the object
	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.True(t, errors.IsNotFound(err), "ops should be fully deleted after finalizer removal")
}

func TestReconcile_ConcurrentOpsInNamespace(t *testing.T) {
	// First ops is actively Updating
	ops1 := newSandboxUpdateOps("ops-active", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	// Second ops is Pending (should be blocked)
	ops2 := newSandboxUpdateOps("ops-pending", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, nil)
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)

	r := newTestReconciler(ops1, ops2, sbx)

	// Reconcile the second ops
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "ops-pending", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.Equal(t, ctrl.Result{}, result)

	// Verify ops-pending was NOT transitioned (still Pending, no status update)
	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "ops-pending", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsPending, updatedOps.Status.Phase)

	// Verify sandbox was not patched
	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
}

func TestSandboxUpdateStateString_Unknown(t *testing.T) {
	unknownState := sandboxUpdateState(99)
	assert.Equal(t, "Unknown", unknownState.String())
}

func TestReconcile_ConcurrentOpsListError(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, nil)

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}).
		WithRuntimeObjects(ops).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*agentsv1alpha1.SandboxUpdateOpsList); ok {
					return fmt.Errorf("simulated ops list error")
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	r := &Reconciler{Client: fakeClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(10)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated ops list error")
}

func TestReconcile_FinalizerAddError(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, nil)

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}).
		WithRuntimeObjects(ops).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return fmt.Errorf("simulated update error")
			},
		}).
		Build()

	r := &Reconciler{Client: fakeClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(10)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated update error")
}

func TestReconcile_SandboxListError(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}).
		WithRuntimeObjects(ops).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*agentsv1alpha1.SandboxList); ok {
					return fmt.Errorf("simulated sandbox list error")
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	r := &Reconciler{Client: fakeClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(10)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated sandbox list error")
}

func TestReconcile_PatchErrorDuringUpgrade(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
		},
	})
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)

	patchCallCount := 0
	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}, &agentsv1alpha1.Sandbox{}).
		WithRuntimeObjects(ops, sbx).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCallCount++
				if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
					return fmt.Errorf("simulated patch error")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()

	r := &Reconciler{Client: fakeClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(10)}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated patch error")
}

func TestReconcile_ToUpgradeCappedByCandidateCount(t *testing.T) {
	maxUnavail := intstrutil.FromInt32(10)
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, &maxUnavail)
	ops.Finalizers = []string{finalizerName}
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
		},
	})
	sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Equal(t, "test-ops", updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
}

func TestClassifySandboxes_ExpectationNotSatisfiedRequeues(t *testing.T) {
	origTimeout := expectations.ExpectationTimeout
	defer func() { expectations.ExpectationTimeout = origTimeout }()
	expectations.ExpectationTimeout = 10 * time.Minute

	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}
	sbx := newSandbox("sbx-requeue", "default", "", agentsv1alpha1.SandboxRunning, nil)
	sbx.UID = types.UID("requeue-test-uid")
	sbx.ResourceVersion = "1"

	expectSbx := sbx.DeepCopy()
	expectSbx.ResourceVersion = "99999"
	ResourceVersionExpectations.Expect(expectSbx)
	defer ResourceVersionExpectations.Delete(sbx)

	r := newTestReconciler(ops, sbx)
	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)
	assert.NotZero(t, result.RequeueAfter, "should requeue when expectation is not satisfied and not timed out")
}

// --- classifySandboxes expectation timeout tests ---

func TestClassifySandboxes_ExpectationTimeout(t *testing.T) {
	// When ResourceVersionExpectations is unsatisfied but timed out,
	// it should delete the expectation and continue classifying (no requeue).
	origTimeout := expectations.ExpectationTimeout
	defer func() { expectations.ExpectationTimeout = origTimeout }()
	expectations.ExpectationTimeout = 0

	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	sbx := newSandbox("sbx-timeout", "default", "", agentsv1alpha1.SandboxRunning, nil)
	sbx.UID = types.UID("timeout-test-uid")
	sbx.ResourceVersion = "1"

	// Set up an expectation with a much higher resource version so it won't be satisfied
	expectSbx := sbx.DeepCopy()
	expectSbx.ResourceVersion = "99999"
	ResourceVersionExpectations.Expect(expectSbx)
	defer ResourceVersionExpectations.Delete(sbx)

	r := newTestReconciler(ops, sbx)

	result, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)
	// Should NOT have RequeueAfter (timeout path was taken, not the requeue path)
	assert.Zero(t, result.RequeueAfter, "should not requeue when expectation times out")

	// Verify sandbox was classified and counted (should be a candidate -> patched)
	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), updatedOps.Status.Replicas, "sandbox should be counted after timeout")
}

// --- handleDeletion error path tests ---

func TestHandleDeletion_ListError(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}, &agentsv1alpha1.Sandbox{}).
		WithRuntimeObjects(ops).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*agentsv1alpha1.SandboxList); ok {
					return fmt.Errorf("simulated list error")
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()

	r := &Reconciler{
		Client:   fakeClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(100),
	}

	_, err := r.handleDeletion(context.Background(), ops)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated list error")
}

func TestHandleDeletion_PatchLabelRemovalError(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}
	sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}, &agentsv1alpha1.Sandbox{}).
		WithRuntimeObjects(ops, sbx).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return fmt.Errorf("simulated patch error")
			},
		}).
		Build()

	r := &Reconciler{
		Client:   fakeClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(100),
	}

	_, err := r.handleDeletion(context.Background(), ops)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated patch error")
}

func TestHandleDeletion_UpdateFinalizerRemovalError(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Finalizers = []string{finalizerName}
	// No sandboxes with ops label, so the patch loop is skipped and we go straight to finalizer removal

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}, &agentsv1alpha1.Sandbox{}).
		WithRuntimeObjects(ops).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				return fmt.Errorf("simulated update error")
			},
		}).
		Build()

	r := &Reconciler{
		Client:   fakeClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(100),
	}

	_, err := r.handleDeletion(context.Background(), ops)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated update error")
}

func TestUpdateStatus_NoChange(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Status.Replicas = 5
	ops.Status.UpdatedReplicas = 3
	ops.Status.FailedReplicas = 0
	ops.Status.UpdatingReplicas = 2

	r := newTestReconciler(ops)

	// Call updateStatus with the same status
	newStatus := ops.Status.DeepCopy()
	err := r.updateStatus(context.Background(), ops, newStatus)
	assert.NoError(t, err)

	// Verify status remained unchanged
	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, ops.Status.Phase, updatedOps.Status.Phase)
	assert.Equal(t, ops.Status.Replicas, updatedOps.Status.Replicas)
}

func TestUpdateStatus_RecordsPhaseEvent(t *testing.T) {
	tests := []struct {
		name           string
		oldPhase       agentsv1alpha1.SandboxUpdateOpsPhase
		newPhase       agentsv1alpha1.SandboxUpdateOpsPhase
		expectEvent    bool
		expectPrefix   string
		expectContains string
	}{
		{
			name:           "records normal event when phase changes to updating",
			oldPhase:       agentsv1alpha1.SandboxUpdateOpsPending,
			newPhase:       agentsv1alpha1.SandboxUpdateOpsUpdating,
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeNormal + " " + eventReasonSandboxUpdateOpsPhaseChanged,
			expectContains: "phase changed from Pending to Updating",
		},
		{
			name:           "records warning event when phase changes to failed",
			oldPhase:       agentsv1alpha1.SandboxUpdateOpsUpdating,
			newPhase:       agentsv1alpha1.SandboxUpdateOpsFailed,
			expectEvent:    true,
			expectPrefix:   corev1.EventTypeWarning + " " + eventReasonSandboxUpdateOpsPhaseChanged,
			expectContains: "failed=1",
		},
		{
			name:        "does not record event when phase does not change",
			oldPhase:    agentsv1alpha1.SandboxUpdateOpsUpdating,
			newPhase:    agentsv1alpha1.SandboxUpdateOpsUpdating,
			expectEvent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newSandboxUpdateOps("test-ops", "default", tt.oldPhase, false, nil)
			ops.Status.Replicas = 3
			ops.Status.UpdatedReplicas = 1
			ops.Status.UpdatingReplicas = 2
			recorder := record.NewFakeRecorder(10)
			fakeClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}).
				WithRuntimeObjects(ops).
				Build()
			r := &Reconciler{
				Client:   fakeClient,
				Scheme:   testScheme,
				Recorder: recorder,
			}
			newStatus := ops.Status.DeepCopy()
			newStatus.Phase = tt.newPhase
			if tt.newPhase == agentsv1alpha1.SandboxUpdateOpsFailed {
				newStatus.FailedReplicas = 1
				newStatus.UpdatingReplicas = 0
			} else {
				newStatus.UpdatedReplicas = 2
			}

			err := r.updateStatus(context.Background(), ops, newStatus)

			assert.NoError(t, err)
			if tt.expectEvent {
				assertUpdateOpsRecorderEvent(t, recorder, tt.expectPrefix, tt.expectContains)
			} else {
				assertNoUpdateOpsRecorderEvent(t, recorder)
			}
		})
	}
}

func assertUpdateOpsRecorderEvent(t *testing.T, recorder *record.FakeRecorder, expectPrefix, expectContains string) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, expectPrefix)
		assert.Contains(t, event, expectContains)
	default:
		t.Fatalf("expected event %q, got none", expectPrefix)
	}
}

func assertNoUpdateOpsRecorderEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("unexpected event: %s", event)
	default:
	}
}
