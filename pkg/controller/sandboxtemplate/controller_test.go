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

package sandboxtemplate

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/clock"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func modelFixtures() *api.SandboxTemplate {
	resources := corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}
	version := &api.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "st-abcdefghijklmnopqrst", Namespace: "default", UID: "version-uid", Generation: 1, CreationTimestamp: metav1.Now(), Finalizers: []string{api.SandboxTemplateFinalizer}, Labels: map[string]string{api.LabelTemplateName: "python-agent"}},
		Spec: api.SandboxTemplateSpec{Backend: api.SandboxTemplateBackendKubernetes, Template: &corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "runtime", Resources: corev1.ResourceRequirements{Requests: resources.DeepCopy(), Limits: resources.DeepCopy()}}},
		}}, Runtimes: []api.RuntimeConfig{{Name: "agent-runtime"}}},
	}
	return version
}

func modelReconciler(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(s))
	require.NoError(t, coordinationv1.AddToScheme(s))
	require.NoError(t, api.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).
		WithStatusSubresource(&api.SandboxTemplate{}, &api.Sandbox{}, &corev1.Pod{}, &api.Checkpoint{}).Build()
	return &Reconciler{Client: c, Options: Options{Timeout: time.Minute}, instanceID: uuid.NewString(), leaseClock: clock.RealClock{}}
}

func TestParseExecutionIdentity(t *testing.T) {
	for _, tt := range []struct {
		name     string
		raw      string
		identity executionIdentity
		valid    bool
	}{
		{name: "absent annotation", valid: true},
		{name: "empty object", raw: "{}", valid: true},
		{name: "execution identity", raw: `{"sandboxUID":"box-1","podUID":"pod-1","containerID":"container-1"}`, identity: executionIdentity{SandboxUID: "box-1", PodUID: "pod-1", ContainerID: "container-1"}, valid: true},
		{name: "malformed JSON", raw: "{"},
		{name: "null is not an identity object", raw: "null"},
		{name: "array is not an identity object", raw: "[]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			identity, err := parseExecutionIdentity(tt.raw)
			if !tt.valid {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.identity, identity)
		})
	}
}

func TestVersionPhases(t *testing.T) {
	for _, tt := range []struct {
		name    string
		mutate  func(*api.SandboxTemplate)
		phase   api.SandboxTemplateBuildPhase
		age     time.Duration
		timeout time.Duration
		expired bool
	}{
		{name: "Draft waits", phase: api.SandboxTemplatePhasePending},
		{name: "Draft default timeout waits", age: 20 * time.Minute, phase: api.SandboxTemplatePhasePending},
		{name: "Draft default timeout expires", age: 40 * time.Minute, expired: true},
		{name: "Draft shorter timeout waits", age: 2 * time.Minute, timeout: 5 * time.Minute, phase: api.SandboxTemplatePhasePending},
		{name: "Draft shorter timeout expires", age: 10 * time.Minute, timeout: 5 * time.Minute, expired: true},
		{name: "Draft longer timeout waits", age: 90 * time.Minute, timeout: 2 * time.Hour, phase: api.SandboxTemplatePhasePending},
		{name: "Draft longer timeout expires", age: 3 * time.Hour, timeout: 2 * time.Hour, expired: true},
		{name: "static definition Ready", phase: api.SandboxTemplatePhaseReady, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = "test/image"
		}},
		{name: "commands never fake Ready", phase: api.SandboxTemplatePhaseBuilding, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = "test/image"
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}, Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo build"}}}}
		}},
		{name: "capture without steps waits", phase: api.SandboxTemplatePhaseBuilding, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = "test/image"
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Checkpoint: &api.CheckpointStrategy{Enable: true}}
		}},
		{name: "unmanaged template untouched", mutate: func(v *api.SandboxTemplate) { v.Labels = nil }},
		{name: "empty logical name blocks processing", mutate: func(v *api.SandboxTemplate) { v.Labels[api.LabelTemplateName] = "" }},
		{name: "unrelated label does not manage template", mutate: func(v *api.SandboxTemplate) { v.Labels = map[string]string{"editor": "metadata"} }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			version := modelFixtures()
			version.CreationTimestamp = metav1.NewTime(time.Now().Add(-tt.age))
			if tt.mutate != nil {
				tt.mutate(version)
			}
			r := modelReconciler(t, version)
			r.Options.Timeout = DefaultBuildTimeout
			if tt.timeout > 0 {
				r.Options.Timeout = tt.timeout
			}
			result, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)})
			require.NoError(t, err)
			current := &api.SandboxTemplate{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			require.Equal(t, tt.phase, current.Status.Phase)
			require.Equal(t, tt.expired, current.DeletionTimestamp != nil)
			require.Empty(t, current.Status.Conditions)
			if tt.phase == api.SandboxTemplatePhasePending {
				require.Greater(t, result.RequeueAfter, time.Duration(0))
				require.LessOrEqual(t, result.RequeueAfter, r.Options.Timeout)
			}
		})
	}
}

