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

package sandboxpool

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestNewSandboxFromSandboxSet(t *testing.T) {
	tests := []struct {
		name                       string
		sandboxSet                 *agentsv1alpha1.SandboxSet
		refTemplate                *agentsv1alpha1.SandboxTemplate
		expectedGenerateName       string
		expectedNamespace          string
		expectedLabels             map[string]string
		expectedAnnotations        map[string]string
		expectedRuntimes           []agentsv1alpha1.RuntimeConfig
		expectedTemplateRef        *agentsv1alpha1.SandboxTemplateRef
		expectedPersistentContents []string
		expectedPauseStrategy      *agentsv1alpha1.PauseStrategy
		expectedProbes             []agentsv1alpha1.Probe
		expectedAutoPausePolicy    *agentsv1alpha1.AutoPausePolicy
	}{
		{
			name: "basic sandboxset without templateRef",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
			},
			expectedGenerateName: "test-sbs-",
			expectedNamespace:    "default",
			expectedLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      "test-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "test-sbs",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
				agentsv1alpha1.LabelManagedBy:        agentsv1alpha1.ManagedBySandboxSetController,
			},
			expectedAnnotations:        map[string]string{},
			expectedRuntimes:           nil,
			expectedTemplateRef:        nil,
			expectedPersistentContents: nil,
		},
		{
			name: "sandboxset with runtimes and persistentContents",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "runtime-sbs",
					Namespace: "runtime-ns",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas:           2,
					PersistentContents: []string{"ip", "memory"},
					Runtimes: []agentsv1alpha1.RuntimeConfig{
						{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
						{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
			},
			expectedGenerateName: "runtime-sbs-",
			expectedNamespace:    "runtime-ns",
			expectedLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      "runtime-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "runtime-sbs",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
				agentsv1alpha1.LabelManagedBy:        agentsv1alpha1.ManagedBySandboxSetController,
			},
			expectedAnnotations: map[string]string{},
			expectedRuntimes: []agentsv1alpha1.RuntimeConfig{
				{Name: agentsv1alpha1.RuntimeConfigForInjectCsiMount},
				{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime},
			},
			expectedTemplateRef:        nil,
			expectedPersistentContents: []string{"ip", "memory"},
		},
		{
			name: "sandboxset with pauseStrategy",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "pause-sbs",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					PauseStrategy: &agentsv1alpha1.PauseStrategy{
						Type: agentsv1alpha1.PauseStrategyHibernate,
						HibernateStrategy: &agentsv1alpha1.HibernateStrategy{
							Type: agentsv1alpha1.HibernateStrategySnapshot,
						},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
			},
			expectedGenerateName: "pause-sbs-",
			expectedNamespace:    "default",
			expectedLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      "pause-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "pause-sbs",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
				agentsv1alpha1.LabelManagedBy:        agentsv1alpha1.ManagedBySandboxSetController,
			},
			expectedAnnotations:        map[string]string{},
			expectedRuntimes:           nil,
			expectedTemplateRef:        nil,
			expectedPersistentContents: nil,
			expectedPauseStrategy: &agentsv1alpha1.PauseStrategy{
				Type: agentsv1alpha1.PauseStrategyHibernate,
				HibernateStrategy: &agentsv1alpha1.HibernateStrategy{
					Type: agentsv1alpha1.HibernateStrategySnapshot,
				},
			},
		},
		{
			name: "sandboxset with probes and autoPausePolicy",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "probe-sbs",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					Probes: []agentsv1alpha1.Probe{
						{
							Name:          "Active",
							ContainerName: "sandbox",
							Probe: corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "check-active"}},
								},
								PeriodSeconds: 30,
							},
						},
					},
					AutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
						Pause: &agentsv1alpha1.PausePolicy{
							WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
								Probe:             "Active",
								MessageRegex:      "^idle$",
								ThresholdDuration: &metav1.Duration{Duration: 10 * time.Minute},
							},
						},
					},
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
			},
			expectedGenerateName: "probe-sbs-",
			expectedNamespace:    "default",
			expectedLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      "probe-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "probe-sbs",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
				agentsv1alpha1.LabelManagedBy:        agentsv1alpha1.ManagedBySandboxSetController,
			},
			expectedAnnotations:        map[string]string{},
			expectedRuntimes:           nil,
			expectedTemplateRef:        nil,
			expectedPersistentContents: nil,
			expectedProbes: []agentsv1alpha1.Probe{
				{
					Name:          "Active",
					ContainerName: "sandbox",
					Probe: corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "check-active"}},
						},
						PeriodSeconds: 30,
					},
				},
			},
			expectedAutoPausePolicy: &agentsv1alpha1.AutoPausePolicy{
				Pause: &agentsv1alpha1.PausePolicy{
					WhenProbedIdleState: &agentsv1alpha1.ProbedIdleStateRule{
						Probe:             "Active",
						MessageRegex:      "^idle$",
						ThresholdDuration: &metav1.Duration{Duration: 10 * time.Minute},
					},
				},
			},
		},
		{
			name: "sandboxset with template labels and annotations",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "labeled-sbs",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app":  "myapp",
									"tier": "backend",
								},
								Annotations: map[string]string{
									"description": "test sandbox",
								},
							},
						},
					},
				},
			},
			expectedGenerateName: "labeled-sbs-",
			expectedNamespace:    "default",
			expectedLabels: map[string]string{
				"app":                                "myapp",
				"tier":                               "backend",
				agentsv1alpha1.LabelSandboxPool:      "labeled-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "labeled-sbs",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
				agentsv1alpha1.LabelManagedBy:        agentsv1alpha1.ManagedBySandboxSetController,
			},
			expectedAnnotations: map[string]string{
				"description": "test sandbox",
			},
			expectedRuntimes:           nil,
			expectedTemplateRef:        nil,
			expectedPersistentContents: nil,
		},
		{
			name: "sandboxset with internal prefix labels should be cleared",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "internal-sbs",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							ObjectMeta: metav1.ObjectMeta{
								Labels: map[string]string{
									"app":                         "myapp",
									agentsv1alpha1.LabelSandboxID: "must-be-removed",
									agentsv1alpha1.InternalPrefix + "old-label": "should-be-removed",
								},
								Annotations: map[string]string{
									"description": "test",
									agentsv1alpha1.InternalPrefix + "old-annotation": "should-be-removed",
								},
							},
						},
					},
				},
			},
			expectedGenerateName: "internal-sbs-",
			expectedNamespace:    "default",
			expectedLabels: map[string]string{
				"app":                                "myapp",
				agentsv1alpha1.LabelSandboxPool:      "internal-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "internal-sbs",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
				agentsv1alpha1.LabelManagedBy:        agentsv1alpha1.ManagedBySandboxSetController,
			},
			expectedAnnotations: map[string]string{
				"description": "test",
			},
			expectedRuntimes:           nil,
			expectedTemplateRef:        nil,
			expectedPersistentContents: nil,
		},
		{
			name: "templateRef with refTemplate inherits labels and annotations",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ref-sbs",
					Namespace: "ref-ns",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: "my-template"},
					},
				},
			},
			refTemplate: &agentsv1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-template",
					Namespace: "ref-ns",
				},
				Spec: agentsv1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app":  "from-template",
								"tier": "web",
							},
							Annotations: map[string]string{
								"source": "sandbox-template",
							},
						},
					},
				},
			},
			expectedGenerateName: "ref-sbs-",
			expectedNamespace:    "ref-ns",
			expectedLabels: map[string]string{
				"app":                                "from-template",
				"tier":                               "web",
				agentsv1alpha1.LabelSandboxPool:      "ref-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "my-template",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
				agentsv1alpha1.LabelManagedBy:        agentsv1alpha1.ManagedBySandboxSetController,
			},
			expectedAnnotations: map[string]string{
				"source": "sandbox-template",
			},
			expectedRuntimes:           nil,
			expectedTemplateRef:        nil,
			expectedPersistentContents: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := NewSandboxFromSandboxSet(tt.sandboxSet, tt.refTemplate)

			// Verify GenerateName
			assert.Equal(t, tt.expectedGenerateName, sandbox.GenerateName, "GenerateName mismatch")

			// Verify Namespace
			assert.Equal(t, tt.expectedNamespace, sandbox.Namespace, "Namespace mismatch")

			// Verify Labels
			assert.Equal(t, tt.expectedLabels, sandbox.Labels, "Labels mismatch")

			// Verify Annotations
			assert.Equal(t, tt.expectedAnnotations, sandbox.Annotations, "Annotations mismatch")

			// Verify Runtimes
			assert.Equal(t, tt.expectedRuntimes, sandbox.Spec.Runtimes, "Runtimes mismatch")

			// Verify TemplateRef
			assert.Equal(t, tt.expectedTemplateRef, sandbox.Spec.TemplateRef, "TemplateRef mismatch")

			// Verify PersistentContents
			assert.Equal(t, tt.expectedPersistentContents, sandbox.Spec.PersistentContents, "PersistentContents mismatch")

			// Verify PauseStrategy
			assert.Equal(t, tt.expectedPauseStrategy, sandbox.Spec.PauseStrategy, "PauseStrategy mismatch")

			// Verify Probes
			assert.Equal(t, tt.expectedProbes, sandbox.Spec.Probes, "Probes mismatch")

			// Verify AutoPausePolicy
			assert.Equal(t, tt.expectedAutoPausePolicy, sandbox.Spec.AutoPausePolicy, "AutoPausePolicy mismatch")

			// Verify internal labels are set correctly
			assert.Equal(t, "false", sandbox.Labels[agentsv1alpha1.LabelSandboxIsClaimed], "LabelSandboxIsClaimed should be false")
			assert.Equal(t, tt.sandboxSet.Name, sandbox.Labels[agentsv1alpha1.LabelSandboxPool], "LabelSandboxPool should match SandboxSet name")
		})
	}
}

