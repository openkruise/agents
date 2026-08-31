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

package core

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	featuregatetesting "k8s.io/component-base/featuregate/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

// enableProbeGate turns on AutoPauseControllerGate, which is off by default and
// gates probe injection and condition sync as well as the pause/resume decision.
func enableProbeGate(t *testing.T) {
	featuregatetesting.SetFeatureGateDuringTest(t, utilfeature.DefaultFeatureGate, features.AutoPauseControllerGate, true)
}

func TestInjectPodProbeAnnotation(t *testing.T) {
	enableProbeGate(t)
	manager := &PodProbeManager{}

	tests := []struct {
		name             string
		box              *agentsv1alpha1.Sandbox
		pod              *corev1.Pod
		expectAnnotation bool
		expectItems      []podProbeItem
	}{
		{
			name: "no probes - no annotation",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{},
			},
			pod:              &corev1.Pod{},
			expectAnnotation: false,
		},
		{
			name: "with probes - annotation set with correct fields",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "activity",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main"}},
				},
			},
			expectAnnotation: true,
			expectItems: []podProbeItem{
				{
					ContainerName:    "main",
					Name:             "activity",
					PodConditionType: agentsv1alpha1.ProbeConditionPrefix + "activity",
				},
			},
		},
		{
			name: "with probes and nil annotations map - creates map",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "activity",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main"}},
				},
			},
			expectAnnotation: true,
			expectItems: []podProbeItem{
				{
					ContainerName:    "main",
					Name:             "activity",
					PodConditionType: agentsv1alpha1.ProbeConditionPrefix + "activity",
				},
			},
		},
		{
			name: "default container name from first container",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "activity",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "custom-container"}},
				},
			},
			expectAnnotation: true,
			expectItems: []podProbeItem{
				{
					ContainerName:    "custom-container",
					Name:             "activity",
					PodConditionType: agentsv1alpha1.ProbeConditionPrefix + "activity",
				},
			},
		},
		{
			name: "explicit container name overrides default",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name:          "activity",
							ContainerName: "explicit-container",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "default-container"}},
				},
			},
			expectAnnotation: true,
			expectItems: []podProbeItem{
				{
					ContainerName:    "explicit-container",
					Name:             "activity",
					PodConditionType: agentsv1alpha1.ProbeConditionPrefix + "activity",
				},
			},
		},
		{
			name: "multiple probes - all injected",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "activity",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "active"}},
								},
							},
						},
						{
							Name: "health",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "healthy"}},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main"}},
				},
			},
			expectAnnotation: true,
			expectItems: []podProbeItem{
				{
					ContainerName:    "main",
					Name:             "activity",
					PodConditionType: agentsv1alpha1.ProbeConditionPrefix + "activity",
				},
				{
					ContainerName:    "main",
					Name:             "health",
					PodConditionType: agentsv1alpha1.ProbeConditionPrefix + "health",
				},
			},
		},
		{
			name: "no containers in pod - empty container name",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "activity",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{},
				},
			},
			expectAnnotation: true,
			expectItems: []podProbeItem{
				{
					ContainerName:    "",
					Name:             "activity",
					PodConditionType: agentsv1alpha1.ProbeConditionPrefix + "activity",
				},
			},
		},
		{
			name: "httpget probe - skipped, no annotation",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "activity",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Path: "/health"},
								},
							},
						},
					},
				},
			},
			pod:              &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}},
			expectAnnotation: false,
		},
		{
			name: "empty probe name - skipped, no annotation",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
								},
							},
						},
					},
				},
			},
			pod:              &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}},
			expectAnnotation: false,
		},
		{
			name: "empty exec command - skipped, no annotation",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "activity",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{}},
								},
							},
						},
					},
				},
			},
			pod:              &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}},
			expectAnnotation: false,
		},
		{
			name: "mixed valid and invalid probes - all skipped",
			box: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Probes: []agentsv1alpha1.Probe{
						{
							Name: "valid",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"echo", "ok"}},
								},
							},
						},
						{
							Name: "invalid",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{Path: "/health"},
								},
							},
						},
					},
				},
			},
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main"}},
				},
			},
			expectAnnotation: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager.InjectProbe(context.Background(), tt.box, tt.pod)

			annotation, exists := tt.pod.Annotations[agentsv1alpha1.AnnotationPodProbe]
			if !tt.expectAnnotation {
				assert.False(t, exists)
				return
			}
			assert.True(t, exists)

			var items []podProbeItem
			require.NoError(t, json.Unmarshal([]byte(annotation), &items))
			assert.Len(t, items, len(tt.expectItems))
			for i, expected := range tt.expectItems {
				if i >= len(items) {
					break
				}
				assert.Equal(t, expected.Name, items[i].Name)
				assert.Equal(t, expected.ContainerName, items[i].ContainerName)
				assert.Equal(t, expected.PodConditionType, items[i].PodConditionType)
			}
		})
	}
}

