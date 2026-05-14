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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openkruise/agents/api/v1alpha1"
)

// newMaterializeReconciler wires a minimal Reconciler backed by a fake client
// with SandboxTemplate registered for ensure / cleanup tests.
func newMaterializeReconciler(objs ...client.Object) *Reconciler {
	c := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objs...).
		WithLists(&v1alpha1.SandboxTemplateList{}, &v1alpha1.SandboxList{}).
		Build()
	return &Reconciler{
		Client: c,
		Scheme: testScheme,
		Codec:  codec,
	}
}

func newInlineSandboxSet(name, image string) *v1alpha1.SandboxSet {
	return &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: v1alpha1.SandboxSetSpec{
			Replicas: 1,
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "main", Image: image}},
					},
				},
			},
		},
	}
}

func TestReconciler_ensureSandboxTemplate(t *testing.T) {
	tests := []struct {
		name        string
		build       func() (*Reconciler, *v1alpha1.SandboxSet)
		expectName  func(t *testing.T, r *Reconciler, sbs *v1alpha1.SandboxSet, name string)
		expectError string
	}{
		{
			name: "inline template first reconcile creates SBT with owner ref",
			build: func() (*Reconciler, *v1alpha1.SandboxSet) {
				sbs := newInlineSandboxSet("sbs-a", "img:v1")
				return newMaterializeReconciler(sbs), sbs
			},
			expectName: func(t *testing.T, r *Reconciler, sbs *v1alpha1.SandboxSet, name string) {
				require.True(t, name != "" && name != sbs.Name)
				sbt := &v1alpha1.SandboxTemplate{}
				require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: sbs.Namespace, Name: name}, sbt))
				ref := metav1.GetControllerOf(sbt)
				require.NotNil(t, ref)
				assert.Equal(t, sbs.UID, ref.UID)
				require.NotNil(t, ref.BlockOwnerDeletion)
				assert.False(t, *ref.BlockOwnerDeletion)
				assert.Equal(t, sbs.Spec.Template.Spec.Containers[0].Image, sbt.Spec.Template.Spec.Containers[0].Image)
			},
		},
		{
			name: "inline template second reconcile is idempotent",
			build: func() (*Reconciler, *v1alpha1.SandboxSet) {
				sbs := newInlineSandboxSet("sbs-b", "img:v1")
				r := newMaterializeReconciler(sbs)
				// Pre-create once so the second call exercises the
				// AlreadyExists branch.
				_, err := r.ensureSandboxTemplate(context.Background(), sbs)
				require.NoError(t, err)
				return r, sbs
			},
			expectName: func(t *testing.T, r *Reconciler, sbs *v1alpha1.SandboxSet, name string) {
				sbtList := &v1alpha1.SandboxTemplateList{}
				require.NoError(t, r.List(context.Background(), sbtList, client.InNamespace(sbs.Namespace)))
				var owned int
				for i := range sbtList.Items {
					if isControlledBy(&sbtList.Items[i], sbs) {
						owned++
					}
				}
				assert.Equal(t, 1, owned)
			},
		},
		{
			name: "templateRef returns ref name without creating SBT",
			build: func() (*Reconciler, *v1alpha1.SandboxSet) {
				userSBT := &v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "user-sbt", Namespace: "default"},
					Spec: v1alpha1.SandboxTemplateSpec{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
						},
					},
				}
				sbs := &v1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "sbs-ref", Namespace: "default", UID: types.UID("uid-ref")},
					Spec: v1alpha1.SandboxSetSpec{
						Replicas: 1,
						EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
							TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "user-sbt"},
						},
					},
				}
				return newMaterializeReconciler(sbs, userSBT), sbs
			},
			expectName: func(t *testing.T, r *Reconciler, sbs *v1alpha1.SandboxSet, name string) {
				assert.Equal(t, "user-sbt", name)
				sbtList := &v1alpha1.SandboxTemplateList{}
				require.NoError(t, r.List(context.Background(), sbtList, client.InNamespace(sbs.Namespace)))
				for i := range sbtList.Items {
					// No auto-materialised SBT (they would be named with sbs prefix).
					assert.NotContains(t, sbtList.Items[i].Name, sbs.Name+"-")
				}
			},
		},
		{
			name: "compute hash is stable under webhook defaulting",
			build: func() (*Reconciler, *v1alpha1.SandboxSet) {
				sbs := newInlineSandboxSet("sbs-hash", "img:v1")
				// Force a field that the defaulter would normalise so we can
				// verify the hash does not drift between successive calls.
				sbs.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(true)
				r := newMaterializeReconciler(sbs)
				return r, sbs
			},
			expectName: func(t *testing.T, r *Reconciler, sbs *v1alpha1.SandboxSet, name string) {
				h1, err := r.computeTemplateHash(context.Background(), sbs)
				require.NoError(t, err)
				h2, err := r.computeTemplateHash(context.Background(), sbs)
				require.NoError(t, err)
				assert.Equal(t, h1, h2)
				assert.Equal(t, fmt.Sprintf("%s-%s", sbs.Name, h1), name)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, sbs := tt.build()
			name, err := r.ensureSandboxTemplate(context.Background(), sbs)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			tt.expectName(t, r, sbs, name)
		})
	}
}