func TestLogicalNameRetainsIndependentVersions(t *testing.T) {
	for _, count := range []int{0, 2, 5000} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			first := modelFixtures()
			objects := []client.Object{}
			expected := map[string]*api.SandboxTemplate{}
			key := client.ObjectKey{Namespace: first.Namespace, Name: "st-00000000000000000000"}
			for i := 0; i < count; i++ {
				version := first.DeepCopy()
				version.Name = fmt.Sprintf("st-%020d", i)
				version.UID = types.UID(fmt.Sprintf("version-%d", i))
				if i == 0 {
					version.Spec.Template.Spec.Containers[0].Image = "test/image"
				}
				objects = append(objects, version)
				expected[version.Name] = version.DeepCopy()
			}
			r := modelReconciler(t, objects...)
			_, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			versions := &api.SandboxTemplateList{}
			require.NoError(t, r.List(t.Context(), versions, client.InNamespace(first.Namespace)))
			require.Len(t, versions.Items, count)
			for _, current := range versions.Items {
				original := expected[current.Name]
				require.NotNil(t, original)
				require.Equal(t, original.UID, current.UID)
				require.Equal(t, original.Spec, current.Spec)
				require.Equal(t, map[string]string{api.LabelTemplateName: "python-agent"}, current.Labels)
				require.Empty(t, current.OwnerReferences)
				require.Nil(t, current.DeletionTimestamp)
				require.Empty(t, current.Annotations)
				if current.Name == key.Name {
					require.Equal(t, api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: 1}, current.Status)
				} else {
					require.Equal(t, original.Status, current.Status)
				}
			}
		})
	}
}

func TestDraftExpiryIsolatedFromSibling(t *testing.T) {
	version := modelFixtures()
	version.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	sibling := version.DeepCopy()
	sibling.Name, sibling.UID = "st-bcdefghijklmnopqrstu0", "sibling-uid"
	sibling.Spec.Template.Spec.Containers[0].Image = "test/image"
	sibling.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: 1}
	owner := []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true)}}
	box := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: version.Name, Namespace: version.Namespace, UID: "box-uid", OwnerReferences: owner}}
	checkpoint := &api.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: version.Name + "-checkpoint", Namespace: version.Namespace, UID: "checkpoint-uid", OwnerReferences: owner}}
	siblingBox := box.DeepCopy()
	siblingBox.Name, siblingBox.UID = sibling.Name, "sibling-box-uid"
	siblingBox.OwnerReferences[0].Name, siblingBox.OwnerReferences[0].UID = sibling.Name, sibling.UID
	siblingCheckpoint := checkpoint.DeepCopy()
	siblingCheckpoint.Name, siblingCheckpoint.UID = sibling.Name+"-checkpoint", "sibling-checkpoint-uid"
	siblingCheckpoint.OwnerReferences[0].Name, siblingCheckpoint.OwnerReferences[0].UID = sibling.Name, sibling.UID
	r := modelReconciler(t, version, sibling, box, checkpoint, siblingBox, siblingCheckpoint)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
	_, err := r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, current))
	require.NotNil(t, current.DeletionTimestamp)
	require.Contains(t, current.Finalizers, api.SandboxTemplateFinalizer)
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(box), &api.Sandbox{}))
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), &api.Checkpoint{}))
	for i := 0; i < 6; i++ {
		_, err = r.ReconcileVersion(t.Context(), req)
		require.NoError(t, err)
	}
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), req.NamespacedName, &api.SandboxTemplate{})))
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(box), &api.Sandbox{})))
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), &api.Checkpoint{})))
	currentSibling := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(sibling), currentSibling))
	require.Equal(t, sibling.Status, currentSibling.Status)
	require.Equal(t, sibling.Labels, currentSibling.Labels)
	require.Nil(t, currentSibling.DeletionTimestamp)
	require.Empty(t, currentSibling.OwnerReferences)
	currentSiblingBox := &api.Sandbox{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(siblingBox), currentSiblingBox))
	require.Equal(t, siblingBox.UID, currentSiblingBox.UID)
	require.Nil(t, currentSiblingBox.DeletionTimestamp)
	currentSiblingCheckpoint := &api.Checkpoint{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(siblingCheckpoint), currentSiblingCheckpoint))
	require.Equal(t, siblingCheckpoint.UID, currentSiblingCheckpoint.UID)
	require.Nil(t, currentSiblingCheckpoint.DeletionTimestamp)
}

