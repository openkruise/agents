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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
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
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "busybox:2"}},
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
			Lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: &agentsv1alpha1.LifecycleAction{
				Exec: &corev1.ExecAction{Command: []string{"flush"}},
			}},
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
	require.NotNil(t, updated.Spec.Lifecycle.PrePause)
	assert.Equal(t, []string{"flush"}, updated.Spec.Lifecycle.PrePause.Exec.Command)
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

func TestIsSandboxOpsApplied_AnnotationOnlyPatch(t *testing.T) {
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

	assert.False(t, isSandboxOpsApplied(newSandbox(nil), ops),
		"sandbox without the annotation needs an upgrade")
	assert.True(t, isSandboxOpsApplied(newSandbox(map[string]string{"agents.kruise.io/upgrade-marker": "v2"}), ops),
		"sandbox already carrying the annotation must be skipped")
}

func TestApplySandboxPatch_HookOnly_MergesHooksWithoutUpgrade(t *testing.T) {
	hooks := &agentsv1alpha1.SandboxLifecycle{
		PrePause: &agentsv1alpha1.LifecycleAction{
			Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "flush.sh"}},
		},
		PostResume: &agentsv1alpha1.LifecycleAction{
			Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "restore.sh"}},
		},
	}
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec:       agentsv1alpha1.SandboxUpdateOpsSpec{Lifecycle: hooks},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSpec{
			Lifecycle: &agentsv1alpha1.SandboxLifecycle{
				PreUpgrade: &agentsv1alpha1.LifecycleAction{
					Exec: &corev1.ExecAction{Command: []string{"backup"}},
				},
				PostUpgrade: &agentsv1alpha1.LifecycleAction{
					Exec: &corev1.ExecAction{Command: []string{"restore"}},
				},
			},
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox"}}},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	require.NoError(t, r.applySandboxPatch(context.Background(), sbx, ops))

	updated := &agentsv1alpha1.Sandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated))
	require.NotNil(t, updated.Spec.Lifecycle)
	assert.Equal(t, []string{"/bin/bash", "-c", "flush.sh"}, updated.Spec.Lifecycle.PrePause.Exec.Command)
	assert.Equal(t, []string{"/bin/bash", "-c", "restore.sh"}, updated.Spec.Lifecycle.PostResume.Exec.Command)
	assert.Equal(t, sbx.Spec.Lifecycle.PreUpgrade, updated.Spec.Lifecycle.PreUpgrade)
	assert.Equal(t, sbx.Spec.Lifecycle.PostUpgrade, updated.Spec.Lifecycle.PostUpgrade)
	assert.Nil(t, updated.Spec.UpgradePolicy, "hook-only ops must not set an UpgradePolicy")
	assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
	assert.Equal(t, "ops-1", updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
}