func TestInjectPodProbeAnnotationGateDisabled(t *testing.T) {
	manager := &PodProbeManager{}
	box := &agentsv1alpha1.Sandbox{
		Spec: agentsv1alpha1.SandboxSpec{
			Probes: []agentsv1alpha1.Probe{
				{
					Name: "activity",
					Probe: corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							Exec: &corev1.ExecAction{Command: []string{"echo", "ok"}},
						},
					},
				},
			},
		},
	}
	pod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}}

	// The gate is off by default: a rollback must leave the pod exactly as it
	// was, not inject probes that nothing will consume.
	manager.InjectProbe(context.Background(), box, pod)

	_, exists := pod.Annotations[agentsv1alpha1.AnnotationPodProbe]
	assert.False(t, exists)
}

func TestSyncPodProbeAnnotation(t *testing.T) {
	enableProbeGate(t)
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	probeSpec := agentsv1alpha1.SandboxSpec{
		Probes: []agentsv1alpha1.Probe{
			{
				Name: "activity",
				Probe: corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
					},
				},
			},
		},
	}

	// Build expected annotation for the probe spec above
	expectedPod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}}}
	manager := &PodProbeManager{}
	manager.InjectProbe(context.Background(), &agentsv1alpha1.Sandbox{Spec: probeSpec}, expectedPod)
	expectedAnnotation := expectedPod.Annotations[agentsv1alpha1.AnnotationPodProbe]

	tests := []struct {
		name          string
		box           *agentsv1alpha1.Sandbox
		pod           *corev1.Pod
		expectPatch   bool
		expectRemoved bool
	}{
		{
			name: "annotation already matches - no patch",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
				Spec:       probeSpec,
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default", Annotations: map[string]string{
					agentsv1alpha1.AnnotationPodProbe: expectedAnnotation,
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			},
			expectPatch: false,
		},
		{
			name: "annotation missing - patch to add",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
				Spec:       probeSpec,
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			},
			expectPatch: true,
		},
		{
			name: "annotation outdated - patch to update",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
				Spec:       probeSpec,
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default", Annotations: map[string]string{
					agentsv1alpha1.AnnotationPodProbe: "[{\"name\":\"old\"}]",
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			},
			expectPatch: true,
		},
		{
			name: "probes removed - patch to delete annotation",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSpec{}, // no probes
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default", Annotations: map[string]string{
					agentsv1alpha1.AnnotationPodProbe: expectedAnnotation,
				}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			},
			expectPatch:   true,
			expectRemoved: true,
		},
		{
			name: "no probes and no annotation - no-op",
			box: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
				Spec:       agentsv1alpha1.SandboxSpec{},
			},
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
			},
			expectPatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeClient := fake.NewClientBuilder().
				WithScheme(scheme).
				WithObjects(tt.pod).
				Build()
			manager := NewPodProbeManager(fakeClient, record.NewFakeRecorder(10))

			err := manager.EnsureProbe(context.Background(), tt.box, tt.pod, &agentsv1alpha1.SandboxStatus{})
			require.NoError(t, err)

			// Fetch the patched pod from fake client
			updated := &corev1.Pod{}
			require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(tt.pod), updated))

			if tt.expectRemoved {
				_, exists := updated.Annotations[agentsv1alpha1.AnnotationPodProbe]
				assert.False(t, exists, "annotation should be removed")
			} else if tt.expectPatch {
				actual := updated.Annotations[agentsv1alpha1.AnnotationPodProbe]
				assert.NotEmpty(t, actual, "annotation should be set")

				// Verify the annotation content matches expected
				if len(tt.box.Spec.Probes) > 0 {
					expected := buildPodProbeAnnotation(tt.box, tt.pod)
					assert.Equal(t, expected, actual)
				}
			} else {
				// No patch expected — annotation should be unchanged
				actual, exists := updated.Annotations[agentsv1alpha1.AnnotationPodProbe]
				if len(tt.box.Spec.Probes) == 0 {
					assert.False(t, exists)
				} else {
					assert.NotEmpty(t, actual)
				}
			}
		})
	}
}

