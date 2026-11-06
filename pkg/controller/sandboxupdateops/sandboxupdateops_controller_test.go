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
	"github.com/stretchr/testify/require"
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
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
		},
	})
	// All sandboxes are updated (ops label + Upgrading Condition Succeeded)
	sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
	})
	// The upgrade policy was written by the ops and must be cleared once the
	// upgrade succeeds.
	sbx.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}
	r := newTestReconciler(ops, sbx)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
	assert.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)

	// The sandbox upgrade policy must be cleared on the success path.
	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Nil(t, updatedSbx.Spec.UpgradePolicy)
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

	// sbx-3: already labeled by ops, Running, no condition, template matches
	// (in-place update applied without Upgrading condition) — skipped as NoNeedUpdate
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
	// sbx-2 (candidate, template matches) is skipped; sbx-3 (ops-labeled,
	// every declared change already applied without the upgrade flow) is
	// counted as updated alongside sbx-1.
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
	assert.Equal(t, int32(2), updatedOps.Status.Replicas)
	assert.Equal(t, int32(2), updatedOps.Status.UpdatedReplicas)
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
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
		},
	})
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
			name: "no ops label, matching template, unobserved generation -> noNeedUpdate",
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
			expected: sandboxNoNeedUpdate,
		},
		{
			name: "no ops label, matching template, unfinished upgrade -> candidate",
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
			name: "no ops label, template differs, Paused -> noNeedUpdate (not fully paused)",
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
			name: "ops label + no condition + changes applied -> updated",
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
			expected: sandboxUpdated,
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
			name: "ops label + Paused phase + no condition + changes applied -> updated",
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
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPaused},
			},
			expected: sandboxUpdated,
		},
		{
			name: "ops label + Paused phase + no condition + changes applied (Paused=True) -> updated",
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue},
					},
				},
			},
			expected: sandboxUpdated,
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
		{
			name: "ops label + ResumeSucceed reason -> resumeSucceed",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					agentsv1alpha1.LabelSandboxUpdateOps: opsName,
				}},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxUpgrading,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonResumeSucceed, Status: metav1.ConditionFalse},
					},
				},
			},
			expected: sandboxResumeSucceed,
		},
		{
			// Phase 1 labeled the sandbox but it resumed normally (user set
			// spec.Paused=false before reaching ResumeSucceed). No Upgrading
			// condition was set, template still doesn't match, sandbox is
			// Running. Should be routed to resumeSucceed so phase 2 applies
			// the template patch.
			name: "ops label + no condition + template differs + Running -> resumeSucceed (lost paused state)",
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
				Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
			},
			expected: sandboxResumeSucceed,
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
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
				},
			}),
		},
	}
	tests := []struct {
		name   string
		reason string
	}{
		{name: "PreUpgradeFailed", reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed},
		{name: "PostUpgradeFailed", reason: agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed},
		{name: "UpgradePodFailed", reason: agentsv1alpha1.SandboxUpgradingReasonUpgradePodFailed},
		{name: "CheckpointFailed", reason: agentsv1alpha1.SandboxUpgradingReasonCheckpointFailed},
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

func TestClassifySandbox_StateFilter(t *testing.T) {
	opsName := "test-ops"
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: opsName},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			StateFilter: &agentsv1alpha1.UpgradeStateFilter{
				States: []agentsv1alpha1.SandboxPhase{agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxPaused},
			},
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
			name: "StateFilter=[Running,Paused], Paused phase -> candidate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue},
					},
				},
			},
			expected: sandboxCandidate,
		},
		{
			name: "StateFilter=[Running,Paused], Running phase -> candidate (normal behavior)",
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
			name: "StateFilter=[Running,Paused], Resuming phase -> noNeedUpdate (not included)",
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
			name: "StateFilter=[Running,Paused], Paused phase, template matches -> noNeedUpdate",
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
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue},
					},
				},
			},
			expected: sandboxNoNeedUpdate,
		},
		{
			name: "StateFilter=[Running,Paused], Paused phase, no Paused condition -> noNeedUpdate (not fully paused)",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
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
			name: "StateFilter=[Running,Paused], Paused phase, condition=False -> noNeedUpdate (pause in progress)",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionFalse},
					},
				},
			},
			expected: sandboxNoNeedUpdate,
		},
		{
			name: "StateFilter=[Running,Paused], Paused phase, spec.Paused=false -> noNeedUpdate (resume in progress)",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue},
					},
				},
			},
			expected: sandboxNoNeedUpdate,
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