func TestApplySandboxPatch_PausedSandbox_OperationModes(t *testing.T) {
	action := &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}}
	tests := []struct {
		name       string
		lifecycle  *agentsv1alpha1.SandboxLifecycle
		wantResume bool
	}{
		{name: "legacy nil lifecycle", wantResume: true},
		{name: "legacy empty lifecycle", lifecycle: &agentsv1alpha1.SandboxLifecycle{}, wantResume: true},
		{name: "legacy preUpgrade", lifecycle: &agentsv1alpha1.SandboxLifecycle{PreUpgrade: action}, wantResume: true},
		{name: "legacy postUpgrade", lifecycle: &agentsv1alpha1.SandboxLifecycle{PostUpgrade: action}, wantResume: true},
		{name: "mixed hooks", lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: action, PreUpgrade: action}, wantResume: true},
		{name: "prePause only", lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: action}},
		{name: "postResume only", lifecycle: &agentsv1alpha1.SandboxLifecycle{PostResume: action}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, strategy := range []agentsv1alpha1.SandboxUpdateOpsStrategyType{
				agentsv1alpha1.SandboxUpdateOpsStrategyRecreate,
				agentsv1alpha1.SandboxUpdateOpsStrategyCheckpointRestore,
			} {
				t.Run(string(strategy), func(t *testing.T) {
					ops := &agentsv1alpha1.SandboxUpdateOps{
						ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
						Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
							Lifecycle:      tt.lifecycle.DeepCopy(),
							UpdateStrategy: agentsv1alpha1.SandboxUpdateOpsStrategy{Type: strategy},
						},
					}
					sbx := newSandbox("sbx-1", "default", "", agentsv1alpha1.SandboxPaused, []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue},
					})
					sbx.Spec.Paused = true

					r := newTestReconciler(sbx)
					require.NoError(t, r.applySandboxPatch(t.Context(), sbx, ops))

					updated := &agentsv1alpha1.Sandbox{}
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
					if tt.lifecycle != nil && *tt.lifecycle == (agentsv1alpha1.SandboxLifecycle{}) {
						assert.Nil(t, updated.Spec.Lifecycle)
					} else {
						assert.Equal(t, tt.lifecycle, updated.Spec.Lifecycle)
					}
					assert.Equal(t, sbx.Spec.Template, updated.Spec.Template)
					assert.True(t, updated.Spec.Paused)
					if tt.wantResume {
						assert.Equal(t, agentsv1alpha1.True, updated.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
						require.NotNil(t, updated.Spec.UpgradePolicy)
						assert.Equal(t, agentsv1alpha1.SandboxUpgradePolicyType(strategy), updated.Spec.UpgradePolicy.Type)
					} else {
						assert.Empty(t, updated.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
						assert.Nil(t, updated.Spec.UpgradePolicy)
					}
				})
			}
		})
	}
}

func TestApplySandboxPatch_MixedOps_AppliesHooksWithUpgrade(t *testing.T) {
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:2"}}},
			}),
			Lifecycle: &agentsv1alpha1.SandboxLifecycle{
				PrePause: &agentsv1alpha1.LifecycleAction{
					Exec: &corev1.ExecAction{Command: []string{"flush"}},
				},
				PreUpgrade: &agentsv1alpha1.LifecycleAction{
					Exec: &corev1.ExecAction{Command: []string{"backup"}},
				},
			},
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSpec{
			Lifecycle: &agentsv1alpha1.SandboxLifecycle{
				PostResume: &agentsv1alpha1.LifecycleAction{
					Exec: &corev1.ExecAction{Command: []string{"keep-me"}},
				},
			},
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:1"}}},
				},
			},
		},
	}

	r := newTestReconciler(sbx)
	require.NoError(t, r.applySandboxPatch(context.Background(), sbx, ops))

	updated := &agentsv1alpha1.Sandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated))
	require.NotNil(t, updated.Spec.UpgradePolicy, "template patch must set the UpgradePolicy")
	require.NotNil(t, updated.Spec.Lifecycle)
	assert.Equal(t, []string{"flush"}, updated.Spec.Lifecycle.PrePause.Exec.Command)
	assert.Equal(t, []string{"backup"}, updated.Spec.Lifecycle.PreUpgrade.Exec.Command)
	assert.Equal(t, []string{"keep-me"}, updated.Spec.Lifecycle.PostResume.Exec.Command,
		"hooks the ops does not specify must be preserved")
	assert.Equal(t, "busybox:2", updated.Spec.Template.Spec.Containers[0].Image)
}

