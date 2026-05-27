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
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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

func TestReconciler_buildSandboxTemplateSpec(t *testing.T) {
	tests := []struct {
		name        string
		sbs         *v1alpha1.SandboxSet
		objects     []client.Object
		wantErr     bool
		errContains string
		verify      func(t *testing.T, spec *v1alpha1.SandboxTemplateSpec)
	}{
		{
			name: "inline template builds spec with all fields",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "inline", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 3,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: samplePodTemplate("img:v1", map[string]string{"app": "inline"}),
						VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
							ObjectMeta: metav1.ObjectMeta{Name: "data"},
							Spec: corev1.PersistentVolumeClaimSpec{
								AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
								Resources: corev1.VolumeResourceRequirements{
									Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
								},
							},
						}},
					},
					PersistentContents: []string{"/data"},
					Runtimes:           []v1alpha1.RuntimeConfig{{Name: "python"}},
				},
			},
			verify: func(t *testing.T, spec *v1alpha1.SandboxTemplateSpec) {
				require.NotNil(t, spec.Template)
				assert.Equal(t, "img:v1", spec.Template.Spec.Containers[0].Image)
				assert.Len(t, spec.VolumeClaimTemplates, 1)
				assert.Equal(t, "data", spec.VolumeClaimTemplates[0].Name)
				assert.Equal(t, []string{"/data"}, spec.PersistentContents)
				assert.Len(t, spec.Runtimes, 1)
				assert.Equal(t, "python", spec.Runtimes[0].Name)
			},
		},
		{
			name: "templateRef resolves template and combines with SandboxSet fields",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ref-sbs", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-a"},
					},
					PersistentContents: []string{"ip"},
					Runtimes:           []v1alpha1.RuntimeConfig{{Name: "node"}},
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
			verify: func(t *testing.T, spec *v1alpha1.SandboxTemplateSpec) {
				require.NotNil(t, spec.Template)
				assert.Equal(t, "img:v1", spec.Template.Spec.Containers[0].Image)
				assert.Equal(t, []string{"ip"}, spec.PersistentContents)
				assert.Len(t, spec.Runtimes, 1)
				assert.Equal(t, "node", spec.Runtimes[0].Name)
			},
		},
		{
			name: "templateRef with nil Template in referenced SandboxTemplate",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ref-nil-tpl", Namespace: "default"},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: 1,
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-nil"},
					},
				},
			},
			objects: []client.Object{
				&v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "tpl-nil", Namespace: "default"},
					Spec:       v1alpha1.SandboxTemplateSpec{},
				},
			},
			verify: func(t *testing.T, spec *v1alpha1.SandboxTemplateSpec) {
				assert.Nil(t, spec.Template)
				assert.Nil(t, spec.VolumeClaimTemplates)
				assert.Nil(t, spec.PersistentContents)
				assert.Nil(t, spec.Runtimes)
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
			name: "neither template nor templateRef returns an error",
			sbs: &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"},
			},
			wantErr:     true,
			errContains: "has neither spec.templateRef nor spec.template",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRevisionTestReconciler(tt.objects...)
			spec, err := r.buildSandboxTemplateSpec(context.Background(), tt.sbs)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
			require.NotNil(t, spec)
			if tt.verify != nil {
				tt.verify(t, spec)
			}
		})
	}
}

// TestComputeRevisionHash covers the hashing behaviour of computeRevisionHash
// across different SandboxTemplateSpec inputs.
func TestComputeRevisionHash(t *testing.T) {
	samePod := samplePodTemplate("img:v1", map[string]string{"app": "same"})

	tests := []struct {
		name        string
		specA       *v1alpha1.SandboxTemplateSpec
		specB       *v1alpha1.SandboxTemplateSpec
		expectEqual bool
	}{
		{
			name: "same template produces identical hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template: samePod.DeepCopy(),
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samePod.DeepCopy(),
			},
			expectEqual: true,
		},
		{
			name: "different images produce different hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v2", nil),
			},
			expectEqual: false,
		},
		{
			name: "persistentContents change produces different hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template:           samplePodTemplate("img:v1", nil),
				PersistentContents: []string{"/data"},
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
			},
			expectEqual: false,
		},
		{
			name: "runtimes change produces different hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
				Runtimes: []v1alpha1.RuntimeConfig{{Name: "python"}},
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
			},
			expectEqual: false,
		},
		{
			name: "VolumeClaimTemplates change produces different hash",
			specA: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
				VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
					ObjectMeta: metav1.ObjectMeta{Name: "data"},
				}},
			},
			specB: &v1alpha1.SandboxTemplateSpec{
				Template: samplePodTemplate("img:v1", nil),
			},
			expectEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hashA, err := computeRevisionHash(tt.specA)
			require.NoError(t, err)
			require.NotEmpty(t, hashA)

			hashB, err := computeRevisionHash(tt.specB)
			require.NoError(t, err)
			require.NotEmpty(t, hashB)

			if tt.expectEqual {
				assert.Equal(t, hashA, hashB,
					"specs describing the same content must share the same hash")
			} else {
				assert.NotEqual(t, hashA, hashB,
					"specs describing different content must have different hashes")
			}
		})
	}
}

// TestComputeRevisionHash_InlineAndTemplateRefConsistency verifies that
// an inline template and a templateRef pointing to identical content produce
// the same revision hash through the full buildSandboxTemplateSpec flow.
func TestComputeRevisionHash_InlineAndTemplateRefConsistency(t *testing.T) {
	samePod := samplePodTemplate("img:v1", map[string]string{"app": "same"})

	inlineSBS := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 1,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: samePod.DeepCopy(),
			},
			PersistentContents: []string{"/data"},
		},
	}

	refSBS := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs", Namespace: "default"},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 1,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "tpl-dup"},
			},
			PersistentContents: []string{"/data"},
		},
	}

	sbt := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "tpl-dup", Namespace: "default"},
		Spec:       v1alpha1.SandboxTemplateSpec{Template: samePod.DeepCopy()},
	}

	r := newRevisionTestReconciler(sbt)

	specInline, err := r.buildSandboxTemplateSpec(context.Background(), inlineSBS)
	require.NoError(t, err)
	hashInline, err := computeRevisionHash(specInline)
	require.NoError(t, err)

	specRef, err := r.buildSandboxTemplateSpec(context.Background(), refSBS)
	require.NoError(t, err)
	hashRef, err := computeRevisionHash(specRef)
	require.NoError(t, err)

	assert.Equal(t, hashInline, hashRef,
		"inline and templateRef describing the same content must produce identical hash")
}
