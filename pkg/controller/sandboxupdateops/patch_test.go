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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestApplySandboxPatch_SuccessfulTemplatePatch(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:2.0"},
					},
				},
			}),
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "nginx:1.0"},
						},
					},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.NoError(t, err)

	// Verify the sandbox was patched
	updated := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated)
	assert.NoError(t, err)
	assert.Equal(t, "nginx:2.0", updated.Spec.Template.Spec.Containers[0].Image)
}

func TestApplySandboxPatch_SetsUpgradePolicyRecreate(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "busybox"},
						},
					},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.NoError(t, err)

	updated := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated)
	assert.NoError(t, err)
	assert.NotNil(t, updated.Spec.UpgradePolicy)
	assert.Equal(t, agentsv1alpha1.SandboxUpgradePolicyRecreate, updated.Spec.UpgradePolicy.Type)
}

func TestApplySandboxPatch_CopiesLifecycle(t *testing.T) {
	lifecycle := &agentsv1alpha1.SandboxLifecycle{
		PreUpgrade: &agentsv1alpha1.UpgradeAction{
			Exec: &corev1.ExecAction{
				Command: []string{"/bin/bash", "-c", "backup.sh"},
			},
			TimeoutSeconds: 30,
		},
	}
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Lifecycle: lifecycle,
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "busybox"},
						},
					},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.NoError(t, err)

	updated := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated)
	assert.NoError(t, err)
	assert.NotNil(t, updated.Spec.Lifecycle)
	assert.NotNil(t, updated.Spec.Lifecycle.PreUpgrade)
	assert.Equal(t, []string{"/bin/bash", "-c", "backup.sh"}, updated.Spec.Lifecycle.PreUpgrade.Exec.Command)
	assert.Equal(t, int32(30), updated.Spec.Lifecycle.PreUpgrade.TimeoutSeconds)
}

func TestApplySandboxPatch_AddsTrackingLabel(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "my-ops", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "busybox"},
						},
					},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.NoError(t, err)

	updated := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated)
	assert.NoError(t, err)
	assert.Equal(t, "my-ops", updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
}

func TestApplySandboxPatch_InvalidPatchJSON(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: runtime.RawExtension{Raw: []byte("{invalid json}")},
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "nginx:1.0"},
						},
					},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to apply strategic merge patch")
}

func TestApplySandboxPatch_PatchAPIError(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "busybox"},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}, &agentsv1alpha1.Sandbox{}).
		WithRuntimeObjects(sbx).
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

	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated patch error")
}

func TestApplySandboxPatch_NilLabelsCreatesMap(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "busybox"},
						},
					},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.NoError(t, err)

	updated := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated)
	assert.NoError(t, err)
	assert.Equal(t, "ops-1", updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
}

func TestApplySandboxPatch_PausedSetsResumeTriggerAnnotation(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:2.0"},
					},
				},
			}),
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "nginx:1.0"},
						},
					},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPaused},
	}

	r := newTestReconciler(sbx)
	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.NoError(t, err)

	updated := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated)
	assert.NoError(t, err)
	// Phase 1: annotation set, template NOT patched
	assert.Equal(t, agentsv1alpha1.True, updated.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
	assert.Equal(t, "nginx:1.0", updated.Spec.Template.Spec.Containers[0].Image)
	assert.NotNil(t, updated.Spec.UpgradePolicy)
	assert.Equal(t, agentsv1alpha1.SandboxUpgradePolicyRecreate, updated.Spec.UpgradePolicy.Type)
}

func TestApplyTemplatePatch_Success(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:2.0"},
					},
				},
			}),
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True,
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "nginx:1.0"},
						},
					},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	err := r.applyTemplatePatch(context.Background(), sbx, ops)
	assert.NoError(t, err)

	updated := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated)
	assert.NoError(t, err)
	// Phase 2: template patched, annotation removed
	assert.Equal(t, "nginx:2.0", updated.Spec.Template.Spec.Containers[0].Image)
	_, exists := updated.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger]
	assert.False(t, exists)
}

func TestApplyTemplatePatch_PatchError(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "main", Image: "nginx:2.0"},
					},
				},
			}),
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True,
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "nginx:1.0"},
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithStatusSubresource(&agentsv1alpha1.SandboxUpdateOps{}, &agentsv1alpha1.Sandbox{}).
		WithRuntimeObjects(sbx).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cli client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				return fmt.Errorf("simulated patch error")
			},
		}).
		Build()
	r := &Reconciler{
		Client:   fakeClient,
		Scheme:   testScheme,
		Recorder: record.NewFakeRecorder(100),
	}
	err := r.applyTemplatePatch(context.Background(), sbx, ops)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "simulated patch error")
}

