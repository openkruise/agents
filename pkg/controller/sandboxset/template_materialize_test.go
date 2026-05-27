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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/fieldindex"
)

// newMaterializeReconciler wires a minimal Reconciler backed by a fake client
// with SandboxTemplate registered for ensure / cleanup tests.
func newMaterializeReconciler(objs ...client.Object) *Reconciler {
	c := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objs...).
		WithIndex(&v1alpha1.SandboxTemplate{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		Build()
	return &Reconciler{
		Client: c,
		Scheme: testScheme,
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
				spec, err := r.buildSandboxTemplateSpec(context.Background(), sbs)
				require.NoError(t, err)
				hash, err := computeRevisionHash(spec)
				require.NoError(t, err)
				_, err = r.ensureSandboxTemplate(context.Background(), sbs, spec, hash)
				require.NoError(t, err)
				return r, sbs
			},
			expectName: func(t *testing.T, r *Reconciler, sbs *v1alpha1.SandboxSet, name string) {
				sbtList := &v1alpha1.SandboxTemplateList{}
				require.NoError(t, r.List(context.Background(), sbtList, client.InNamespace(sbs.Namespace)))
				var owned int
				for i := range sbtList.Items {
					ref := metav1.GetControllerOf(&sbtList.Items[i])
					if ref != nil && ref.UID == sbs.UID {
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
			name: "compute hash is stable across successive calls",
			build: func() (*Reconciler, *v1alpha1.SandboxSet) {
				sbs := newInlineSandboxSet("sbs-hash", "img:v1")
				// Force a field that the defaulter would normalise so we can
				// verify the hash does not drift between successive calls.
				sbs.Spec.Template.Spec.AutomountServiceAccountToken = ptr.To(true)
				r := newMaterializeReconciler(sbs)
				return r, sbs
			},
			expectName: func(t *testing.T, r *Reconciler, sbs *v1alpha1.SandboxSet, name string) {
				spec, err := r.buildSandboxTemplateSpec(context.Background(), sbs)
				require.NoError(t, err)
				h1, err := computeRevisionHash(spec)
				require.NoError(t, err)
				spec2, err := r.buildSandboxTemplateSpec(context.Background(), sbs)
				require.NoError(t, err)
				h2, err := computeRevisionHash(spec2)
				require.NoError(t, err)
				assert.Equal(t, h1, h2)
				assert.Equal(t, fmt.Sprintf("%s-%s", sbs.Name, h1), name)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, sbs := tt.build()
			spec, err := r.buildSandboxTemplateSpec(context.Background(), sbs)
			if tt.expectError != "" {
				if err != nil {
					require.Error(t, err)
					assert.Contains(t, err.Error(), tt.expectError)
					return
				}
			}
			require.NoError(t, err)
			hash, err := computeRevisionHash(spec)
			require.NoError(t, err)
			name, err := r.ensureSandboxTemplate(context.Background(), sbs, spec, hash)
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

func TestReconciler_ensureTemplateRevision(t *testing.T) {
	tests := []struct {
		name        string
		build       func() (*Reconciler, *v1alpha1.SandboxSet)
		expectError string
		verify      func(t *testing.T, hash, name string)
	}{
		{
			name: "inline template returns hash and materialized template name",
			build: func() (*Reconciler, *v1alpha1.SandboxSet) {
				sbs := newInlineSandboxSet("sbs-rev", "img:v1")
				return newMaterializeReconciler(sbs), sbs
			},
			verify: func(t *testing.T, hash, name string) {
				assert.NotEmpty(t, hash)
				assert.Contains(t, name, "sbs-rev-")
				assert.Contains(t, name, hash)
			},
		},
		{
			name: "templateRef returns hash and referenced template name",
			build: func() (*Reconciler, *v1alpha1.SandboxSet) {
				userSBT := &v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: "user-tpl", Namespace: "default"},
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
							TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "user-tpl"},
						},
					},
				}
				return newMaterializeReconciler(sbs, userSBT), sbs
			},
			verify: func(t *testing.T, hash, name string) {
				assert.NotEmpty(t, hash)
				assert.Equal(t, "user-tpl", name)
			},
		},
		{
			name: "templateRef not found returns error",
			build: func() (*Reconciler, *v1alpha1.SandboxSet) {
				sbs := &v1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "sbs-err", Namespace: "default", UID: types.UID("uid-err")},
					Spec: v1alpha1.SandboxSetSpec{
						Replicas: 1,
						EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
							TemplateRef: &v1alpha1.SandboxTemplateRef{Name: "missing"},
						},
					},
				}
				return newMaterializeReconciler(sbs), sbs
			},
			expectError: "failed to resolve sandbox template",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, sbs := tt.build()
			hash, name, err := r.ensureTemplateRevision(context.Background(), sbs)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			tt.verify(t, hash, name)
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

	tests := []struct {
		name          string
		objs          func() []client.Object
		wantAnnotated func(t *testing.T, items []v1alpha1.SandboxTemplate)
	}{
		{
			name: "under the limit nothing is annotated",
			objs: func() []client.Object {
				var objs []client.Object
				for i := 0; i < 5; i++ {
					objs = append(objs, buildOwnedSBT(i, time.Duration(i)*time.Second))
				}
				objs = append(objs, sbs)
				return objs
			},
			wantAnnotated: func(t *testing.T, items []v1alpha1.SandboxTemplate) {
				for i := range items {
					assert.Empty(t, items[i].Annotations[v1alpha1.AnnotationCleanupCandidate])
				}
			},
		},
		{
			name: "beyond limit oldest are annotated as cleanup candidates",
			objs: func() []client.Object {
				var objs []client.Object
				// Create 12 SBTs; the 2 oldest should be annotated.
				for i := 0; i < 12; i++ {
					objs = append(objs, buildOwnedSBT(i, time.Duration(i)*time.Second))
				}
				objs = append(objs, sbs)
				return objs
			},
			wantAnnotated: func(t *testing.T, items []v1alpha1.SandboxTemplate) {
				assert.Len(t, items, 12) // all still exist
				annotated := map[string]bool{}
				for i := range items {
					if items[i].Annotations[v1alpha1.AnnotationCleanupCandidate] == "true" {
						annotated[items[i].Name] = true
					}
				}
				// The two oldest (h0, h1) should be annotated.
				assert.True(t, annotated[fmt.Sprintf("%s-h0", sbs.Name)])
				assert.True(t, annotated[fmt.Sprintf("%s-h1", sbs.Name)])
				assert.Len(t, annotated, 2)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newMaterializeReconciler(tt.objs()...)
			r.cleanupOldSandboxTemplates(context.Background(), sbs)
			sbtList := &v1alpha1.SandboxTemplateList{}
			require.NoError(t, r.List(context.Background(), sbtList, client.InNamespace(sbs.Namespace)))
			tt.wantAnnotated(t, sbtList.Items)
		})
	}
}