func TestSubmissionWinningExpiryKeepsBuildSandbox(t *testing.T) {
	version := modelFixtures()
	version.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	box := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: version.Name, Namespace: version.Namespace, UID: "box-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true)}}}}
	r := modelReconciler(t, version, box)
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
		if _, ok := obj.(*api.SandboxTemplate); ok {
			current := &api.SandboxTemplate{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), current))
			current.Spec.Template.Spec.Containers[0].Image = "test/image"
			require.NoError(t, c.Update(ctx, current))
		}
		return c.Delete(ctx, obj, opts...)
	}})
	_, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)})
	require.True(t, apierrors.IsConflict(err), "%v", err)
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	require.Nil(t, current.DeletionTimestamp)
	require.NotEmpty(t, current.Spec.Template.Spec.Containers[0].Image)
	currentBox := &api.Sandbox{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(box), currentBox))
	require.Nil(t, currentBox.DeletionTimestamp)
}

func TestCleanupFinalizerUsesForegroundDeletion(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		t.Run(map[bool]string{false: "Version retains GC finalizer", true: "Version retains protection after rejected deletion"}[rejected], func(t *testing.T) {
			version := modelFixtures()
			version.DeletionTimestamp = ptr.To(metav1.Now())
			box := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: version.Name, Namespace: version.Namespace, UID: "box-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)}}}}
			r := modelReconciler(t, version, box)
			base := r.Client.(client.WithWatch)
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(version), version))
			var deletes int
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					t.Fatal("cleanup must not recreate resources")
					return nil
				},
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					if boxes, ok := list.(*api.SandboxList); ok {
						boxes.Items = nil
						return nil
					}
					return c.List(ctx, list, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					deletes++
					require.IsType(t, version, obj)
					options := &client.DeleteOptions{}
					for _, option := range opts {
						option.ApplyToDelete(options)
					}
					require.Equal(t, metav1.DeletePropagationForeground, *options.PropagationPolicy)
					require.Equal(t, version.UID, *options.Preconditions.UID)
					require.Equal(t, version.ResourceVersion, *options.Preconditions.ResourceVersion)
					if rejected {
						return fmt.Errorf("foreground deletion rejected")
					}
					current := obj.DeepCopyObject().(client.Object)
					require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(obj), current))
					// The fake client does not add the apiserver's foreground finalizer.
					current.SetFinalizers(append(current.GetFinalizers(), metav1.FinalizerDeleteDependents))
					return c.Update(ctx, current)
				},
			})
			_, err := r.cleanupVersion(t.Context(), version)
			current := &api.SandboxTemplate{}
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(version), current))
			if rejected {
				require.ErrorContains(t, err, "foreground deletion rejected")
				require.Contains(t, current.Finalizers, api.SandboxTemplateFinalizer)
			} else {
				require.NoError(t, err)
				require.NotContains(t, current.Finalizers, api.SandboxTemplateFinalizer)
				require.Contains(t, current.Finalizers, metav1.FinalizerDeleteDependents)
			}
			require.Equal(t, 1, deletes)
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(box), &api.Sandbox{}))
		})
	}
}

func TestDeletingVersionIgnoresRestoredSandboxReferences(t *testing.T) {
	version := modelFixtures()
	version.DeletionTimestamp = ptr.To(metav1.Now())
	version.Spec.Template.Spec.Containers[0].Image = "test/image"
	checkpoint := &api.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Name: version.Name + "-checkpoint", Namespace: version.Namespace, UID: "checkpoint-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: version.UID, Controller: ptr.To(true)}}},
		Status:     api.CheckpointStatus{CheckpointId: "backend-artifact"},
	}
	consumer := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: version.Namespace, UID: "consumer-uid", Annotations: map[string]string{api.AnnotationRestoreFrom: "backend-artifact"}}}
	r := modelReconciler(t, version, checkpoint, consumer)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
	require.Eventually(t, func() bool {
		_, err := r.ReconcileVersion(t.Context(), req)
		return err == nil && apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(version), &api.SandboxTemplate{}))
	}, 5*time.Second, 10*time.Millisecond)
	// A delivered sandbox is a snapshot, not a live reference: it keeps
	// running while the Version and its checkpoint are removed.
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(consumer), &api.Sandbox{}))
	err := r.Get(t.Context(), client.ObjectKeyFromObject(checkpoint), &api.Checkpoint{})
	require.True(t, apierrors.IsNotFound(err), "checkpoint should be removed with the Version")
}