func TestClassifySandbox_StateFilterPausedOnly(t *testing.T) {
	opsName := "test-ops"
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: opsName},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			StateFilter: &agentsv1alpha1.UpgradeStateFilter{
				States: []agentsv1alpha1.SandboxPhase{agentsv1alpha1.SandboxPaused},
			},
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
			name: "StateFilter=[Paused], Paused phase -> candidate",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}},
							},
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue},
					},
				},
			},
			expected: sandboxCandidate,
		},
		{
			name: "StateFilter=[Paused], Running phase -> noNeedUpdate (Running not included)",
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
			expected: sandboxNoNeedUpdate,
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

func TestIsSandboxOpsApplied_Template(t *testing.T) {
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
			assert.Equal(t, tt.expected, isSandboxOpsApplied(sbx, ops))
		})
	}

	t.Run("empty patch retains legacy upgrade tracking", func(t *testing.T) {
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
		assert.False(t, isSandboxOpsApplied(sbx, ops))
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
		assert.True(t, isSandboxOpsApplied(sbx, ops))
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
		assert.False(t, isSandboxOpsApplied(sbx, ops))
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
		assert.False(t, isSandboxOpsApplied(sbx, ops))
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

	// Create 2 sandboxes with ops label and resume trigger annotation
	sbx1 := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)
	sbx1.Annotations = map[string]string{agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True}
	sbx2 := newSandbox("sbx-2", "default", "test-ops", agentsv1alpha1.SandboxRunning, nil)
	sbx2.Annotations = map[string]string{agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True}

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

	// Verify sandbox labels and annotations were removed
	updatedSbx1 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx1)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx1.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
	assert.Empty(t, updatedSbx1.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])

	updatedSbx2 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-2", Namespace: "default"}, updatedSbx2)
	assert.NoError(t, err)
	assert.Empty(t, updatedSbx2.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
	assert.Empty(t, updatedSbx2.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])

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
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
		},
	})
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

func TestHandleDeletion_ClearsUpgradePolicyOnlyForUpdatedSandboxes(t *testing.T) {
	action := &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"backup"}}}
	tests := []struct {
		name      string
		patch     runtime.RawExtension
		lifecycle *agentsv1alpha1.SandboxLifecycle
	}{
		{name: "template patch", patch: mustMarshalPatch(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}}}})},
		{name: "legacy nil lifecycle"},
		{name: "legacy empty lifecycle", lifecycle: &agentsv1alpha1.SandboxLifecycle{}},
		{name: "legacy upgrade hooks", lifecycle: &agentsv1alpha1.SandboxLifecycle{PreUpgrade: action}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsFailed, false, nil)
			ops.Spec.Patch = tt.patch
			ops.Spec.Lifecycle = tt.lifecycle.DeepCopy()
			ops.Finalizers = []string{finalizerName}
			policy := &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore}
			sbxUpdated := newSandbox("sbx-updated", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
			})
			sbxUpdated.Spec.UpgradePolicy = policy.DeepCopy()
			sbxUpgrading := newSandbox("sbx-upgrading", "default", "test-ops", agentsv1alpha1.SandboxUpgrading, []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod, Status: metav1.ConditionFalse},
			})
			sbxUpgrading.Spec.UpgradePolicy = policy.DeepCopy()
			for _, sbx := range []*agentsv1alpha1.Sandbox{sbxUpdated, sbxUpgrading} {
				sbx.Annotations = map[string]string{agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True}
			}

			r := newTestReconciler(ops, sbxUpdated, sbxUpgrading)
			require.NoError(t, r.Delete(t.Context(), ops))
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ops)})
			require.NoError(t, err)

			for _, sbx := range []*agentsv1alpha1.Sandbox{sbxUpdated, sbxUpgrading} {
				updated := &agentsv1alpha1.Sandbox{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
				assert.Empty(t, updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
				assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
				if sbx.Name == sbxUpdated.Name {
					assert.Nil(t, updated.Spec.UpgradePolicy)
				} else {
					assert.Equal(t, policy, updated.Spec.UpgradePolicy)
				}
			}
		})
	}
}

func TestIsUpgradeSucceeded(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		want       bool
	}{
		{name: "no conditions", conditions: nil, want: false},
		{
			name: "upgrade succeeded",
			conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
			},
			want: true,
		},
		{
			name: "succeeded reason but status false",
			conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionFalse},
			},
			want: false,
		},
		{
			name: "upgrade in progress",
			conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod, Status: metav1.ConditionFalse},
			},
			want: false,
		},
		{
			name: "upgrade failed",
			conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, Status: metav1.ConditionFalse},
			},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, tt.conditions)
			assert.Equal(t, tt.want, isUpgradeSucceeded(sbx))
		})
	}
}