func TestSanitizeTemplatePatch(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "annotation-only patch drops null containers",
			raw:  `{"metadata":{"annotations":{"agents.kruise.io/upgrade-marker":"v2"}},"spec":{"containers":null}}`,
			want: `{"metadata":{"annotations":{"agents.kruise.io/upgrade-marker":"v2"}}}`,
		},
		{
			name: "patch with real containers is unchanged",
			raw:  `{"spec":{"containers":[{"name":"main","image":"nginx:2.0"}]}}`,
			want: `{"spec":{"containers":[{"name":"main","image":"nginx:2.0"}]}}`,
		},
		{
			name: "null optional lists are kept as delete directives",
			raw:  `{"spec":{"containers":[{"name":"main"}],"initContainers":null,"volumes":null}}`,
			want: `{"spec":{"containers":[{"name":"main"}],"initContainers":null,"volumes":null}}`,
		},
		{
			name: "invalid json is returned unchanged",
			raw:  `{invalid json}`,
			want: `{invalid json}`,
		},
		{
			name: "patch without spec is unchanged",
			raw:  `{"metadata":{"labels":{"app":"test"}}}`,
			want: `{"metadata":{"labels":{"app":"test"}}}`,
		},
		{
			// The sanitize round trip must not corrupt int64 fields beyond
			// float64 precision (2^53), e.g. activeDeadlineSeconds or runAsUser.
			name: "large integers survive the round trip",
			raw:  `{"spec":{"containers":null,"activeDeadlineSeconds":9007199254740993,"securityContext":{"runAsUser":1000000000000000001}}}`,
			want: `{"spec":{"activeDeadlineSeconds":9007199254740993,"securityContext":{"runAsUser":1000000000000000001}}}`,
		},
		{
			name: "typical patch with ordinary numbers is unchanged",
			raw:  `{"spec":{"containers":[{"name":"main","image":"nginx:2.0"}],"terminationGracePeriodSeconds":30}}`,
			want: `{"spec":{"containers":[{"name":"main","image":"nginx:2.0"}],"terminationGracePeriodSeconds":30}}`,
		},
		{
			name: "empty object is unchanged",
			raw:  `{}`,
			want: `{}`,
		},
		{
			name: "explicit null spec is unchanged",
			raw:  `{"metadata":{"labels":{"app":"test"}},"spec":null}`,
			want: `{"metadata":{"labels":{"app":"test"}},"spec":null}`,
		},
		{
			name: "spec not an object is unchanged",
			raw:  `{"spec":"invalid"}`,
			want: `{"spec":"invalid"}`,
		},
		{
			name: "non-object json root is unchanged",
			raw:  `["not","an","object"]`,
			want: `["not","an","object"]`,
		},
		{
			// An explicit empty list is a user-authored delete-all directive
			// and must not be confused with the marshaling null artifact.
			name: "empty containers list is kept as delete-all directive",
			raw:  `{"spec":{"containers":[]}}`,
			want: `{"spec":{"containers":[]}}`,
		},
		{
			name: "null containers dropped while other spec fields are kept",
			raw:  `{"spec":{"containers":null,"nodeSelector":{"zone":"a"}}}`,
			want: `{"spec":{"nodeSelector":{"zone":"a"}}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, string(sanitizeTemplatePatch([]byte(tt.raw))))
		})
	}
}

// TestApplySandboxPatch_AnnotationOnlyPatchKeepsContainers guards against the
// regression where marshaling a PodTemplateSpec that only sets ObjectMeta
// emits "spec":{"containers":null} (PodSpec.Containers has no omitempty) and
// the strategic merge patch treated it as a delete directive, wiping the
// required containers of the sandbox template.
func TestApplySandboxPatch_AnnotationOnlyPatchKeepsContainers(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"agents.kruise.io/upgrade-marker": "v2"},
				},
			}),
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{"app": "test"},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "nginx:1.0"},
						},
					},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	err := r.applySandboxPatch(context.Background(), sbx, ops)
	assert.NoError(t, err)

	updated := &agentsv1alpha1.Sandbox{}
	err = r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated)
	assert.NoError(t, err)
	assert.Equal(t, "v2", updated.Spec.Template.Annotations["agents.kruise.io/upgrade-marker"])
	assert.Len(t, updated.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, "nginx:1.0", updated.Spec.Template.Spec.Containers[0].Image)
}

func TestIsSandboxTemplateMatchPatch_AnnotationOnlyPatch(t *testing.T) {
	newSandbox := func(annotations map[string]string) *agentsv1alpha1.Sandbox {
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
			Spec: agentsv1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Annotations: annotations},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{Name: "main", Image: "nginx:1.0"},
							},
						},
					},
				},
			},
		}
	}
	ops := &agentsv1alpha1.SandboxUpdateOps{
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{"agents.kruise.io/upgrade-marker": "v2"},
				},
			}),
		},
	}

	assert.False(t, isSandboxTemplateMatchPatch(newSandbox(nil), ops),
		"sandbox without the annotation needs an upgrade")
	assert.True(t, isSandboxTemplateMatchPatch(newSandbox(map[string]string{"agents.kruise.io/upgrade-marker": "v2"}), ops),
		"sandbox already carrying the annotation must be skipped")
}