func TestIsSandboxOpsApplied_Lifecycle(t *testing.T) {
	prePause := &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}}
	postResume := &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}}
	sbx := &agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{
		Lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: prePause},
	}}

	assert.False(t, isSandboxOpsApplied(sbx, &agentsv1alpha1.SandboxUpdateOps{}),
		"legacy empty ops requires upgrade-state tracking")

	assert.True(t, isSandboxOpsApplied(sbx, &agentsv1alpha1.SandboxUpdateOps{
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{Lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: prePause}},
	}), "declared prePause already on the sandbox")

	assert.False(t, isSandboxOpsApplied(sbx, &agentsv1alpha1.SandboxUpdateOps{
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{Lifecycle: &agentsv1alpha1.SandboxLifecycle{
			PrePause: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"other"}}},
		}},
	}), "declared prePause differs from the sandbox")

	assert.False(t, isSandboxOpsApplied(sbx, &agentsv1alpha1.SandboxUpdateOps{
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{Lifecycle: &agentsv1alpha1.SandboxLifecycle{PostResume: postResume}},
	}), "declared postResume missing on the sandbox")

	upgradeHook := &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"x"}}}
	assert.False(t, isSandboxOpsApplied(&agentsv1alpha1.Sandbox{}, &agentsv1alpha1.SandboxUpdateOps{
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{Lifecycle: &agentsv1alpha1.SandboxLifecycle{
			PreUpgrade: upgradeHook,
		}},
	}), "declared preUpgrade missing on the sandbox")
	assert.False(t, isSandboxOpsApplied(&agentsv1alpha1.Sandbox{Spec: agentsv1alpha1.SandboxSpec{
		Lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: prePause, PreUpgrade: upgradeHook},
	}}, &agentsv1alpha1.SandboxUpdateOps{
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{Lifecycle: &agentsv1alpha1.SandboxLifecycle{
			PrePause: prePause, PreUpgrade: upgradeHook,
		}},
	}), "empty-patch upgrade ops requires upgrade-state tracking even when hooks match")
}

func TestApplySandboxPatch_MixedOps_PausedSandbox_Phase1CarriesHooks(t *testing.T) {
	// A paused sandbox with a mixed ops (template patch + prePause): phase 1
	// must carry the hooks in the same patch as the resume trigger — they are
	// spec-level fields and do not affect the pod-template hash, so the
	// sandbox still resumes with the OLD template.
	ops := &agentsv1alpha1.SandboxUpdateOps{
		ObjectMeta: metav1.ObjectMeta{Name: "ops-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxUpdateOpsSpec{
			Patch: mustMarshalPatch(corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:2"}}},
			}),
			Lifecycle: &agentsv1alpha1.SandboxLifecycle{
				PrePause: &agentsv1alpha1.LifecycleAction{
					Exec: &corev1.ExecAction{Command: []string{"flush"}},
				},
			},
		},
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-1", Namespace: "default"},
		Spec: agentsv1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:1"}}},
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPaused},
	}

	r := newTestReconciler(sbx)
	require.NoError(t, r.applySandboxPatch(context.Background(), sbx, ops))

	updated := &agentsv1alpha1.Sandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, updated))
	// Phase 1: resume trigger + hooks + policy in one patch, template untouched.
	assert.Equal(t, agentsv1alpha1.True, updated.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
	require.NotNil(t, updated.Spec.Lifecycle)
	require.NotNil(t, updated.Spec.Lifecycle.PrePause)
	assert.Equal(t, []string{"flush"}, updated.Spec.Lifecycle.PrePause.Exec.Command)
	assert.NotNil(t, updated.Spec.UpgradePolicy)
	assert.Equal(t, "busybox:1", updated.Spec.Template.Spec.Containers[0].Image,
		"phase 1 must not patch the template")

	// Phase 2 after resume: the template patch lands and the trigger clears.
	updated.Status.Phase = agentsv1alpha1.SandboxRunning
	require.NoError(t, r.Status().Update(context.Background(), updated))
	require.NoError(t, r.applyTemplatePatch(context.Background(), updated, ops))

	final := &agentsv1alpha1.Sandbox{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "sbx-1", Namespace: "default"}, final))
	assert.Equal(t, "busybox:2", final.Spec.Template.Spec.Containers[0].Image)
	assert.Empty(t, final.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
	require.NotNil(t, final.Spec.Lifecycle.PrePause, "hooks survive phase 2")
}