func TestReconcile_CleanupUpdatedSandbox(t *testing.T) {
	policy := &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}
	tests := []struct {
		name           string
		policy         *agentsv1alpha1.SandboxUpgradePolicy
		hasFailedLabel bool
		injectPatchErr bool
		wantPatches    int
		wantPolicyNil  bool
		wantLabelGone  bool
	}{
		{
			name:          "clears policy after upgrade succeeds",
			policy:        policy.DeepCopy(),
			wantPatches:   1,
			wantPolicyNil: true,
		},
		{
			name:           "clears failed label and policy in a single patch",
			policy:         policy.DeepCopy(),
			hasFailedLabel: true,
			wantPatches:    1,
			wantPolicyNil:  true,
			wantLabelGone:  true,
		},
		{
			name:           "removes stale failed label when policy already absent",
			policy:         nil,
			hasFailedLabel: true,
			wantPatches:    1,
			wantPolicyNil:  true,
			wantLabelGone:  true,
		},
		{
			name:          "no patch issued when nothing needs cleanup",
			policy:        nil,
			wantPatches:   0,
			wantPolicyNil: true,
		},
		{
			name:           "patch error is best-effort and does not block reconciliation",
			policy:         policy.DeepCopy(),
			hasFailedLabel: true,
			injectPatchErr: true,
			wantPatches:    1,
			wantPolicyNil:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
			ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
				},
			})
			ops.Finalizers = []string{finalizerName}
			sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
			})
			sbx.Spec.UpgradePolicy = tt.policy
			if tt.hasFailedLabel {
				sbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed] = agentsv1alpha1.True
			}

			sandboxPatches := 0
			fakeClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}, &agentsv1alpha1.Sandbox{}).
				WithRuntimeObjects(ops, sbx).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
							sandboxPatches++
							if tt.injectPatchErr {
								return fmt.Errorf("simulated cleanup patch error")
							}
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()

			r := &Reconciler{Client: fakeClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(10)}
			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
			})
			assert.NoError(t, err)

			// The ops still reaches Completed even when the cleanup patch fails.
			updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
			err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
			assert.NoError(t, err)
			assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
			assert.Equal(t, tt.wantPatches, sandboxPatches)

			updatedSbx := &agentsv1alpha1.Sandbox{}
			err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
			assert.NoError(t, err)
			if tt.wantPolicyNil {
				assert.Nil(t, updatedSbx.Spec.UpgradePolicy)
			} else {
				assert.NotNil(t, updatedSbx.Spec.UpgradePolicy)
			}
			if tt.wantLabelGone {
				assert.NotEqual(t, agentsv1alpha1.True, updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])
			} else if tt.hasFailedLabel {
				assert.Equal(t, agentsv1alpha1.True, updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])
			}
		})
	}
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
	const (
		otherOpsName  = "ops-other"
		targetOpsName = "ops-target"
	)

	tests := []struct {
		name string
		// phase of the other ops sharing the namespace
		otherPhase agentsv1alpha1.SandboxUpdateOpsPhase
		// phase of the ops being reconciled
		targetPhase     agentsv1alpha1.SandboxUpdateOpsPhase
		targetDeleting  bool
		sandboxOpsLabel string
		wantResult      ctrl.Result
		wantPhase       agentsv1alpha1.SandboxUpdateOpsPhase
		wantOpsGone     bool
		wantSandboxOps  string
	}{
		{
			name:        "blocked while another ops is updating",
			otherPhase:  agentsv1alpha1.SandboxUpdateOpsUpdating,
			targetPhase: agentsv1alpha1.SandboxUpdateOpsPending,
			// Requeued so the ops resumes once the active one finishes,
			// instead of stalling until the next resync.
			wantResult: ctrl.Result{RequeueAfter: blockedRequeueInterval},
			wantPhase:  agentsv1alpha1.SandboxUpdateOpsPending,
		},
		{
			name:           "proceeds once the other ops completes",
			otherPhase:     agentsv1alpha1.SandboxUpdateOpsCompleted,
			targetPhase:    agentsv1alpha1.SandboxUpdateOpsPending,
			wantResult:     ctrl.Result{},
			wantPhase:      agentsv1alpha1.SandboxUpdateOpsUpdating,
			wantSandboxOps: targetOpsName,
		},
		{
			name:        "terminal ops is not requeued while another ops updates",
			otherPhase:  agentsv1alpha1.SandboxUpdateOpsUpdating,
			targetPhase: agentsv1alpha1.SandboxUpdateOpsCompleted,
			wantResult:  ctrl.Result{},
			wantPhase:   agentsv1alpha1.SandboxUpdateOpsCompleted,
		},
		{
			name:            "deletion is not blocked by another updating ops",
			otherPhase:      agentsv1alpha1.SandboxUpdateOpsUpdating,
			targetPhase:     agentsv1alpha1.SandboxUpdateOpsUpdating,
			targetDeleting:  true,
			sandboxOpsLabel: targetOpsName,
			wantResult:      ctrl.Result{},
			wantOpsGone:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			other := newSandboxUpdateOps(otherOpsName, "default", tt.otherPhase, false, nil)
			target := newSandboxUpdateOps(targetOpsName, "default", tt.targetPhase, false, nil)
			target.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
				},
			})
			target.Finalizers = []string{finalizerName}
			sbx := newSandbox("sbx-1", "default", tt.sandboxOpsLabel, agentsv1alpha1.SandboxRunning, nil)

			r := newTestReconciler(other, target, sbx)

			if tt.targetDeleting {
				// The fake client only sets DeletionTimestamp because the
				// finalizer keeps the object around.
				assert.NoError(t, r.Delete(context.Background(), target))
			}

			result, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: targetOpsName, Namespace: "default"},
			})
			assert.NoError(t, err)
			assert.Equal(t, tt.wantResult, result)

			updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
			err = r.Get(context.Background(), types.NamespacedName{Name: targetOpsName, Namespace: "default"}, updatedOps)
			if tt.wantOpsGone {
				assert.True(t, errors.IsNotFound(err), "ops should be gone after finalizer removal")
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantPhase, updatedOps.Status.Phase)
			}

			updatedSbx := &agentsv1alpha1.Sandbox{}
			err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
			assert.NoError(t, err)
			assert.Equal(t, tt.wantSandboxOps, updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
		})
	}
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

