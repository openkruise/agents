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
