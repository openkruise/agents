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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/api/v1alpha1"
)

// newRevisionTestReconciler builds a Reconciler backed by a fake client for
// revision-related unit tests.
func newRevisionTestReconciler(objs ...client.Object) *Reconciler {
	c := fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).Build()
	return &Reconciler{
		Client: c,
		Scheme: testScheme,
		Codec:  codec,
	}
}

// samplePodTemplate returns a minimal pod template with the given image and
// optional labels, used to compose SandboxSet / SandboxTemplate fixtures.
func samplePodTemplate(image string, labels map[string]string) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: image}},
		},
	}
}

func TestReconciler_getPatch(t *testing.T) {
	tests := []struct {
		name    string
		sbs     *v1alpha1.SandboxSet
		objects []client.Object
		wantErr bool
		// errContains is asserted only when wantErr is true.
		errContains string
		// verify runs only when wantErr is false and inspects the decoded
		// `spec` object returned by getPatch.
		verify func(t *testing.T, spec map[string]interface{})
	}{
		{
			name: "inline template is normalised to spec.template with $patch=replace",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "inline", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: samplePodTemplate("img:v1", map[string]string{"app": "inline"}),
					},
				},
			},
			verify: func(t *testing.T, spec map[string]interface{}) {
				template, ok := spec["template"].(map[string]interface{})
				require.True(t, ok, "spec.template must be present for inline template")
				assert.Equal(t, "replace", template["$patch"])
				_, refExists := spec["templateRef"]
				assert.False(t, refExists, "spec.templateRef must not appear in specCopy")
			},
		},
		{
			name: "templateRef resolves and normalises to spec.template",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ref-sbs", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-a"},
					},
				},
			},
			objects: []client.Object{
				&v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "tpl-a", Namespace: "default"},
					Spec: v1alpha1.SandboxTemplateSpec{
						Template: samplePodTemplate("img:v1", map[string]string{"app": "ref"}),
					},
				},
			},
			verify: func(t *testing.T, spec map[string]interface{}) {
				template, ok := spec["template"].(map[string]interface{})
				require.True(t, ok, "templateRef branch should normalise to spec.template")
				assert.Equal(t, "replace", template["$patch"])
				// spec.templateRef must NOT be written: the hash key is normalised
				// to spec.template regardless of inline vs templateRef.
				_, refExists := spec["templateRef"]
				assert.False(t, refExists, "spec.templateRef must not appear in specCopy")
			},
		},
		{
			name: "templateRef not found propagates a resolve error",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ref-missing", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "does-not-exist"},
					},
				},
			},
			wantErr:     true,
			errContains: "failed to resolve sandbox template",
		},
		{
			name: "neither template nor templateRef yields spec without template",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
			},
			verify: func(t *testing.T, spec map[string]interface{}) {
				_, tplExists := spec["template"]
				assert.False(t, tplExists, "spec.template must be absent when neither source is set")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRevisionTestReconciler(tt.objects...)
			raw, err := r.getPatch(context.Background(), tt.sbs)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)

			var decoded map[string]interface{}
			require.NoError(t, json.Unmarshal(raw, &decoded))
			spec, ok := decoded["spec"].(map[string]interface{})
			require.True(t, ok, "spec must be present")
			if tt.verify != nil {
				tt.verify(t, spec)
			}
		})
	}
}

// TestReconciler_newRevision covers the hashing behaviour of newRevision across
// inline / templateRef combinations and the error path when a referenced
// SandboxTemplate is missing.
//
// When compareWith is non-nil, the test computes a revision for both sbs and
// compareWith using the shared reconciler, and asserts hash equality per
// expectEqual. When wantErr is true, only sbs is invoked and the error is
// asserted.
func TestReconciler_newRevision(t *testing.T) {
	samePod := samplePodTemplate("img:v1", map[string]string{"app": "same"})

	tests := []struct {
		name        string
		objects     []client.Object
		sbs         *v1alpha1.SandboxSet
		compareWith *v1alpha1.SandboxSet
		expectEqual bool
		wantErr     bool
		errContains string
	}{
		{
			name: "inline and templateRef describing the same pod produce identical hash",
			objects: []client.Object{
				&v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "tpl-dup", Namespace: "default"},
					Spec:       v1alpha1.SandboxTemplateSpec{Template: samePod.DeepCopy()},
				},
			},
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: samePod.DeepCopy(),
					},
				},
			},
			compareWith: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "dup", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-dup"},
					},
				},
			},
			expectEqual: true,
		},
		{
			name: "different referenced SandboxTemplates produce different hash",
			objects: []client.Object{
				&v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "tpl-v1", Namespace: "default"},
					Spec:       v1alpha1.SandboxTemplateSpec{Template: samplePodTemplate("img:v1", nil)},
				},
				&v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "tpl-v2", Namespace: "default"},
					Spec:       v1alpha1.SandboxTemplateSpec{Template: samplePodTemplate("img:v2", nil)},
				},
			},
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "diff", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-v1"},
					},
				},
			},
			compareWith: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "diff", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-v2"},
					},
				},
			},
			expectEqual: false,
		},
		{
			name: "templateRef not found returns resolve error",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "missing", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "no-such-tpl"},
					},
				},
			},
			wantErr:     true,
			errContains: "failed to resolve sandbox template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRevisionTestReconciler(tt.objects...)

			crA, err := r.newRevision(context.Background(), tt.sbs, 1, nil)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
			hashA := crA.Labels[ControllerRevisionHashLabel]
			require.NotEmpty(t, hashA)

			if tt.compareWith == nil {
				return
			}
			crB, err := r.newRevision(context.Background(), tt.compareWith, 1, nil)
			require.NoError(t, err)
			hashB := crB.Labels[ControllerRevisionHashLabel]
			require.NotEmpty(t, hashB)

			if tt.expectEqual {
				assert.Equal(t, hashA, hashB,
					"revisions describing the same pod template must share the same hash")
			} else {
				assert.NotEqual(t, hashA, hashB,
					"revisions describing different pod templates must have different hashes")
			}
		})
	}
}