func TestSyncSandboxUpgradeState(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
		},
	})
	policy := &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}

	opsLabels := map[string]string{agentsv1alpha1.LabelSandboxUpdateOps: "test-ops"}
	failedLabels := map[string]string{
		agentsv1alpha1.LabelSandboxUpdateOps:     "test-ops",
		agentsv1alpha1.LabelSandboxUpgradeFailed: agentsv1alpha1.True,
	}
	newStateSandbox := func(name string, labels map[string]string, withPolicy bool) *agentsv1alpha1.Sandbox {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels},
		}
		if withPolicy {
			sbx.Spec.UpgradePolicy = policy.DeepCopy()
		}
		return sbx
	}

	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		state          sandboxUpdateState
		emptyPatch     bool
		lifecycle      *agentsv1alpha1.SandboxLifecycle
		injectPatchErr bool
		wantPatches    int
		wantLabel      bool
		wantLabelGone  bool
		wantPolicyNil  bool
		wantPolicyKept bool
	}{
		{
			name:        "failed and no label -> add label",
			sandbox:     newStateSandbox("sbx-1", opsLabels, false),
			state:       sandboxFailed,
			wantPatches: 1,
			wantLabel:   true,
		},
		{
			name:        "failed and already has label -> no patch",
			sandbox:     newStateSandbox("sbx-2", failedLabels, false),
			state:       sandboxFailed,
			wantPatches: 0,
			wantLabel:   true,
		},
		{
			name:          "updating and has label -> remove label",
			sandbox:       newStateSandbox("sbx-3", failedLabels, false),
			state:         sandboxUpdating,
			wantPatches:   1,
			wantLabelGone: true,
		},
		{
			name:        "updating and no label -> no patch",
			sandbox:     newStateSandbox("sbx-4", opsLabels, false),
			state:       sandboxUpdating,
			wantPatches: 0,
		},
		{
			name:          "updated removes stale label and clears policy in a single patch",
			sandbox:       newStateSandbox("sbx-5", failedLabels, true),
			state:         sandboxUpdated,
			wantPatches:   1,
			wantLabelGone: true,
			wantPolicyNil: true,
		},
		{
			name:          "updated clears policy only",
			sandbox:       newStateSandbox("sbx-6", opsLabels, true),
			state:         sandboxUpdated,
			wantPatches:   1,
			wantPolicyNil: true,
		},
		{
			name:          "updated removes stale label when policy already absent",
			sandbox:       newStateSandbox("sbx-7", failedLabels, false),
			state:         sandboxUpdated,
			wantPatches:   1,
			wantLabelGone: true,
		},
		{
			name:        "updated and nothing to clean -> no patch",
			sandbox:     newStateSandbox("sbx-8", opsLabels, false),
			state:       sandboxUpdated,
			wantPatches: 0,
		},
		{
			name:           "resumeSucceed removes stale label and keeps policy",
			sandbox:        newStateSandbox("sbx-9", failedLabels, true),
			state:          sandboxResumeSucceed,
			wantPatches:    1,
			wantLabelGone:  true,
			wantPolicyKept: true,
		},
		{
			name:           "patch error is best-effort",
			sandbox:        newStateSandbox("sbx-10", failedLabels, true),
			state:          sandboxUpdated,
			injectPatchErr: true,
			wantPatches:    1,
			wantPolicyKept: true,
		},
		{
			name:          "legacy empty-patch success clears owned policy",
			sandbox:       newStateSandbox("sbx-11", failedLabels, true),
			state:         sandboxUpdated,
			emptyPatch:    true,
			wantPatches:   1,
			wantLabelGone: true,
			wantPolicyNil: true,
		},
		{
			name:           "configuration pending preserves failed label and policy",
			sandbox:        newStateSandbox("sbx-12", failedLabels, true),
			state:          sandboxUpdating,
			emptyPatch:     true,
			lifecycle:      &agentsv1alpha1.SandboxLifecycle{PrePause: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}}},
			wantLabel:      true,
			wantPolicyKept: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			currentOps := ops.DeepCopy()
			if tt.emptyPatch {
				currentOps.Spec.Patch = runtime.RawExtension{}
			}
			currentOps.Spec.Lifecycle = tt.lifecycle.DeepCopy()
			sandboxPatches := 0
			fakeClient := fake.NewClientBuilder().
				WithScheme(testScheme).
				WithRuntimeObjects(tt.sandbox).
				WithInterceptorFuncs(interceptor.Funcs{
					Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
						if _, ok := obj.(*agentsv1alpha1.Sandbox); ok {
							sandboxPatches++
							if tt.injectPatchErr {
								return fmt.Errorf("simulated sync patch error")
							}
						}
						return c.Patch(ctx, obj, patch, opts...)
					},
				}).
				Build()
			r := &Reconciler{Client: fakeClient, Scheme: testScheme, Recorder: record.NewFakeRecorder(10)}

			r.syncSandboxUpgradeState(context.Background(), tt.sandbox, currentOps, tt.state)
			assert.Equal(t, tt.wantPatches, sandboxPatches)

			updated := &agentsv1alpha1.Sandbox{}
			err := r.Get(context.Background(), types.NamespacedName{Name: tt.sandbox.Name, Namespace: tt.sandbox.Namespace}, updated)
			assert.NoError(t, err)
			if tt.wantLabel {
				assert.Equal(t, agentsv1alpha1.True, updated.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])
			}
			if tt.wantLabelGone {
				assert.NotEqual(t, agentsv1alpha1.True, updated.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])
			}
			if tt.wantPolicyNil {
				assert.Nil(t, updated.Spec.UpgradePolicy)
			}
			if tt.wantPolicyKept {
				assert.NotNil(t, updated.Spec.UpgradePolicy)
			}
		})
	}
}