func TestReconcile_MatchingTemplateHooks_UpdatesWithoutUpgrade(t *testing.T) {
	action := &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"updated"}}}
	hooks := []struct {
		name      string
		lifecycle *agentsv1alpha1.SandboxLifecycle
	}{
		{name: "prePause", lifecycle: &agentsv1alpha1.SandboxLifecycle{PrePause: action}},
		{name: "postResume", lifecycle: &agentsv1alpha1.SandboxLifecycle{PostResume: action}},
		{name: "preUpgrade", lifecycle: &agentsv1alpha1.SandboxLifecycle{PreUpgrade: action}},
		{name: "postUpgrade", lifecycle: &agentsv1alpha1.SandboxLifecycle{PostUpgrade: action}},
	}
	states := []struct {
		name        string
		phase       agentsv1alpha1.SandboxPhase
		succeeded   bool
		withTrigger bool
	}{
		{name: "paused", phase: agentsv1alpha1.SandboxPaused},
		{name: "paused after upgrade", phase: agentsv1alpha1.SandboxPaused, succeeded: true, withTrigger: true},
		{name: "running after upgrade", phase: agentsv1alpha1.SandboxRunning, succeeded: true},
	}
	for _, hook := range hooks {
		for _, state := range states {
			for _, policyType := range []agentsv1alpha1.SandboxUpgradePolicyType{"", agentsv1alpha1.SandboxUpgradePolicyRecreate, agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore} {
				t.Run(fmt.Sprintf("%s/%s/%s", hook.name, state.name, policyType), func(t *testing.T) {
					ops := newSandboxUpdateOps("ops-1", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, nil)
					ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}}}})
					ops.Spec.Lifecycle = hook.lifecycle.DeepCopy()
					ops.Spec.StateFilter = &agentsv1alpha1.UpgradeStateFilter{States: []agentsv1alpha1.SandboxPhase{agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxPaused}}
					previousOps := newSandboxUpdateOps("previous-ops", "default", agentsv1alpha1.SandboxUpdateOpsCompleted, false, nil)
					sbx := newSandbox("sbx-1", "default", previousOps.Name, state.phase, nil)
					sbx.Generation = 1
					sbx.Status.ObservedGeneration = 1
					sbx.Spec.Paused = state.phase == agentsv1alpha1.SandboxPaused
					if sbx.Spec.Paused {
						sbx.Status.Conditions = []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue}}
					}
					if state.succeeded {
						sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonSucceeded, Status: metav1.ConditionTrue})
					}
					if policyType != "" {
						sbx.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{Type: policyType}
					}
					if state.withTrigger {
						sbx.Annotations = map[string]string{agentsv1alpha1.AnnotationUpgradeResumeTrigger: agentsv1alpha1.True}
					}
					sbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed] = agentsv1alpha1.True
					sbx.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{
						PrePause:   &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"old-flush"}}},
						PostResume: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"old-restore"}}},
					}

					r := newTestReconciler(ops, previousOps, sbx)
					req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ops)}
					result, err := r.Reconcile(t.Context(), req)
					require.NoError(t, err)
					assert.Equal(t, time.Second, result.RequeueAfter)
					updated := &agentsv1alpha1.Sandbox{}
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
					assert.Equal(t, sbx.Labels, updated.Labels)
					assert.Equal(t, sbx.Annotations, updated.Annotations)
					assert.Equal(t, sbx.Spec.Template, updated.Spec.Template)
					assert.Equal(t, sbx.Spec.Paused, updated.Spec.Paused)
					assert.Equal(t, sbx.Spec.UpgradePolicy, updated.Spec.UpgradePolicy)
					assert.Equal(t, sbx.Status, updated.Status)
					assert.Equal(t, sbx.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger], updated.Annotations[agentsv1alpha1.AnnotationUpgradeResumeTrigger])
					assert.Equal(t, agentsv1alpha1.True, updated.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])
					require.NotNil(t, updated.Spec.Lifecycle)
					expected := sbx.Spec.Lifecycle.DeepCopy()
					if hook.lifecycle.PrePause != nil {
						expected.PrePause = action.DeepCopy()
					}
					if hook.lifecycle.PostResume != nil {
						expected.PostResume = action.DeepCopy()
					}
					expected.PreUpgrade = hook.lifecycle.PreUpgrade.DeepCopy()
					expected.PostUpgrade = hook.lifecycle.PostUpgrade.DeepCopy()
					assert.Equal(t, expected, updated.Spec.Lifecycle)

					updated.Generation++
					require.NoError(t, r.Update(t.Context(), updated))
					result, err = r.Reconcile(t.Context(), req)
					require.NoError(t, err)
					assert.Zero(t, result.RequeueAfter)
					updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
					require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedOps))
					assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
					assert.Zero(t, updatedOps.Status.Replicas)
					assert.Zero(t, updatedOps.Status.UpdatedReplicas)
					assert.Zero(t, updatedOps.Status.UpdatingReplicas)
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
					assert.Less(t, updated.Status.ObservedGeneration, updated.Generation)
					assert.Equal(t, sbx.Spec.UpgradePolicy, updated.Spec.UpgradePolicy)
					assert.Equal(t, sbx.Status, updated.Status)

					require.NoError(t, r.Delete(t.Context(), updatedOps))
					_, err = r.Reconcile(t.Context(), req)
					require.NoError(t, err)
					require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
					assert.Equal(t, sbx.Labels, updated.Labels)
					assert.Equal(t, sbx.Annotations, updated.Annotations)
					assert.Equal(t, sbx.Spec.UpgradePolicy, updated.Spec.UpgradePolicy)
					assert.Equal(t, expected, updated.Spec.Lifecycle)
				})
			}
		}
	}
}