func TestReconciler_cleanupOldSandboxTemplates_listError(t *testing.T) {
	sbs := newInlineSandboxSet("sbs-listerr", "img:v1")
	sbs.Spec.Template = nil

	// Build a fake client whose List call always fails.
	base := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(sbs).
		WithIndex(&v1alpha1.SandboxTemplate{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return errors.New("simulated list error")
			},
		}).
		Build()
	r := &Reconciler{Client: base, Scheme: testScheme}

	// Should not panic; error is logged internally.
	r.cleanupOldSandboxTemplates(context.Background(), sbs)
}

func TestReconciler_cleanupOldSandboxTemplates_patchError(t *testing.T) {
	sbs := newInlineSandboxSet("sbs-patcherr", "img:v1")
	sbs.Spec.Template = nil
	base := time.Now()

	var objs []client.Object
	for i := 0; i < 12; i++ {
		sbt := &v1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:              fmt.Sprintf("%s-h%d", sbs.Name, i),
				Namespace:         sbs.Namespace,
				CreationTimestamp: metav1.NewTime(base.Add(time.Duration(i) * time.Second)),
			},
		}
		require.NoError(t, ctrl.SetControllerReference(sbs, sbt, testScheme))
		objs = append(objs, sbt)
	}
	objs = append(objs, sbs)

	// Build client that rejects Patch on the two oldest templates (h0, h1).
	base2 := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objs...).
		WithIndex(&v1alpha1.SandboxTemplate{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if obj.GetName() == fmt.Sprintf("%s-h0", sbs.Name) || obj.GetName() == fmt.Sprintf("%s-h1", sbs.Name) {
					return errors.New("simulated patch error")
				}
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := &Reconciler{Client: base2, Scheme: testScheme}

	// Should not panic; errors are logged for h0 and h1.
	r.cleanupOldSandboxTemplates(context.Background(), sbs)

	// h0 and h1 are not annotated because Patch returned an error.
	sbtList := &v1alpha1.SandboxTemplateList{}
	require.NoError(t, r.List(context.Background(), sbtList, client.InNamespace(sbs.Namespace)))
	for i := range sbtList.Items {
		if sbtList.Items[i].Name == fmt.Sprintf("%s-h0", sbs.Name) || sbtList.Items[i].Name == fmt.Sprintf("%s-h1", sbs.Name) {
			assert.Empty(t, sbtList.Items[i].Annotations[v1alpha1.AnnotationCleanupCandidate])
		}
	}
}

func TestReconciler_ensureSandboxTemplate_errorPaths(t *testing.T) {
	tests := []struct {
		name        string
		build       func() (*Reconciler, *v1alpha1.SandboxSet, *v1alpha1.SandboxTemplateSpec, string)
		expectError string
	}{
		{
			name: "Template nil returns empty name and no error",
			build: func() (*Reconciler, *v1alpha1.SandboxSet, *v1alpha1.SandboxTemplateSpec, string) {
				sbs := &v1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "sbs-nil", Namespace: "default", UID: types.UID("uid-nil")},
					Spec:       v1alpha1.SandboxSetSpec{Replicas: 1},
				}
				return newMaterializeReconciler(sbs), sbs, &v1alpha1.SandboxTemplateSpec{}, "abc"
			},
		},
		{
			name: "Create failure propagated to caller",
			build: func() (*Reconciler, *v1alpha1.SandboxSet, *v1alpha1.SandboxTemplateSpec, string) {
				sbs := newInlineSandboxSet("sbs-createerr", "img:v1")
				base := fake.NewClientBuilder().
					WithScheme(testScheme).
					WithObjects(sbs).
					WithIndex(&v1alpha1.SandboxTemplate{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
					WithInterceptorFuncs(interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
							if _, ok := obj.(*v1alpha1.SandboxTemplate); ok {
								return errors.New("simulated create error")
							}
							return c.Create(ctx, obj, opts...)
						},
					}).
					Build()
				r := &Reconciler{Client: base, Scheme: testScheme}
				spec := &v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
					},
				}
				return r, sbs, spec, "hash1"
			},
			expectError: "simulated create error",
		},
		{
			name: "AlreadyExists with mismatched owner returns error",
			build: func() (*Reconciler, *v1alpha1.SandboxSet, *v1alpha1.SandboxTemplateSpec, string) {
				sbs := newInlineSandboxSet("sbs-mismatch", "img:v1")
				// Pre-create an SBT with the expected name but owned by a
				// different SandboxSet.
				otherSBS := &v1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{Name: "other-sbs", Namespace: "default", UID: types.UID("uid-other")},
				}
				spec := &v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
					},
				}
				hash := "hash1"
				name := fmt.Sprintf("%s-%s", sbs.Name, hash)
				existingSBT := &v1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
					Spec:       *spec,
				}
				require.NoError(t, ctrl.SetControllerReference(otherSBS, existingSBT, testScheme))
				r := newMaterializeReconciler(sbs, existingSBT)
				return r, sbs, spec, hash
			},
			expectError: "already exists but is not owned by this SandboxSet",
		},
		{
			name: "AlreadyExists but Get fails returns error",
			build: func() (*Reconciler, *v1alpha1.SandboxSet, *v1alpha1.SandboxTemplateSpec, string) {
				sbs := newInlineSandboxSet("sbs-getfail", "img:v1")
				spec := &v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "x"}}},
					},
				}
				// Client returns AlreadyExists on Create but then Get
				// fails with a transient error.
				base := fake.NewClientBuilder().
					WithScheme(testScheme).
					WithObjects(sbs).
					WithIndex(&v1alpha1.SandboxTemplate{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
					WithInterceptorFuncs(interceptor.Funcs{
						Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
							if _, ok := obj.(*v1alpha1.SandboxTemplate); ok {
								return apierrors.NewAlreadyExists(v1alpha1.Resource("sandboxtemplates"), obj.GetName())
							}
							return c.Create(ctx, obj, opts...)
						},
						Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
							if _, ok := obj.(*v1alpha1.SandboxTemplate); ok {
								return errors.New("simulated get error")
							}
							return c.Get(ctx, key, obj, opts...)
						},
					}).
					Build()
				r := &Reconciler{Client: base, Scheme: testScheme}
				return r, sbs, spec, "hash1"
			},
			expectError: "failed to verify existing SandboxTemplate",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, sbs, spec, hash := tt.build()
			name, err := r.ensureSandboxTemplate(context.Background(), sbs, spec, hash)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, name) // Template==nil path returns empty name
		})
	}
}