func TestReconcile_FailedSandboxGetsUpgradeFailedLabel(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}},
		},
	})
	// sbx-1: failed via condition
	sbx1 := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, Status: metav1.ConditionFalse},
	})
	// sbx-2: succeeded
	sbx2 := newSandbox("sbx-2", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
	})
	r := newTestReconciler(ops, sbx1, sbx2)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	// sbx-1 should have the upgrade-failed label
	updatedSbx1 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx1)
	assert.NoError(t, err)
	assert.Equal(t, agentsv1alpha1.True, updatedSbx1.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed],
		"failed sandbox should have upgrade-failed label")

	// sbx-2 should NOT have the upgrade-failed label
	updatedSbx2 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-2", Namespace: "default"}, updatedSbx2)
	assert.NoError(t, err)
	assert.NotEqual(t, agentsv1alpha1.True, updatedSbx2.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed],
		"successful sandbox should not have upgrade-failed label")
}

func TestReconcile_UpgradeFailedLabelRemovedOnRecovery(t *testing.T) {
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
	// sbx-1: previously failed (has label), now succeeded
	sbx1 := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxRunning, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue},
	})
	sbx1.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed] = agentsv1alpha1.True
	r := newTestReconciler(ops, sbx1)

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	// Label should be removed after recovery
	updatedSbx1 := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx1)
	assert.NoError(t, err)
	assert.NotEqual(t, agentsv1alpha1.True, updatedSbx1.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed],
		"recovered sandbox should not have upgrade-failed label")
}

func TestReconcile_Phase2PatchForResumeSucceed(t *testing.T) {
	action := &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"backup"}}}
	tests := []struct {
		name      string
		patch     runtime.RawExtension
		lifecycle *agentsv1alpha1.SandboxLifecycle
		wantImage string
	}{
		{name: "template patch", patch: mustMarshalPatch(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}}}}), wantImage: "busybox:2.0"},
		{name: "legacy nil lifecycle", wantImage: "busybox:1.0"},
		{name: "legacy empty lifecycle", lifecycle: &agentsv1alpha1.SandboxLifecycle{}, wantImage: "busybox:1.0"},
		{name: "legacy preUpgrade", lifecycle: &agentsv1alpha1.SandboxLifecycle{PreUpgrade: action}, wantImage: "busybox:1.0"},
		{name: "legacy postUpgrade", lifecycle: &agentsv1alpha1.SandboxLifecycle{PostUpgrade: action}, wantImage: "busybox:1.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
			ops.Finalizers = []string{finalizerName}
			ops.Spec.Patch = tt.patch
			ops.Spec.Lifecycle = tt.lifecycle.DeepCopy()
			sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxUpgrading, []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonResumeSucceed, Status: metav1.ConditionFalse},
			})
			sbx.Spec.Lifecycle = tt.lifecycle.DeepCopy()
			sbx.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}
			sbx.Annotations = map[string]string{agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True}

			r := newTestReconciler(ops, sbx)
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ops)}
			_, err := r.Reconcile(t.Context(), req)
			require.NoError(t, err)

			updatedSbx := &agentsv1alpha1.Sandbox{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updatedSbx))
			assert.Equal(t, tt.wantImage, updatedSbx.Spec.Template.Spec.Containers[0].Image)
			assert.Empty(t, updatedSbx.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
			assert.Equal(t, sbx.Spec.UpgradePolicy, updatedSbx.Spec.UpgradePolicy)
			updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
			require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedOps))
			assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsUpdating, updatedOps.Status.Phase)
			assert.Equal(t, int32(1), updatedOps.Status.UpdatingReplicas)
			assert.Zero(t, updatedOps.Status.UpdatedReplicas)
		})
	}
}

func TestReconcile_Phase2PatchPausedOpsSkipsPhase2(t *testing.T) {
	// When ops.Spec.Paused is true, phase 2 patch should NOT be executed
	// even if there are resumeSucceed candidates.
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, true, nil)
	ops.Finalizers = []string{finalizerName}
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Image: "busybox:2.0"},
			},
		},
	})
	sbx := newSandbox("sbx-1", "default", "test-ops", agentsv1alpha1.SandboxUpgrading, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonResumeSucceed, Status: metav1.ConditionFalse},
	})
	sbx.Annotations = map[string]string{
		agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True,
	}

	r := newTestReconciler(ops, sbx)
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
	})
	assert.NoError(t, err)

	// Verify phase 2 patch was NOT applied (ops is paused)
	updatedSbx := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updatedSbx)
	assert.NoError(t, err)
	assert.Equal(t, "busybox:1.0", updatedSbx.Spec.Template.Spec.Containers[0].Image,
		"template should NOT be patched when ops is paused")
	_, exists := updatedSbx.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger]
	assert.True(t, exists, "resume trigger annotation should remain when ops is paused")
}