func TestRequestDeletionBoundaries(t *testing.T) {
	t.Run("already deleting reports pending", func(t *testing.T) {
		version := modelFixtures()
		version.DeletionTimestamp = ptr.To(metav1.Now())
		version.Finalizers = []string{api.SandboxTemplateFinalizer}
		r := modelReconciler(t, version)
		pending, err := r.requestDeletion(t.Context(), version, metav1.DeletePropagationForeground)
		require.NoError(t, err)
		require.True(t, pending)
	})
	t.Run("missing object reports done", func(t *testing.T) {
		version := modelFixtures()
		r := modelReconciler(t)
		pending, err := r.requestDeletion(t.Context(), version, metav1.DeletePropagationForeground)
		require.NoError(t, err)
		require.False(t, pending)
	})
}

func TestFinishCleanupBoundaries(t *testing.T) {
	t.Run("object gone after deletion", func(t *testing.T) {
		version := modelFixtures()
		version.Finalizers = nil
		r := modelReconciler(t, version)
		require.NoError(t, r.finishCleanup(t.Context(), version, api.SandboxTemplateFinalizer))
		require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(version), &api.SandboxTemplate{})))
	})
	t.Run("finalizer already removed", func(t *testing.T) {
		version := modelFixtures()
		version.Finalizers = []string{"test/retain"}
		r := modelReconciler(t, version)
		require.NoError(t, r.finishCleanup(t.Context(), version, api.SandboxTemplateFinalizer))
	})
	t.Run("recreated object fails identity check", func(t *testing.T) {
		version := modelFixtures()
		version.Finalizers = nil
		r := modelReconciler(t, version)
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			require.NoError(t, c.Delete(ctx, obj, opts...))
			replacement := modelFixtures()
			replacement.UID = "replacement-uid"
			replacement.Finalizers = []string{api.SandboxTemplateFinalizer}
			return c.Create(ctx, replacement)
		}})
		err := r.finishCleanup(t.Context(), version, api.SandboxTemplateFinalizer)
		require.ErrorContains(t, err, "identity changed")
	})
	t.Run("finalizer removal conflict retries", func(t *testing.T) {
		version := modelFixtures()
		r := modelReconciler(t, version)
		var patches atomic.Int32
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if current, ok := obj.(*api.SandboxTemplate); ok && patches.Add(1) == 1 {
				edited := current.DeepCopy()
				edited.Finalizers = []string{api.SandboxTemplateFinalizer}
				edited.Labels["editor"] = "winner"
				require.NoError(t, c.Update(ctx, edited))
				return apierrors.NewConflict(schema.GroupResource{Group: api.GroupVersion.Group, Resource: "sandboxtemplates"}, obj.GetName(), errors.New("edited"))
			}
			return c.Patch(ctx, obj, patch, opts...)
		}})
		require.NoError(t, r.finishCleanup(t.Context(), version, api.SandboxTemplateFinalizer))
		require.EqualValues(t, 2, patches.Load())
		require.True(t, apierrors.IsNotFound(r.Get(t.Context(), client.ObjectKeyFromObject(version), &api.SandboxTemplate{})))
	})
}

func TestCleanupVersionStorageBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name     string
		deleting bool
		noFinal  bool
		listKind string
		patch    bool
		delete40 bool
	}{
		{name: "finalizer patch fails", noFinal: true, patch: true},
		{name: "Sandbox list fails", deleting: true, listKind: "sandboxes"},
		{name: "Checkpoint list fails", deleting: true, listKind: "checkpoints"},
		{name: "deletion already finished", delete40: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			version := modelFixtures()
			switch {
			case tt.deleting:
				version.DeletionTimestamp = ptr.To(metav1.Now())
			case tt.noFinal:
				version.Finalizers = nil
			}
			r := modelReconciler(t, version)
			failure := errors.New("storage unavailable")
			funcs := interceptor.Funcs{}
			switch {
			case tt.patch:
				funcs.Patch = func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
					return failure
				}
			case tt.listKind == "sandboxes":
				funcs = failListFor(&api.SandboxList{}, failure)
			case tt.listKind == "checkpoints":
				funcs = failListFor(&api.CheckpointList{}, failure)
			case tt.delete40:
				funcs.Delete = func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					if _, ok := obj.(*api.SandboxTemplate); ok {
						return apierrors.NewNotFound(api.GroupVersion.WithResource("sandboxtemplates").GroupResource(), obj.GetName())
					}
					return c.Delete(ctx, obj, opts...)
				}
			}
			r.Client = interceptor.NewClient(r.Client.(client.WithWatch), funcs)
			pending, err := r.cleanupVersion(t.Context(), version)
			if tt.delete40 {
				require.NoError(t, err)
				require.False(t, pending)
				current := &api.SandboxTemplate{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
				require.Contains(t, current.Finalizers, api.SandboxTemplateFinalizer)
				require.Nil(t, current.DeletionTimestamp)
				return
			}
			require.False(t, pending)
			require.ErrorIs(t, err, failure)
		})
	}
}

func TestBuildCleanupBoundaries(t *testing.T) {
	t.Run("running worker defers cleanup", func(t *testing.T) {
		r, version, _, _ := executionFixture(t)
		version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: version.Generation}
		require.NoError(t, r.Status().Update(t.Context(), version))
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r.workers = map[types.UID]*buildWorker{version.UID: {key: client.ObjectKeyFromObject(version), cancel: cancel}}
		result, err := r.reconcileBuildCleanup(t.Context(), version)
		require.NoError(t, err)
		require.Equal(t, retryInterval, result.RequeueAfter)
		require.ErrorIs(t, ctx.Err(), context.Canceled)
	})
	t.Run("Sandbox read failure", func(t *testing.T) {
		r, version, _, _ := executionFixture(t)
		version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseReady, ObservedGeneration: version.Generation}
		require.NoError(t, r.Status().Update(t.Context(), version))
		failure := errors.New("sandbox read unavailable")
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), failGetFor(&api.Sandbox{}, failure))
		_, err := r.reconcileBuildCleanup(t.Context(), version)
		require.ErrorIs(t, err, failure)
	})
	t.Run("Checkpoint list failure", func(t *testing.T) {
		r, version, box, _ := executionFixture(t)
		version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseFailed, ObservedGeneration: version.Generation}
		require.NoError(t, r.Status().Update(t.Context(), version))
		require.NoError(t, r.Delete(t.Context(), box))
		failure := errors.New("checkpoint list unavailable")
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), failListFor(&api.CheckpointList{}, failure))
		_, err := r.reconcileBuildCleanup(t.Context(), version)
		require.ErrorIs(t, err, failure)
	})
}

func TestDraftExpiryStopsWorkerBeforeDeletion(t *testing.T) {
	version := modelFixtures()
	version.CreationTimestamp = metav1.NewTime(time.Now().Add(-2 * time.Hour))
	r := modelReconciler(t, version)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.workers = map[types.UID]*buildWorker{version.UID: {key: client.ObjectKeyFromObject(version), cancel: cancel}}
	result, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)})
	require.NoError(t, err)
	require.Equal(t, retryInterval, result.RequeueAfter)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	require.Nil(t, current.DeletionTimestamp)
}

func TestFinalizerPatchFailure(t *testing.T) {
	r, version, _, _ := executionFixture(t)
	current := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(version), current))
	current.Finalizers = nil
	require.NoError(t, r.Update(t.Context(), current))
	failure := errors.New("finalizer patch unavailable")
	r.Client = interceptor.NewClient(r.Client.(client.WithWatch), interceptor.Funcs{Patch: func(context.Context, client.WithWatch, client.Object, client.Patch, ...client.PatchOption) error {
		return failure
	}})
	_, err := r.ReconcileVersion(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)})
	require.ErrorIs(t, err, failure)
}

func TestSetPhaseIsIdempotent(t *testing.T) {
	version := modelFixtures()
	r := modelReconciler(t, version)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
	_, err := r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	first := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, first))
	require.Equal(t, api.SandboxTemplatePhasePending, first.Status.Phase)
	_, err = r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	second := &api.SandboxTemplate{}
	require.NoError(t, r.Get(t.Context(), req.NamespacedName, second))
	require.Equal(t, first.ResourceVersion, second.ResourceVersion)
}