func TestApplySandboxPatch_UpgradeOwnership(t *testing.T) {
	tests := []struct {
		name        string
		opsLabel    string
		phase       agentsv1alpha1.SandboxPhase
		image       string
		wantUpgrade bool
	}{
		{name: "unclaimed configuration", phase: agentsv1alpha1.SandboxRunning, image: "busybox:1.0"},
		{name: "owned upgrade recovery", opsLabel: "ops-1", phase: agentsv1alpha1.SandboxUpgrading, image: "busybox:1.0", wantUpgrade: true},
		{name: "previous operation upgrade recovery", opsLabel: "previous-ops", phase: agentsv1alpha1.SandboxUpgrading, image: "busybox:1.0", wantUpgrade: true},
		{name: "deleted operation upgrade recovery", phase: agentsv1alpha1.SandboxUpgrading, image: "busybox:1.0", wantUpgrade: true},
		{name: "template drift requires upgrade", opsLabel: "previous-ops", phase: agentsv1alpha1.SandboxUpgrading, image: "busybox:0.0", wantUpgrade: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newSandboxUpdateOps("ops-1", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
			ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}}}})
			ops.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{PrePause: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}}}
			sbx := newSandbox("sbx-1", "default", tt.opsLabel, tt.phase, nil)
			if tt.phase == agentsv1alpha1.SandboxUpgrading {
				sbx.Status.Conditions = []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, Status: metav1.ConditionFalse}}
			}
			sbx.Spec.Template.Spec.Containers[0].Image = tt.image
			sbx.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyCheckpointRestore}
			sbx.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed] = agentsv1alpha1.True

			r := newTestReconciler(sbx)
			require.NoError(t, r.applySandboxPatch(t.Context(), sbx, ops))
			updated := &agentsv1alpha1.Sandbox{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
			assert.Equal(t, "busybox:1.0", updated.Spec.Template.Spec.Containers[0].Image)
			assert.Equal(t, ops.Spec.Lifecycle, updated.Spec.Lifecycle)
			assert.Equal(t, sbx.Annotations, updated.Annotations)
			if tt.wantUpgrade {
				require.NotNil(t, updated.Spec.UpgradePolicy)
				assert.Equal(t, agentsv1alpha1.SandboxUpgradePolicyRecreate, updated.Spec.UpgradePolicy.Type)
				assert.Equal(t, ops.Name, updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
				assert.Empty(t, updated.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])
				assert.Equal(t, sandboxFailed, r.classifySandbox(t.Context(), updated, ops))
			} else {
				assert.Equal(t, sbx.Spec.UpgradePolicy, updated.Spec.UpgradePolicy)
				assert.Equal(t, sbx.Labels, updated.Labels)
				assert.Equal(t, sandboxNoNeedUpdate, r.classifySandbox(t.Context(), updated, ops))
			}
		})
	}
}