func TestIsStateIncluded_DefaultExcludesPaused(t *testing.T) {
	tests := []struct {
		name     string
		ops      *agentsv1alpha1.SandboxUpdateOps
		phase    agentsv1alpha1.SandboxPhase
		expected bool
	}{
		{
			name:     "nil StateFilter, Running -> true",
			ops:      &agentsv1alpha1.SandboxUpdateOps{},
			phase:    agentsv1alpha1.SandboxRunning,
			expected: true,
		},
		{
			name:     "nil StateFilter, Paused -> false (not included by default)",
			ops:      &agentsv1alpha1.SandboxUpdateOps{},
			phase:    agentsv1alpha1.SandboxPaused,
			expected: false,
		},
		{
			name:     "nil StateFilter, Pending -> false",
			ops:      &agentsv1alpha1.SandboxUpdateOps{},
			phase:    agentsv1alpha1.SandboxPending,
			expected: false,
		},
		{
			name:     "nil StateFilter, Upgrading -> true (always included)",
			ops:      &agentsv1alpha1.SandboxUpdateOps{},
			phase:    agentsv1alpha1.SandboxUpgrading,
			expected: true,
		},
		{
			name: "empty States, Running -> true",
			ops: &agentsv1alpha1.SandboxUpdateOps{
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					StateFilter: &agentsv1alpha1.UpgradeStateFilter{},
				},
			},
			phase:    agentsv1alpha1.SandboxRunning,
			expected: true,
		},
		{
			name: "empty States, Paused -> false (not included by default)",
			ops: &agentsv1alpha1.SandboxUpdateOps{
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					StateFilter: &agentsv1alpha1.UpgradeStateFilter{},
				},
			},
			phase:    agentsv1alpha1.SandboxPaused,
			expected: false,
		},
		{
			name: "States=[Running], Paused -> false",
			ops: &agentsv1alpha1.SandboxUpdateOps{
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					StateFilter: &agentsv1alpha1.UpgradeStateFilter{
						States: []agentsv1alpha1.SandboxPhase{agentsv1alpha1.SandboxRunning},
					},
				},
			},
			phase:    agentsv1alpha1.SandboxPaused,
			expected: false,
		},
		{
			name: "States=[Paused], Running -> false",
			ops: &agentsv1alpha1.SandboxUpdateOps{
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					StateFilter: &agentsv1alpha1.UpgradeStateFilter{
						States: []agentsv1alpha1.SandboxPhase{agentsv1alpha1.SandboxPaused},
					},
				},
			},
			phase:    agentsv1alpha1.SandboxRunning,
			expected: false,
		},
		{
			name: "States=[Running,Paused], Running -> true",
			ops: &agentsv1alpha1.SandboxUpdateOps{
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					StateFilter: &agentsv1alpha1.UpgradeStateFilter{
						States: []agentsv1alpha1.SandboxPhase{agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxPaused},
					},
				},
			},
			phase:    agentsv1alpha1.SandboxRunning,
			expected: true,
		},
		{
			name: "States=[Running,Paused], Paused -> true",
			ops: &agentsv1alpha1.SandboxUpdateOps{
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					StateFilter: &agentsv1alpha1.UpgradeStateFilter{
						States: []agentsv1alpha1.SandboxPhase{agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxPaused},
					},
				},
			},
			phase:    agentsv1alpha1.SandboxPaused,
			expected: true,
		},
		{
			name: "States=[Running], Upgrading -> true (always included)",
			ops: &agentsv1alpha1.SandboxUpdateOps{
				Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
					StateFilter: &agentsv1alpha1.UpgradeStateFilter{
						States: []agentsv1alpha1.SandboxPhase{agentsv1alpha1.SandboxRunning},
					},
				},
			},
			phase:    agentsv1alpha1.SandboxUpgrading,
			expected: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isStateIncluded(tt.ops, tt.phase)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestReconcile_LabeledSandboxInIntermediatePhaseStaysTracked(t *testing.T) {
	// Regression test: a sandbox already claimed by this ops (has the ops label)
	// that transits through an intermediate phase not in the StateFilter (e.g.
	// Resuming after a user un-pause mid two-phase upgrade) must stay tracked.
	// Otherwise the ops could complete while the sandbox is still pending its
	// template patch.
	tests := []struct {
		name             string
		opsLabel         string
		phase            agentsv1alpha1.SandboxPhase
		expectedOpsPhase agentsv1alpha1.SandboxUpdateOpsPhase
		expectedReplicas int32
		expectedUpdating int32
	}{
		{
			name:             "labeled + Resuming -> stays Updating",
			opsLabel:         "test-ops",
			phase:            agentsv1alpha1.SandboxResuming,
			expectedOpsPhase: agentsv1alpha1.SandboxUpdateOpsUpdating,
			expectedReplicas: 1,
			expectedUpdating: 1,
		},
		{
			name:             "labeled + Pending -> stays Updating",
			opsLabel:         "test-ops",
			phase:            agentsv1alpha1.SandboxPending,
			expectedOpsPhase: agentsv1alpha1.SandboxUpdateOpsUpdating,
			expectedReplicas: 1,
			expectedUpdating: 1,
		},
		{
			name:             "unlabeled + Resuming -> filtered out, ops completes",
			opsLabel:         "",
			phase:            agentsv1alpha1.SandboxResuming,
			expectedOpsPhase: agentsv1alpha1.SandboxUpdateOpsCompleted,
			expectedReplicas: 0,
			expectedUpdating: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
			ops.Spec.StateFilter = &agentsv1alpha1.UpgradeStateFilter{
				States: []agentsv1alpha1.SandboxPhase{
					agentsv1alpha1.SandboxRunning,
					agentsv1alpha1.SandboxPaused,
				},
			}
			ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "busybox:2.0"},
					},
				},
			})
			sbx := newSandbox("sbx-1", "default", tt.opsLabel, tt.phase, nil)
			r := newTestReconciler(ops, sbx)

			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
			})
			assert.NoError(t, err)

			updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
			err = r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedOpsPhase, updatedOps.Status.Phase)
			assert.Equal(t, tt.expectedReplicas, updatedOps.Status.Replicas)
			assert.Equal(t, tt.expectedUpdating, updatedOps.Status.UpdatingReplicas)
		})
	}
}