func TestClearAndInitInnerKeys(t *testing.T) {
	tests := []struct {
		name     string
		input    map[string]string
		expected map[string]string
	}{
		{
			name:     "nil map returns empty map",
			input:    nil,
			expected: map[string]string{},
		},
		{
			name:     "empty map returns empty map",
			input:    map[string]string{},
			expected: map[string]string{},
		},
		{
			name: "internal keys are deleted except preserved ones",
			input: map[string]string{
				agentsv1alpha1.InternalPrefix + "cleanup-enabled":           "true",
				agentsv1alpha1.InternalPrefix + "cleanup-retain-on-failure": "5m",
				agentsv1alpha1.InternalPrefix + "cleanup-candidate":         "true",
				agentsv1alpha1.InternalPrefix + "sandbox-pool":              "test-pool",
				agentsv1alpha1.LabelSandboxID:                               "must-be-removed",
			},
			expected: map[string]string{
				agentsv1alpha1.InternalPrefix + "cleanup-enabled":           "true",
				agentsv1alpha1.InternalPrefix + "cleanup-retain-on-failure": "5m",
			},
		},
		{
			name: "non-internal keys are preserved",
			input: map[string]string{
				"app":                                "my-app",
				"agents.kruise.io/cleanup-candidate": "true",
			},
			expected: map[string]string{
				"app": "my-app",
			},
		},
		{
			name: "mix of internal, preserved, and non-internal keys",
			input: map[string]string{
				"app": "my-app",
				agentsv1alpha1.InternalPrefix + "cleanup-enabled":   "true",
				agentsv1alpha1.InternalPrefix + "cleanup-candidate": "true",
			},
			expected: map[string]string{
				"app": "my-app",
				agentsv1alpha1.InternalPrefix + "cleanup-enabled": "true",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := clearAndInitInnerKeys(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// TestNewSandboxFromSandboxSetPodTemplateNotPolluted is a regression test for a
// map-aliasing bug: the Sandbox metadata labels/annotations must not share maps
// with spec.template. Pool and template identity labels are copied explicitly,
// while metadata-only labels must not leak into the Pod template.
func TestNewSandboxFromSandboxSetPodTemplateNotPolluted(t *testing.T) {
	userLabels := map[string]string{"network-open": "true"}
	userAnnotations := map[string]string{"team": "platform"}
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-a", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSetSpec{
			Replicas: 1,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: userLabels, Annotations: userAnnotations},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}},
				},
			},
		},
	}

	sbx := NewSandboxFromSandboxSet(sbs, nil)
	// Simulate the internal label write performed by createSandbox afterwards.
	sbx.Labels[agentsv1alpha1.LabelTemplateHash] = "rev-1"

	// Metadata carries user labels plus internal labels.
	assert.Equal(t, "true", sbx.Labels["network-open"])
	assert.Equal(t, "pool-a", sbx.Labels[agentsv1alpha1.LabelSandboxPool])
	assert.Equal(t, "rev-1", sbx.Labels[agentsv1alpha1.LabelTemplateHash])

	// The Pod template carries the explicitly propagated identity labels, but
	// metadata-only labels such as claimed and template-hash must not leak into it.
	assert.Equal(t, map[string]string{
		"network-open":                      "true",
		agentsv1alpha1.LabelSandboxPool:     "pool-a",
		agentsv1alpha1.LabelSandboxTemplate: "pool-a",
	}, sbx.Spec.Template.Labels)
	assert.NotContains(t, sbx.Spec.Template.Labels, agentsv1alpha1.LabelSandboxIsClaimed)
	assert.NotContains(t, sbx.Spec.Template.Labels, agentsv1alpha1.LabelTemplateHash)
	assert.Equal(t, userAnnotations, sbx.Spec.Template.Annotations,
		"internal annotations must not leak into spec.template.annotations")
	// The source SandboxSet spec must stay untouched as well.
	assert.Equal(t, userLabels, sbs.Spec.Template.Labels)

	// templateRef path: the referenced SandboxTemplate must never be mutated.
	refLabels := map[string]string{"app": "from-template"}
	refTemplate := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "my-template", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Template: &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: refLabels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "nginx"}}},
			},
		},
	}
	sbsRef := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pool-b", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSetSpec{
			Replicas: 1,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: "my-template"},
			},
		},
	}
	sbxRef := NewSandboxFromSandboxSet(sbsRef, refTemplate)
	sbxRef.Labels[agentsv1alpha1.LabelTemplateHash] = "rev-2"
	require.NotNil(t, sbxRef.Spec.Template)
	assert.Nil(t, sbxRef.Spec.TemplateRef)
	assert.Equal(t, map[string]string{
		"app":                               "from-template",
		agentsv1alpha1.LabelSandboxPool:     "pool-b",
		agentsv1alpha1.LabelSandboxTemplate: "my-template",
	}, sbxRef.Spec.Template.Labels)
	assert.Equal(t, refLabels, refTemplate.Spec.Template.Labels,
		"referenced SandboxTemplate must not be mutated")
}

func TestNewSandboxFromSandboxSetCopiesLifecycle(t *testing.T) {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "pool", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSetSpec{
			Lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: &agentsv1alpha1.LifecycleAction{
				Exec: &corev1.ExecAction{Command: []string{"flush"}},
			}},
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{Template: &corev1.PodTemplateSpec{}},
		},
	}

	sandbox := NewSandboxFromSandboxSet(sbs, nil)
	require.NotNil(t, sandbox.Spec.Lifecycle)
	require.NotNil(t, sandbox.Spec.Lifecycle.PrePause)
	assert.Equal(t, []string{"flush"}, sandbox.Spec.Lifecycle.PrePause.Exec.Command)

	sandbox.Spec.Lifecycle.PrePause.Exec.Command[0] = "changed"
	assert.Equal(t, []string{"flush"}, sbs.Spec.Lifecycle.PrePause.Exec.Command, "materialization must not mutate the SandboxSet cache object")
}