func TestReconciler_ensureTemplateRevision_templateNilError(t *testing.T) {
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sbs-notpl", Namespace: "default", UID: types.UID("uid-notpl")},
		Spec:       v1alpha1.SandboxSetSpec{Replicas: 1},
	}
	r := newMaterializeReconciler(sbs)
	_, _, err := r.ensureTemplateRevision(context.Background(), sbs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has neither spec.templateRef nor spec.template")
}

func TestReconciler_cleanupOldSandboxTemplates_skipAlreadyAnnotated(t *testing.T) {
	sbs := newInlineSandboxSet("sbs-skip", "img:v1")
	sbs.Spec.Template = nil
	base := time.Now()

	var objs []client.Object
	for i := 0; i < 12; i++ {
		sbt := &v1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{
				Name:              fmt.Sprintf("%s-h%d", sbs.Name, i),
				Namespace:         sbs.Namespace,
				CreationTimestamp: metav1.NewTime(base.Add(time.Duration(i) * time.Second)),
			},
		}
		require.NoError(t, ctrl.SetControllerReference(sbs, sbt, testScheme))
		// Pre-annotate the two oldest so cleanup should skip them.
		if i < 2 {
			sbt.Annotations = map[string]string{v1alpha1.AnnotationCleanupCandidate: v1alpha1.True}
		}
		objs = append(objs, sbt)
	}
	objs = append(objs, sbs)

	// Build a client that counts Patch calls.
	var patchCount int
	fc := fake.NewClientBuilder().
		WithScheme(testScheme).
		WithObjects(objs...).
		WithIndex(&v1alpha1.SandboxTemplate{}, fieldindex.IndexNameForOwnerRefUID, fieldindex.OwnerIndexFunc).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				patchCount++
				return c.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	r := &Reconciler{Client: fc, Scheme: testScheme}

	r.cleanupOldSandboxTemplates(context.Background(), sbs)
	// No Patch calls because h0 and h1 are already annotated.
	assert.Zero(t, patchCount)
}