func TestReconcile_HookOnlyOps_CompletesWithUpdatedReplicas(t *testing.T) {
	hooks := &agentsv1alpha1.SandboxLifecycle{
		PrePause: &agentsv1alpha1.LifecycleAction{
			Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "flush.sh"}},
		},
	}
	ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, nil)
	ops.Spec.Lifecycle = hooks
	// No template patch: hook-only ops.

	sbx1 := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
	sbx2 := newSandbox("sbx-2", "default", "", agentsv1alpha1.SandboxRunning, nil)

	r := newTestReconciler(ops, sbx1, sbx2)
	// Reconcile until the ops completes: with the default maxUnavailable of 1,
	// each round applies the hooks to one sandbox and observes the previous
	// one as updated.
	var updatedOps *agentsv1alpha1.SandboxUpdateOps
	for i := 0; i < 10; i++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "test-ops", Namespace: "default"},
		})
		assert.NoError(t, err)
		updatedOps = &agentsv1alpha1.SandboxUpdateOps{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "test-ops", Namespace: "default"}, updatedOps))
		if updatedOps.Status.Phase == agentsv1alpha1.SandboxUpdateOpsCompleted {
			break
		}
	}

	// Both sandboxes carry the hooks, with no upgrade policy and no upgrade
	// condition — the hooks merged without any pod replacement.
	for _, name := range []string{"sbx-1", "sbx-2"} {
		updated := &agentsv1alpha1.Sandbox{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "default"}, updated))
		require.NotNil(t, updated.Spec.Lifecycle, name)
		require.NotNil(t, updated.Spec.Lifecycle.PrePause, name)
		assert.Nil(t, updated.Spec.UpgradePolicy, name)
		assert.Equal(t, "test-ops", updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps], name)
	}

	require.NotNil(t, updatedOps)
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
	assert.Equal(t, int32(2), updatedOps.Status.Replicas)
	assert.Equal(t, int32(2), updatedOps.Status.UpdatedReplicas)
	assert.Equal(t, int32(0), updatedOps.Status.UpdatingReplicas)
}

func TestClassifySandbox_EmptyPatchOperationModes(t *testing.T) {
	action := &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}}
	modes := []struct {
		name              string
		lifecycle         *agentsv1alpha1.SandboxLifecycle
		configurationOnly bool
	}{
		{name: "legacy nil lifecycle"},
		{name: "legacy empty lifecycle", lifecycle: &agentsv1alpha1.SandboxLifecycle{}},
		{name: "legacy preUpgrade", lifecycle: &agentsv1alpha1.SandboxLifecycle{PreUpgrade: action}},
		{name: "legacy postUpgrade", lifecycle: &agentsv1alpha1.SandboxLifecycle{PostUpgrade: action}},
		{name: "mixed hooks", lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: action, PreUpgrade: action}},
		{name: "prePause only", lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: action}, configurationOnly: true},
		{name: "postResume only", lifecycle: &agentsv1alpha1.SandboxLifecycle{PostResume: action}, configurationOnly: true},
	}
	states := []struct {
		name               string
		opsLabel           string
		phase              agentsv1alpha1.SandboxPhase
		reason             string
		conditionStatus    metav1.ConditionStatus
		observedGeneration int64
		wantUpgrade        sandboxUpdateState
		wantConfiguration  sandboxUpdateState
	}{
		{name: "fresh", phase: agentsv1alpha1.SandboxRunning, observedGeneration: 1, wantUpgrade: sandboxCandidate, wantConfiguration: sandboxNoNeedUpdate},
		{name: "generation pending", opsLabel: "test-ops", phase: agentsv1alpha1.SandboxUpgrading, observedGeneration: 0, wantUpgrade: sandboxUpdating, wantConfiguration: sandboxUpdating},
		{name: "running without condition", opsLabel: "test-ops", phase: agentsv1alpha1.SandboxRunning, observedGeneration: 1, wantUpgrade: sandboxResumeSucceed, wantConfiguration: sandboxUpdated},
		{name: "preUpgrade failure", opsLabel: "test-ops", phase: agentsv1alpha1.SandboxUpgrading, reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, conditionStatus: metav1.ConditionFalse, observedGeneration: 1, wantUpgrade: sandboxFailed, wantConfiguration: sandboxUpdated},
		{name: "postUpgrade failure", opsLabel: "test-ops", phase: agentsv1alpha1.SandboxUpgrading, reason: agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed, conditionStatus: metav1.ConditionFalse, observedGeneration: 1, wantUpgrade: sandboxFailed, wantConfiguration: sandboxUpdated},
		{name: "upgrading", opsLabel: "test-ops", phase: agentsv1alpha1.SandboxUpgrading, reason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod, conditionStatus: metav1.ConditionFalse, observedGeneration: 1, wantUpgrade: sandboxUpdating, wantConfiguration: sandboxUpdated},
		{name: "resuming", opsLabel: "test-ops", phase: agentsv1alpha1.SandboxUpgrading, reason: agentsv1alpha1.SandboxUpgradingReasonResuming, conditionStatus: metav1.ConditionFalse, observedGeneration: 1, wantUpgrade: sandboxUpdating, wantConfiguration: sandboxUpdated},
		{name: "resume succeeded", opsLabel: "test-ops", phase: agentsv1alpha1.SandboxUpgrading, reason: agentsv1alpha1.SandboxUpgradingReasonResumeSucceed, conditionStatus: metav1.ConditionFalse, observedGeneration: 1, wantUpgrade: sandboxResumeSucceed, wantConfiguration: sandboxUpdated},
		{name: "upgrade succeeded", opsLabel: "test-ops", phase: agentsv1alpha1.SandboxRunning, reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, conditionStatus: metav1.ConditionTrue, observedGeneration: 1, wantUpgrade: sandboxUpdated, wantConfiguration: sandboxUpdated},
	}
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			for _, state := range states {
				for _, hooksMatch := range []bool{true, false} {
					t.Run(fmt.Sprintf("%s/hooksMatch=%t", state.name, hooksMatch), func(t *testing.T) {
						ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
						ops.Spec.Lifecycle = mode.lifecycle.DeepCopy()
						sbx := newSandbox("sbx-1", "default", state.opsLabel, state.phase, nil)
						sbx.Generation = 1
						sbx.Status.ObservedGeneration = state.observedGeneration
						if hooksMatch {
							sbx.Spec.Lifecycle = mode.lifecycle.DeepCopy()
						}
						if state.reason != "" {
							sbx.Status.Conditions = []metav1.Condition{{
								Type:   string(agentsv1alpha1.SandboxConditionUpgrading),
								Reason: state.reason,
								Status: state.conditionStatus,
							}}
						}
						want := state.wantUpgrade
						if mode.configurationOnly {
							want = state.wantConfiguration
							if !hooksMatch && want != sandboxUpdating {
								want = sandboxCandidate
							}
						}
						r := newTestReconciler()
						assert.Equal(t, want, r.classifySandbox(t.Context(), sbx, ops))
					})
				}
			}
		})
	}
}