func TestReconcile_MatchingTemplateBatch_RequeuesUntilApplied(t *testing.T) {
	tests := []struct {
		name          string
		maxConcurrent string
		withUpgrade   bool
	}{
		{name: "single configuration per round", maxConcurrent: "1"},
		{name: "percentage budget", maxConcurrent: "50%"},
		{name: "mixed owned upgrade", maxConcurrent: "100%", withUpgrade: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newSandboxUpdateOps("ops-1", "default", agentsv1alpha1.SandboxUpdateOpsUpdating, false, nil)
			limit := intstrutil.Parse(tt.maxConcurrent)
			ops.Spec.UpdateStrategy.MaxUnavailable = &limit
			ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:1.0"}}}})
			ops.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{PrePause: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}}}
			sandboxes := []*agentsv1alpha1.Sandbox{
				newSandbox("a", "default", "", agentsv1alpha1.SandboxRunning, nil),
				newSandbox("b", "default", "", agentsv1alpha1.SandboxRunning, nil),
				newSandbox("c", "default", "", agentsv1alpha1.SandboxRunning, nil),
			}
			objects := []runtime.Object{ops, sandboxes[0], sandboxes[1], sandboxes[2]}
			owned := newSandbox("owned", "default", ops.Name, agentsv1alpha1.SandboxUpgrading, []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod, Status: metav1.ConditionFalse}})
			owned.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}
			if tt.withUpgrade {
				objects = append(objects, owned)
			}
			r := newTestReconciler(objects...)
			req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ops)}
			result, err := r.Reconcile(t.Context(), req)
			require.NoError(t, err)
			assert.Equal(t, time.Second, result.RequeueAfter)
			for i := 0; i < 3 && result.RequeueAfter > 0; i++ {
				result, err = r.Reconcile(t.Context(), req)
				require.NoError(t, err)
			}
			assert.Zero(t, result.RequeueAfter)
			for _, sbx := range sandboxes {
				updated := &agentsv1alpha1.Sandbox{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
				assert.Equal(t, ops.Spec.Lifecycle, updated.Spec.Lifecycle)
				assert.Empty(t, updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
				assert.Nil(t, updated.Spec.UpgradePolicy)
			}
			updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
			require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedOps))
			if tt.withUpgrade {
				assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsUpdating, updatedOps.Status.Phase)
				assert.Equal(t, int32(1), updatedOps.Status.UpdatingReplicas)
				owned.Status.Conditions[0].Reason = agentsv1alpha1.SandboxUpgradingReasonSucceeded
				owned.Status.Conditions[0].Status = metav1.ConditionTrue
				owned.Status.Phase = agentsv1alpha1.SandboxRunning
				require.NoError(t, r.Status().Update(t.Context(), owned))
				_, err = r.Reconcile(t.Context(), req)
				require.NoError(t, err)
				require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedOps))
				assert.Equal(t, int32(1), updatedOps.Status.Replicas)
				assert.Equal(t, int32(1), updatedOps.Status.UpdatedReplicas)
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(owned), owned))
				assert.Nil(t, owned.Spec.UpgradePolicy)
			} else {
				assert.Zero(t, updatedOps.Status.Replicas)
				assert.Zero(t, updatedOps.Status.UpdatedReplicas)
			}
			assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
		})
	}
}