// A policy left behind after spec.probes is removed is invalid, but the probes
// themselves are not: EnsureProbe must still run so the stale Pod annotation and
// the stale probe condition are cleared. Freezing them instead would leave the
// pause decision reading a condition whose LastTransitionTime never moves again.
func TestEnsureProbe_DanglingPolicyClearsStaleState(t *testing.T) {
	enableProbeGate(t)
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))

	condType := agentsv1alpha1.ProbeConditionType("activity")
	box := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSpec{
			// spec.probes is gone; the policy still references the probe it defined.
			AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "activity",
						MessageRegex:      "^idle$",
						ThresholdDuration: &metav1.Duration{Duration: time.Minute},
					},
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default", Annotations: map[string]string{
			agentsv1alpha1.AnnotationPodProbe: `[{"name":"activity"}]`,
		}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}},
	}
	newStatus := &agentsv1alpha1.SandboxStatus{
		Conditions: []metav1.Condition{{
			Type:               condType,
			Status:             metav1.ConditionTrue,
			Reason:             agentsv1alpha1.ProbeReasonSucceeded,
			Message:            "idle",
			LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Hour)),
		}},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()
	manager := NewPodProbeManager(fakeClient, record.NewFakeRecorder(10))
	require.NoError(t, manager.EnsureProbe(context.Background(), box, pod, newStatus))

	updated := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(pod), updated))
	_, exists := updated.Annotations[agentsv1alpha1.AnnotationPodProbe]
	assert.False(t, exists, "stale probe annotation should be removed")

	assert.Nil(t, utils.GetSandboxCondition(newStatus, condType), "stale probe condition should be removed")

	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionProbeValid))
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status, "the dangling policy is still reported as invalid")
}