func TestReconcile_HookOnlyOps_PreservesIndependentUpgradePolicy(t *testing.T) {
	for _, policyType := range []agentsv1alpha1.SandboxUpgradePolicyType{
		agentsv1alpha1.SandboxUpgradePolicyRecreate,
		agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore,
	} {
		for _, reason := range []string{"", agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, agentsv1alpha1.SandboxUpgradingReasonPostUpgradeFailed, agentsv1alpha1.SandboxUpgradingReasonSucceeded} {
			t.Run(fmt.Sprintf("%s/%s", policyType, reason), func(t *testing.T) {
				ops := newSandboxUpdateOps("test-ops", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, nil)
				ops.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
					PrePause: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}},
				}
				sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxRunning, nil)
				sbx.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{Type: policyType}
				sbx.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
					PreUpgrade:  &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"backup"}}},
					PostUpgrade: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}},
					PostResume:  &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"initialise"}}},
				}
				sbx.Annotations = map[string]string{agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True}
				if reason != "" {
					conditionStatus := metav1.ConditionFalse
					if reason == agentsv1alpha1.SandboxUpgradingReasonSucceeded {
						conditionStatus = metav1.ConditionTrue
					} else {
						sbx.Status.Phase = agentsv1alpha1.SandboxUpgrading
						sbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed] = agentsv1alpha1.True
					}
					sbx.Status.Conditions = []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: reason, Status: conditionStatus}}
				}

				r := newTestReconciler(ops, sbx)
				req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ops)}
				for i := 0; i < 2; i++ {
					_, err := r.Reconcile(t.Context(), req)
					require.NoError(t, err)
				}
				updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
				require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedOps))
				assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
				assert.Equal(t, int32(1), updatedOps.Status.UpdatedReplicas)

				expectedLifecycle := sbx.Spec.Lifecycle.DeepCopy()
				expectedLifecycle.PrePause = ops.Spec.Lifecycle.PrePause.DeepCopy()
				updatedSbx := &agentsv1alpha1.Sandbox{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updatedSbx))
				assert.Equal(t, expectedLifecycle, updatedSbx.Spec.Lifecycle)
				assert.Equal(t, sbx.Spec.UpgradePolicy, updatedSbx.Spec.UpgradePolicy)
				assert.Equal(t, sbx.Status, updatedSbx.Status)
				assert.Equal(t, sbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed], updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])

				require.NoError(t, r.Delete(t.Context(), updatedOps))
				_, err := r.Reconcile(t.Context(), req)
				require.NoError(t, err)
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updatedSbx))
				assert.Empty(t, updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
				assert.Equal(t, expectedLifecycle, updatedSbx.Spec.Lifecycle)
				assert.Equal(t, sbx.Spec.UpgradePolicy, updatedSbx.Spec.UpgradePolicy)
				assert.Equal(t, sbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed], updatedSbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])
				assert.Equal(t, agentsv1alpha1.True, updatedSbx.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
			})
		}
	}
}
