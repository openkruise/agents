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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
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
		ObjectMeta: metav1.ObjectMeta{Name: "st-abcdefghijklmnopqrst", Namespace: "default", UID: "version-uid", Generation: 1, CreationTimestamp: metav1.Now(), Labels: map[string]string{api.LabelTemplateName: "python-agent"}},
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
	require.NoError(t, api.AddToScheme(s))
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(objects...).
		WithStatusSubresource(&api.SandboxTemplate{}, &api.Sandbox{}, &corev1.Pod{}, &api.Checkpoint{}).Build()
	return &Reconciler{Client: c, Options: Options{Timeout: time.Minute}}
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
		{name: "unsubmitted Ready record is retained", age: 2 * time.Hour, phase: api.SandboxTemplatePhaseReady, mutate: func(v *api.SandboxTemplate) {
			v.Status.Phase, v.Status.ObservedGeneration = api.SandboxTemplatePhaseReady, v.Generation
		}},
		{name: "stale terminal result does not skip Draft handling", phase: api.SandboxTemplatePhasePending, mutate: func(v *api.SandboxTemplate) {
			v.Status.Phase, v.Status.ObservedGeneration = api.SandboxTemplatePhaseFailed, v.Generation-1
		}},
		{name: "static definition Ready", phase: api.SandboxTemplatePhaseReady, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = "test/image"
		}},
		{name: "commands never fake Ready", phase: api.SandboxTemplatePhaseBuilding, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = "test/image"
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{Steps: []api.TemplateBuildStep{{Command: &api.TemplateBuildCommand{Command: "echo build"}}}}
			v.Spec.Checkpoint = &api.CheckpointStrategy{Enable: true}
		}},
		{name: "capture without steps waits", phase: api.SandboxTemplatePhaseBuilding, mutate: func(v *api.SandboxTemplate) {
			v.Spec.Template.Spec.Containers[0].Image = "test/image"
			v.Spec.BuildOptions = &api.SandboxTemplateBuildOptions{}
			v.Spec.Checkpoint = &api.CheckpointStrategy{Enable: true}
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
			getErr := r.Get(t.Context(), client.ObjectKeyFromObject(version), current)
			if tt.expired {
				require.True(t, apierrors.IsNotFound(getErr), "%v", getErr)
				return
			}
			require.NoError(t, getErr)
			require.Equal(t, tt.phase, current.Status.Phase)
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
	// The expired draft is deleted; the apiserver's ownerReference garbage
	// collection releases its Sandbox and Checkpoint, which the fake client
	// does not model, so only the sibling's resources are asserted here.
	require.True(t, apierrors.IsNotFound(r.Get(t.Context(), req.NamespacedName, &api.SandboxTemplate{})))
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

func TestDeletingVersionIgnoresRestoredSandboxReferences(t *testing.T) {
	version := modelFixtures()
	version.DeletionTimestamp = ptr.To(metav1.Now())
	version.Finalizers = []string{"test/retain"}
	version.Spec.Template.Spec.Containers[0].Image = "test/image"
	consumer := &api.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "consumer", Namespace: version.Namespace, UID: "consumer-uid", Annotations: map[string]string{api.AnnotationRestoreFrom: "backend-artifact"}}}
	r := modelReconciler(t, version, consumer)
	req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(version)}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r.workers = map[types.UID]*buildWorker{version.UID: {key: req.NamespacedName, cancel: cancel}}
	_, err := r.ReconcileVersion(t.Context(), req)
	require.NoError(t, err)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	// A delivered sandbox is a snapshot, not a live reference: deleting the
	// Version never blocks on or touches it; owned resources are released by
	// ownerReference garbage collection.
	require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(consumer), &api.Sandbox{}))
}

func TestRequestDeletionBoundaries(t *testing.T) {
	t.Run("already deleting reports pending", func(t *testing.T) {
		version := modelFixtures()
		version.DeletionTimestamp = ptr.To(metav1.Now())
		version.Finalizers = []string{"test/retain"}
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
	t.Run("Checkpoint read failure", func(t *testing.T) {
		r, version, box, _ := executionFixture(t)
		version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseFailed, ObservedGeneration: version.Generation}
		require.NoError(t, r.Status().Update(t.Context(), version))
		require.NoError(t, r.Delete(t.Context(), box))
		failure := errors.New("checkpoint read unavailable")
		r.Client = interceptor.NewClient(r.Client.(client.WithWatch), failGetFor(&api.Checkpoint{}, failure))
		_, err := r.reconcileBuildCleanup(t.Context(), version)
		require.ErrorIs(t, err, failure)
	})
	t.Run("foreign Checkpoint at the deterministic key is skipped", func(t *testing.T) {
		r, version, box, _ := executionFixture(t)
		version.Status = api.SandboxTemplateStatus{Phase: api.SandboxTemplatePhaseFailed, ObservedGeneration: version.Generation}
		require.NoError(t, r.Status().Update(t.Context(), version))
		require.NoError(t, r.Delete(t.Context(), box))
		foreign := &api.Checkpoint{ObjectMeta: metav1.ObjectMeta{
			Namespace: version.Namespace, Name: version.Name + "-checkpoint", UID: "foreign-uid",
			OwnerReferences: []metav1.OwnerReference{{APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate", Name: version.Name, UID: "replacement-version-uid", Controller: ptr.To(true)}},
		}}
		require.NoError(t, r.Create(t.Context(), foreign))
		_, err := r.reconcileBuildCleanup(t.Context(), version)
		require.NoError(t, err)
		current := &api.Checkpoint{}
		require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(foreign), current))
		require.Nil(t, current.DeletionTimestamp)
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
