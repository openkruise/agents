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

package sandboxset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

func TestCalculateSandboxSetStatusFromGroup(t *testing.T) {
	tests := []struct {
		name              string
		initialStatus     *agentsv1alpha1.SandboxSetStatus
		groups            GroupedSandboxes
		dirtyScaleUp      map[expectations.ScaleAction][]string
		expectedReplicas  int32
		expectedAvailable int32
		description       string
	}{
		{
			name: "empty groups and no dirty scale up",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating:  []*agentsv1alpha1.Sandbox{},
				Available: []*agentsv1alpha1.Sandbox{},
				Used:      []*agentsv1alpha1.Sandbox{},
				Dead:      []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  0,
			expectedAvailable: 0,
			description:       "should have 0 replicas and 0 available when all groups are empty",
		},
		{
			name: "only available sandboxes",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  3,
			expectedAvailable: 3,
			description:       "should count 3 available sandboxes",
		},
		{
			name: "only creating sandboxes",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
				},
				Available: []*agentsv1alpha1.Sandbox{},
				Used:      []*agentsv1alpha1.Sandbox{},
				Dead:      []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  2,
			expectedAvailable: 0,
			description:       "should count creating sandboxes in replicas but not in available",
		},
		{
			name: "creating and available sandboxes",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-4"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-5"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  5,
			expectedAvailable: 3,
			description:       "should count both creating and available sandboxes",
		},
		{
			name: "with dirty scale up (expectations not satisfied)",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp: map[expectations.ScaleAction][]string{
				expectations.Create: {"sandbox-pending-1", "sandbox-pending-2"},
			},
			expectedReplicas:  5,
			expectedAvailable: 2,
			description:       "should include dirty scale up in replicas count",
		},
		{
			name: "used sandboxes should not be counted",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
				},
				Used: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
				},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  1,
			expectedAvailable: 1,
			description:       "should not count used sandboxes",
		},
		{
			name: "dead sandboxes should not be counted",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-dead-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-dead-2"}},
				},
			},
			dirtyScaleUp:      map[expectations.ScaleAction][]string{},
			expectedReplicas:  1,
			expectedAvailable: 1,
			description:       "should not count dead sandboxes",
		},
		{
			name: "all types of sandboxes combined",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-creating-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-creating-2"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-available-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-available-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-available-3"}},
				},
				Used: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-used-1"}},
				},
				Dead: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-dead-1"}},
				},
			},
			dirtyScaleUp: map[expectations.ScaleAction][]string{
				expectations.Create: {"sandbox-pending-1"},
			},
			expectedReplicas:  6,
			expectedAvailable: 3,
			description:       "should only count creating + available + dirtyCreate for replicas, and available for availableReplicas",
		},
		{
			name: "large scale with dirty scale up",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-3"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-4"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-5"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-6"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-7"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-8"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-9"}},
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-10"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp: map[expectations.ScaleAction][]string{
				expectations.Create: {
					"sandbox-pending-1",
					"sandbox-pending-2",
					"sandbox-pending-3",
				},
			},
			expectedReplicas:  13,
			expectedAvailable: 5,
			description:       "should correctly count large numbers of sandboxes",
		},
		{
			name: "dirty scale up with delete action should not affect replicas",
			initialStatus: &agentsv1alpha1.SandboxSetStatus{
				Replicas:          0,
				AvailableReplicas: 0,
			},
			groups: GroupedSandboxes{
				Creating: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-1"}},
				},
				Available: []*agentsv1alpha1.Sandbox{
					{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-2"}},
				},
				Used: []*agentsv1alpha1.Sandbox{},
				Dead: []*agentsv1alpha1.Sandbox{},
			},
			dirtyScaleUp: map[expectations.ScaleAction][]string{
				expectations.Create: {"sandbox-pending-1"},
				expectations.Delete: {"sandbox-to-delete-1", "sandbox-to-delete-2"},
			},
			expectedReplicas:  3,
			expectedAvailable: 1,
			description:       "should only count Create dirty expectations, not Delete",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			// Make a copy of the initial status
			status := tt.initialStatus.DeepCopy()

			// Call the function
			calculateSandboxSetStatusFromGroup(ctx, status, tt.groups, tt.dirtyScaleUp)

			// Assert results
			assert.Equal(t, tt.expectedReplicas, status.Replicas, tt.description+" - replicas mismatch")
			assert.Equal(t, tt.expectedAvailable, status.AvailableReplicas, tt.description+" - availableReplicas mismatch")

			// Additional validation
			assert.GreaterOrEqual(t, status.Replicas, status.AvailableReplicas,
				"replicas should be >= availableReplicas")
		})
	}
}

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
			},
			expectedAnnotations:        map[string]string{},
			expectedRuntimes:           nil,
			expectedTemplateRef:        nil,
			expectedPersistentContents: nil,
		},
		{
			name: "sandboxset with templateRef",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbs",
					Namespace: "test-ns",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 5,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
						TemplateRef: &agentsv1alpha1.SandboxTemplateRef{
							Name: "my-template",
						},
					},
				},
			},
			expectedGenerateName: "test-sbs-",
			expectedNamespace:    "test-ns",
			expectedLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      "test-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "my-template",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
			},
			expectedAnnotations: map[string]string{},
			expectedRuntimes:    nil,
			expectedTemplateRef: &agentsv1alpha1.SandboxTemplateRef{
				Name: "my-template",
			},
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
									"app": "myapp",
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
			},
			expectedAnnotations: map[string]string{
				"source": "sandbox-template",
			},
			expectedRuntimes:           nil,
			expectedTemplateRef:        &agentsv1alpha1.SandboxTemplateRef{Name: "my-template"},
			expectedPersistentContents: nil,
		},
		{
			name: "templateRef with nil refTemplate does not panic",
			sandboxSet: &agentsv1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "nil-ref-sbs",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: "missing"},
					},
				},
			},
			refTemplate:          nil,
			expectedGenerateName: "nil-ref-sbs-",
			expectedNamespace:    "default",
			expectedLabels: map[string]string{
				agentsv1alpha1.LabelSandboxPool:      "nil-ref-sbs",
				agentsv1alpha1.LabelSandboxTemplate:  "missing",
				agentsv1alpha1.LabelSandboxIsClaimed: "false",
			},
			expectedAnnotations:        map[string]string{},
			expectedRuntimes:           nil,
			expectedTemplateRef:        &agentsv1alpha1.SandboxTemplateRef{Name: "missing"},
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

			// Verify internal labels are set correctly
			assert.Equal(t, "false", sandbox.Labels[agentsv1alpha1.LabelSandboxIsClaimed], "LabelSandboxIsClaimed should be false")
			assert.Equal(t, tt.sandboxSet.Name, sandbox.Labels[agentsv1alpha1.LabelSandboxPool], "LabelSandboxPool should match SandboxSet name")
		})
	}
}