// TestValidateProbeConfiguration covers the controller-side reporting path. The
// probe and policy rules themselves are covered by pkg/autopause, so this only
// checks that both are validated and surfaced on the ProbeValid condition.
func TestValidateProbeConfiguration(t *testing.T) {
	validProbes := []agentsv1alpha1.Probe{
		{
			Name: "activity",
			Probe: corev1.Probe{
				ProbeHandler: corev1.ProbeHandler{
					Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
				},
			},
		},
	}
	idlePolicy := func(probe string) *agentsv1alpha1.AutoPausePolicy {
		return &agentsv1alpha1.AutoPausePolicy{
			Pause: &agentsv1alpha1.PausePolicy{
				WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
					Probe:             probe,
					MessageRegex:      "^idle$",
					ThresholdDuration: &metav1.Duration{Duration: time.Minute},
				},
			},
		}
	}

	tests := []struct {
		name string
		spec agentsv1alpha1.SandboxSpec
		// staleProbeValid seeds a ProbeValid=False left over from an earlier spec,
		// to check whether validate still reports on a configuration that is gone.
		staleProbeValid bool
		// expectProbesUsable is validate's return value: whether the probes can
		// still be applied to the Pod. A policy-only error keeps it true.
		expectProbesUsable bool
		expectCondition    metav1.ConditionStatus
		expectMessage      string
	}{
		{
			name:               "no probes and no policy - nothing to validate",
			spec:               agentsv1alpha1.SandboxSpec{},
			expectProbesUsable: true,
		},
		{
			// The user fixed the failure by deleting the configuration, so the
			// verdict on it must go too instead of staying on the status forever.
			name:               "probes and policy removed - stale verdict is dropped",
			spec:               agentsv1alpha1.SandboxSpec{},
			staleProbeValid:    true,
			expectProbesUsable: true,
		},
		{
			name:               "valid probes and policy",
			spec:               agentsv1alpha1.SandboxSpec{Probes: validProbes, AutoPausePolicy: idlePolicy("activity")},
			expectProbesUsable: true,
			expectCondition:    metav1.ConditionTrue,
		},
		{
			name: "invalid probe handler",
			spec: agentsv1alpha1.SandboxSpec{Probes: []agentsv1alpha1.Probe{{
				Name:  "activity",
				Probe: corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Path: "/health"}}},
			}}},
			expectCondition: metav1.ConditionFalse,
			expectMessage:   "Unsupported value",
		},
		{
			name:               "policy references undefined probe - probes stay usable",
			spec:               agentsv1alpha1.SandboxSpec{Probes: validProbes, AutoPausePolicy: idlePolicy("missing")},
			expectProbesUsable: true,
			expectCondition:    metav1.ConditionFalse,
			expectMessage:      "must reference a probe name defined in spec.probes",
		},
		{
			// Removing spec.probes but leaving the policy behind must not stop the
			// sync that clears the stale Pod annotation and probe conditions.
			name:               "probes removed while policy remains - probes stay usable",
			spec:               agentsv1alpha1.SandboxSpec{AutoPausePolicy: idlePolicy("activity")},
			expectProbesUsable: true,
			expectCondition:    metav1.ConditionFalse,
			expectMessage:      "must reference a probe name defined in spec.probes",
		},
		{
			name:               "policy carries no rule",
			spec:               agentsv1alpha1.SandboxSpec{Probes: validProbes, AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{}},
			expectProbesUsable: true,
			expectCondition:    metav1.ConditionFalse,
			expectMessage:      "at least one of pause.whenProbedIdleState or resume.whenProbedScheduleTime is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			box := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "box", Namespace: "default"},
				Spec:       tt.spec,
			}
			manager := NewPodProbeManager(nil, record.NewFakeRecorder(10))
			newStatus := &agentsv1alpha1.SandboxStatus{}
			if tt.staleProbeValid {
				utils.SetSandboxCondition(newStatus, metav1.Condition{
					Type:               string(agentsv1alpha1.SandboxConditionProbeValid),
					Status:             metav1.ConditionFalse,
					Reason:             agentsv1alpha1.SandboxProbeValidReasonValidationFailed,
					Message:            "probe validation failed",
					LastTransitionTime: metav1.Now(),
				})
			}

			assert.Equal(t, tt.expectProbesUsable, manager.validate(context.Background(), box, newStatus))

			cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionProbeValid))
			if tt.expectCondition == "" {
				assert.Nil(t, cond)
				return
			}
			require.NotNil(t, cond)
			assert.Equal(t, tt.expectCondition, cond.Status)
			if tt.expectMessage != "" {
				assert.Contains(t, cond.Message, tt.expectMessage)
			}
		})
	}
}

func TestFindPodCondition(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: corev1.PodConditionType(agentsv1alpha1.ProbeConditionPrefix + "activity"), Status: corev1.ConditionTrue},
			},
		},
	}

	tests := []struct {
		name     string
		condType string
		wantNil  bool
	}{
		{
			name:     "existing probe condition",
			condType: agentsv1alpha1.ProbeConditionPrefix + "activity",
			wantNil:  false,
		},
		{
			name:     "non-existing condition",
			condType: agentsv1alpha1.ProbeConditionPrefix + "nonexistent",
			wantNil:  true,
		},
		{
			name:     "built-in PodReady condition",
			condType: string(corev1.PodReady),
			wantNil:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findPodCondition(pod, tt.condType)
			if tt.wantNil {
				assert.Nil(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Equal(t, tt.condType, string(result.Type))
			}
		})
	}
}