func TestReconcile_RecoveryOpsCountsPreviouslyFailedSandbox(t *testing.T) {
	limit := intstrutil.FromInt32(1)
	ops := newSandboxUpdateOps("recovery-ops", "default", agentsv1alpha1.SandboxUpdateOpsPending, false, &limit)
	ops.Spec.Patch = mustMarshalPatch(corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "busybox:2.0"}}}})
	ops.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{PreUpgrade: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "exit 0"}}}}
	failed := newSandbox("a-failed", "default", "", agentsv1alpha1.SandboxUpgrading, []metav1.Condition{
		{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonPreUpgradeFailed, Status: metav1.ConditionFalse},
	})
	failed.Spec.Template.Spec.Containers[0].Image = "busybox:2.0"
	failed.Spec.UpgradePolicy = &agentsv1alpha1.SandboxUpgradePolicy{Type: agentsv1alpha1.SandboxUpgradePolicyRecreate}
	failed.Spec.Lifecycle = &agentsv1alpha1.SandboxLifecycle{PreUpgrade: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"/bin/sh", "-c", "exit 1"}}}}
	failed.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed] = agentsv1alpha1.True
	pending := newSandbox("b-pending", "default", "", agentsv1alpha1.SandboxRunning, nil)
	pending.Spec.UpgradePolicy = failed.Spec.UpgradePolicy.DeepCopy()

	r := newTestReconciler(ops, failed, pending)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(ops)}
	result, err := r.Reconcile(t.Context(), req)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)
	updatedPending := &agentsv1alpha1.Sandbox{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(pending), updatedPending))
	assert.Empty(t, updatedPending.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
	assert.Equal(t, "busybox:1.0", updatedPending.Spec.Template.Spec.Containers[0].Image)

	for i, sbx := range []*agentsv1alpha1.Sandbox{failed, pending} {
		updated := &agentsv1alpha1.Sandbox{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
		assert.Equal(t, ops.Name, updated.Labels[agentsv1alpha1.LabelSandboxUpdateOps])
		assert.Equal(t, "busybox:2.0", updated.Spec.Template.Spec.Containers[0].Image)
		assert.Equal(t, ops.Spec.Lifecycle, updated.Spec.Lifecycle)
		require.NotNil(t, updated.Spec.UpgradePolicy)
		updated.Status.Phase = agentsv1alpha1.SandboxUpgrading
		updated.Status.ObservedGeneration = updated.Generation
		updated.Status.Conditions = []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionUpgrading), Reason: agentsv1alpha1.SandboxUpgradingReasonUpgradePod, Status: metav1.ConditionFalse}}
		require.NoError(t, r.Status().Update(t.Context(), updated))
		_, err = r.Reconcile(t.Context(), req)
		require.NoError(t, err)
		updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
		require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedOps))
		assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsUpdating, updatedOps.Status.Phase)
		assert.Equal(t, int32(2), updatedOps.Status.Replicas)
		assert.Equal(t, int32(1), updatedOps.Status.UpdatingReplicas)
		assert.EqualValues(t, i, updatedOps.Status.UpdatedReplicas)

		updated.Status.Phase = agentsv1alpha1.SandboxRunning
		updated.Status.Conditions[0].Reason = agentsv1alpha1.SandboxUpgradingReasonSucceeded
		updated.Status.Conditions[0].Status = metav1.ConditionTrue
		require.NoError(t, r.Status().Update(t.Context(), updated))
		_, err = r.Reconcile(t.Context(), req)
		require.NoError(t, err)
	}

	updatedOps := &agentsv1alpha1.SandboxUpdateOps{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, updatedOps))
	assert.Equal(t, agentsv1alpha1.SandboxUpdateOpsCompleted, updatedOps.Status.Phase)
	assert.Equal(t, int32(2), updatedOps.Status.UpdatedReplicas)
	assert.Zero(t, updatedOps.Status.FailedReplicas)
	assert.Zero(t, updatedOps.Status.UpdatingReplicas)
	for _, sbx := range []*agentsv1alpha1.Sandbox{failed, pending} {
		updated := &agentsv1alpha1.Sandbox{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sbx), updated))
		assert.Nil(t, updated.Spec.UpgradePolicy)
		assert.Empty(t, updated.Labels[agentsv1alpha1.LabelSandboxUpgradeFailed])
	}
}