func TestReconciler_cleanupOldSandboxTemplates(t *testing.T) {
	sbs := newInlineSandboxSet("sbs-clean", "img:v1")
	sbs.Spec.Template = nil // template is irrelevant for cleanup logic
	base := time.Now()

	buildOwnedSBT := func(idx int, createdAtOffset time.Duration) *v1alpha1.SandboxTemplate {
		sbt := &v1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:              fmt.Sprintf("%s-h%d", sbs.Name, idx),
				Namespace:         sbs.Namespace,
				CreationTimestamp: metav1.NewTime(base.Add(createdAtOffset)),
			},
		}
		require.NoError(t, ctrl.SetControllerReference(sbs, sbt, testScheme))
		return sbt
	}
	sandboxWithHash := func(hash string) *v1alpha1.Sandbox {
		sbx := &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sbx-" + hash,
				Namespace: sbs.Namespace,
				Labels:    map[string]string{v1alpha1.LabelTemplateHash: hash},
			},
		}
		sbx.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: v1alpha1.GroupVersion.String(),
			Kind:       "SandboxSet",
			Name:       sbs.Name,
			UID:        sbs.UID,
			Controller: ptr.To(true),
		}}
		return sbx
	}

	tests := []struct {
		name          string
		objs          func() []client.Object
		wantRemaining func(t *testing.T, names []string)
	}{
		{
			name: "under the limit nothing is deleted",
			objs: func() []client.Object {
				var objs []client.Object
				for i := 0; i < 5; i++ {
					objs = append(objs, buildOwnedSBT(i, time.Duration(i)*time.Second))
				}
				objs = append(objs, sbs)
				return objs
			},
			wantRemaining: func(t *testing.T, names []string) {
				assert.Len(t, names, 5)
			},
		},
		{
			name: "orphans beyond limit are pruned oldest first",
			objs: func() []client.Object {
				var objs []client.Object
				// Create 12 orphan SBTs; only 10 should remain (youngest).
				for i := 0; i < 12; i++ {
					objs = append(objs, buildOwnedSBT(i, time.Duration(i)*time.Second))
				}
				objs = append(objs, sbs)
				return objs
			},
			wantRemaining: func(t *testing.T, names []string) {
				assert.Len(t, names, 10)
				// The two oldest (h0, h1) must be gone.
				for _, n := range names {
					assert.NotEqual(t, fmt.Sprintf("%s-h0", sbs.Name), n)
					assert.NotEqual(t, fmt.Sprintf("%s-h1", sbs.Name), n)
				}
			},
		},
		{
			name: "in-use SBTs are always preserved",
			objs: func() []client.Object {
				var objs []client.Object
				for i := 0; i < 12; i++ {
					objs = append(objs, buildOwnedSBT(i, time.Duration(i)*time.Second))
				}
				// Sandbox pins the oldest hash (h0); it must survive cleanup.
				objs = append(objs, sandboxWithHash("h0"))
				objs = append(objs, sbs)
				return objs
			},
			wantRemaining: func(t *testing.T, names []string) {
				assert.Contains(t, names, fmt.Sprintf("%s-h0", sbs.Name))
				// Budget = limit(10) - inUse(1) = 9 orphans retained out of 11.
				assert.Len(t, names, 10)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newMaterializeReconciler(tt.objs()...)
			r.cleanupOldSandboxTemplates(context.Background(), sbs)
			sbtList := &v1alpha1.SandboxTemplateList{}
			require.NoError(t, r.List(context.Background(), sbtList, client.InNamespace(sbs.Namespace)))
			names := make([]string, 0, len(sbtList.Items))
			for i := range sbtList.Items {
				names = append(names, sbtList.Items[i].Name)
			}
			tt.wantRemaining(t, names)
		})
	}
}