func TestSyncConditions(t *testing.T) {
	condType := agentsv1alpha1.ProbeConditionPrefix + "activity"
	lastTransition := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	newerTransition := metav1.NewTime(time.Now().Add(-1 * time.Minute))

	validProbe := agentsv1alpha1.Probe{
		Name: "activity",
		Probe: corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				Exec: &corev1.ExecAction{Command: []string{"echo", "test"}},
			},
		},
	}

	makeBox := func(probes []agentsv1alpha1.Probe) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
			Spec:       agentsv1alpha1.SandboxSpec{Probes: probes},
		}
	}

	podWithConditionAt := func(condType string, status corev1.ConditionStatus, reason, message string, transition metav1.Time) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{
					{
						Type:               corev1.PodConditionType(condType),
						Status:             status,
						Reason:             reason,
						Message:            message,
						LastTransitionTime: transition,
					},
				},
			},
		}
	}

	podWithCondition := func(condType string, status corev1.ConditionStatus, reason, message string) *corev1.Pod {
		return podWithConditionAt(condType, status, reason, message, lastTransition)
	}

	barePod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"}}

	tests := []struct {
		name                  string
		box                   *agentsv1alpha1.Sandbox
		pod                   *corev1.Pod
		existingCond          []metav1.Condition
		wantCondCnt           int
		wantStatus            metav1.ConditionStatus
		wantReason            string
		wantMessage           string
		wantCondAbsent        string
		wantTransition        *metav1.Time
		wantTransitionAfter   *metav1.Time
		wantTransitionNonZero bool
	}{
		{
			name:        "new probe - pod condition not yet available, set Unknown",
			box:         makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:         barePod,
			wantCondCnt: 1,
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  agentsv1alpha1.ProbeReasonPending,
			wantMessage: "probe result not yet available",
		},
		{
			name:        "normal sync from pod condition",
			box:         makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:         podWithCondition(condType, corev1.ConditionTrue, agentsv1alpha1.ProbeReasonSucceeded, "inactive"),
			wantCondCnt: 1,
			wantStatus:  metav1.ConditionTrue,
			wantReason:  agentsv1alpha1.ProbeReasonSucceeded,
			wantMessage: "inactive",
		},
		{
			name: "probe removed - condition removed",
			box:  makeBox(nil),
			pod:  podWithCondition(condType, corev1.ConditionTrue, agentsv1alpha1.ProbeReasonSucceeded, "inactive"),
			existingCond: []metav1.Condition{
				{
					Type:               condType,
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.ProbeReasonSucceeded,
					Message:            "inactive",
					LastTransitionTime: lastTransition,
				},
			},
			wantCondCnt:    0,
			wantCondAbsent: condType,
		},
		{
			name: "skip when condition unchanged",
			box:  makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:  podWithCondition(condType, corev1.ConditionTrue, agentsv1alpha1.ProbeReasonSucceeded, "inactive"),
			existingCond: []metav1.Condition{
				{
					Type:               condType,
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.ProbeReasonSucceeded,
					Message:            "inactive",
					LastTransitionTime: lastTransition,
				},
			},
			wantCondCnt: 1,
			wantStatus:  metav1.ConditionTrue,
			wantReason:  agentsv1alpha1.ProbeReasonSucceeded,
			wantMessage: "inactive",
		},
		{
			name: "update existing condition when changed",
			box:  makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:  podWithCondition(condType, corev1.ConditionTrue, agentsv1alpha1.ProbeReasonSucceeded, "inactive"),
			existingCond: []metav1.Condition{
				{
					Type:               condType,
					Status:             metav1.ConditionFalse,
					Reason:             agentsv1alpha1.ProbeReasonError,
					Message:            "old message",
					LastTransitionTime: lastTransition,
				},
			},
			wantCondCnt: 1,
			wantStatus:  metav1.ConditionTrue,
			wantReason:  agentsv1alpha1.ProbeReasonSucceeded,
			wantMessage: "inactive",
		},
		{
			name: "new probe with existing Unknown - not overwritten",
			box:  makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:  barePod,
			existingCond: []metav1.Condition{
				{
					Type:               condType,
					Status:             metav1.ConditionUnknown,
					Reason:             agentsv1alpha1.ProbeReasonPending,
					Message:            "probe result not yet available",
					LastTransitionTime: lastTransition,
				},
			},
			wantCondCnt: 1,
			wantStatus:  metav1.ConditionUnknown,
			wantReason:  agentsv1alpha1.ProbeReasonPending,
			wantMessage: "probe result not yet available",
		},
		{
			name:           "empty pod reason on healthy probe - defaults to Succeeded",
			box:            makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:            podWithCondition(condType, corev1.ConditionTrue, "", "inactive"),
			wantCondCnt:    1,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     agentsv1alpha1.ProbeReasonSucceeded,
			wantMessage:    "inactive",
			wantTransition: &lastTransition,
		},
		{
			name:        "empty pod reason on failed probe - defaults to Error",
			box:         makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:         podWithCondition(condType, corev1.ConditionFalse, "", "probe failed"),
			wantCondCnt: 1,
			wantStatus:  metav1.ConditionFalse,
			wantReason:  agentsv1alpha1.ProbeReasonError,
			wantMessage: "probe failed",
		},
		{
			name:                  "zero pod transition time - filled in",
			box:                   makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:                   podWithConditionAt(condType, corev1.ConditionTrue, "", "active", metav1.Time{}),
			wantCondCnt:           1,
			wantStatus:            metav1.ConditionTrue,
			wantReason:            agentsv1alpha1.ProbeReasonSucceeded,
			wantMessage:           "active",
			wantTransitionNonZero: true,
		},
		{
			name: "message-only change advances LastTransitionTime",
			box:  makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:  podWithConditionAt(condType, corev1.ConditionTrue, "", "inactive", newerTransition),
			existingCond: []metav1.Condition{
				{
					Type:               condType,
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.ProbeReasonSucceeded,
					Message:            "active",
					LastTransitionTime: lastTransition,
				},
			},
			wantCondCnt:    1,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     agentsv1alpha1.ProbeReasonSucceeded,
			wantMessage:    "inactive",
			wantTransition: &newerTransition,
		},
		{
			// Without a pod timestamp to mirror, the recorded one has to stay put:
			// auto-pause measures its idle threshold from it, and a timestamp that
			// advances every reconcile keeps the threshold permanently in the future.
			name: "zero pod transition time, unchanged result - existing timestamp preserved",
			box:  makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:  podWithConditionAt(condType, corev1.ConditionTrue, "", "inactive", metav1.Time{}),
			existingCond: []metav1.Condition{
				{
					Type:               condType,
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.ProbeReasonSucceeded,
					Message:            "inactive",
					LastTransitionTime: lastTransition,
				},
			},
			wantCondCnt:    1,
			wantStatus:     metav1.ConditionTrue,
			wantReason:     agentsv1alpha1.ProbeReasonSucceeded,
			wantMessage:    "inactive",
			wantTransition: &lastTransition,
		},
		{
			name: "zero pod transition time, changed result - timestamp advances",
			box:  makeBox([]agentsv1alpha1.Probe{validProbe}),
			pod:  podWithConditionAt(condType, corev1.ConditionTrue, "", "inactive", metav1.Time{}),
			existingCond: []metav1.Condition{
				{
					Type:               condType,
					Status:             metav1.ConditionTrue,
					Reason:             agentsv1alpha1.ProbeReasonSucceeded,
					Message:            "active",
					LastTransitionTime: lastTransition,
				},
			},
			wantCondCnt:         1,
			wantStatus:          metav1.ConditionTrue,
			wantReason:          agentsv1alpha1.ProbeReasonSucceeded,
			wantMessage:         "inactive",
			wantTransitionAfter: &newerTransition,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newStatus := &agentsv1alpha1.SandboxStatus{
				Conditions: tt.existingCond,
			}
			manager := &PodProbeManager{}
			manager.syncConditions(tt.box, tt.pod, newStatus)

			if tt.wantCondCnt == 0 {
				if tt.wantCondAbsent != "" {
					cond := utils.GetSandboxCondition(newStatus, tt.wantCondAbsent)
					assert.Nil(t, cond)
				}
				return
			}
			cond := utils.GetSandboxCondition(newStatus, condType)
			assert.NotNil(t, cond)
			assert.Equal(t, tt.wantStatus, cond.Status)
			assert.Equal(t, tt.wantReason, cond.Reason)
			assert.Equal(t, tt.wantMessage, cond.Message)
			if tt.wantTransition != nil {
				assert.Equal(t, *tt.wantTransition, cond.LastTransitionTime)
			}
			if tt.wantTransitionAfter != nil {
				assert.True(t, cond.LastTransitionTime.After(tt.wantTransitionAfter.Time),
					"expected LastTransitionTime %v to be after %v", cond.LastTransitionTime, *tt.wantTransitionAfter)
			}
			if tt.wantTransitionNonZero {
				assert.False(t, cond.LastTransitionTime.IsZero())
			}
		})
	}
}
