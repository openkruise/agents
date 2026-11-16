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

package poolautoscaler

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	api "github.com/openkruise/agents/api/v1alpha1"
)

func managedFixture() (*api.PoolAutoscaler, *api.SandboxSet) {
	owner := metav1.OwnerReference{
		APIVersion: api.GroupVersion.String(), Kind: "SandboxTemplate",
		Name: "abcdefghijklmnopqrst", UID: "version", Controller: ptr.To(true),
	}
	// Resource names and template references need not match the controller owner.
	pa := newPoolAutoscaler("autoscaler", "team", "pool", 10, &api.CapacityPolicy{TargetAvailable: intstr.FromInt32(2)})
	pa.UID = "autoscaler"
	pa.Spec.MinReplicas = 2
	pa.Spec.ScaleTargetRef.APIVersion = api.GroupVersion.String()
	pa.OwnerReferences = []metav1.OwnerReference{owner}
	pa.Finalizers = []string{"test/hold"}
	p := newSandboxSet("pool", pa.Namespace, 0, 0, 0)
	p.UID = "pool"
	p.Finalizers = []string{"test/hold"}
	p.OwnerReferences = []metav1.OwnerReference{owner}
	return pa, p
}

func TestManagedAutoscalerStopsOnObservedDeletionOrOwnerMismatch(t *testing.T) {
	const ownerMismatch = "pool does not share the autoscaler's SandboxTemplate controller owner"
	const deleting = "autoscaler or pool is deleting"
	tests := []struct {
		name            string
		mutate          func(*api.PoolAutoscaler, *api.SandboxSet)
		unmanaged       bool
		inactiveMessage string
	}{
		{name: "same controller owner without template or claim objects"},
		{name: "different owner namespace", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.Namespace = "other" }, inactiveMessage: ownerMismatch},
		{name: "different owner name", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.OwnerReferences[0].Name = "other-version" }, inactiveMessage: ownerMismatch},
		{name: "different actual owner UID", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.OwnerReferences[0].UID = "replacement-version" }, inactiveMessage: ownerMismatch},
		{name: "pool has no controller owner", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.OwnerReferences = nil }, inactiveMessage: ownerMismatch},
		{name: "pool owner is not controller", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.OwnerReferences[0].Controller = ptr.To(false) }, inactiveMessage: ownerMismatch},
		{name: "pool owner has different kind", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.OwnerReferences[0].Kind = "SandboxTemplateClaim" }, inactiveMessage: ownerMismatch},
		{name: "pool owner has different API version", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.OwnerReferences[0].APIVersion = "other.example/v1" }, inactiveMessage: ownerMismatch},
		{name: "pool owner has no actual UID", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.OwnerReferences[0].UID = "" }, inactiveMessage: ownerMismatch},
		{name: "managed pool deletion", mutate: func(_ *api.PoolAutoscaler, p *api.SandboxSet) { p.DeletionTimestamp = ptr.To(metav1.Now()) }, inactiveMessage: deleting},
		{name: "managed autoscaler deletion", mutate: func(pa *api.PoolAutoscaler, _ *api.SandboxSet) { pa.DeletionTimestamp = ptr.To(metav1.Now()) }, inactiveMessage: deleting},
		{name: "target and definition references are not managed checks", mutate: func(pa *api.PoolAutoscaler, p *api.SandboxSet) {
			pa.Spec.ScaleTargetRef.Kind, pa.Spec.ScaleTargetRef.APIVersion = "Other", "other.example/v1"
			p.Spec.TemplateRef = &api.SandboxTemplateRef{Name: "other-template"}
		}},
		{name: "unmanaged autoscaler can scale a template-owned pool", mutate: func(pa *api.PoolAutoscaler, _ *api.SandboxSet) { pa.OwnerReferences = nil }, unmanaged: true},
		{name: "non-controller template owner is unmanaged", mutate: func(pa *api.PoolAutoscaler, p *api.SandboxSet) {
			pa.OwnerReferences[0].Controller, p.OwnerReferences[0].UID = ptr.To(false), "foreign"
		}, unmanaged: true},
		{name: "different autoscaler controller kind is unmanaged", mutate: func(pa *api.PoolAutoscaler, _ *api.SandboxSet) { pa.OwnerReferences[0].Kind = "Other" }, unmanaged: true},
		{name: "unmanaged pool deletion keeps existing scaling behavior", mutate: func(pa *api.PoolAutoscaler, p *api.SandboxSet) {
			pa.OwnerReferences, p.DeletionTimestamp = nil, ptr.To(metav1.Now())
		}, unmanaged: true},
		{name: "unmanaged autoscaler deletion still stops at reconcile entry", mutate: func(pa *api.PoolAutoscaler, _ *api.SandboxSet) {
			pa.OwnerReferences, pa.DeletionTimestamp = nil, ptr.To(metav1.Now())
		}, unmanaged: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pa, p := managedFixture()
			if tt.mutate != nil {
				tt.mutate(pa, p)
			}
			managed, guardErr := checkManagedTarget(pa, p)
			require.Equal(t, !tt.unmanaged, managed)
			if tt.inactiveMessage != "" {
				require.ErrorIs(t, guardErr, errInactiveManagedTarget)
				require.EqualError(t, guardErr, errInactiveManagedTarget.Error()+": "+tt.inactiveMessage)
			} else {
				require.NoError(t, guardErr)
			}
			// Reconcile itself resolves targets only within the autoscaler's namespace.
			if p.Namespace != pa.Namespace {
				return
			}
			r := newTestReconciler(pa, p)
			key := client.ObjectKeyFromObject(pa)
			r.getOrCreateMonitorFor(key, pa.UID, pa.Spec.ScaleTargetRef.Name, p.UID)
			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			require.NoError(t, err)
			got := &api.SandboxSet{}
			require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(p), got))
			want := int32(2)
			if guardErr != nil || pa.DeletionTimestamp != nil {
				want = 0
				require.NotContains(t, r.monitors, key)
			} else {
				wantUID := p.UID
				if tt.unmanaged {
					wantUID = ""
				}
				require.Equal(t, wantUID, r.monitors[key].targetUID)
			}
			require.Equal(t, want, got.Spec.Replicas)
			gotPA := &api.PoolAutoscaler{}
			require.NoError(t, r.Get(t.Context(), key, gotPA))
			condition := apiMeta.FindStatusCondition(gotPA.Status.Conditions, string(api.PoolAutoscalerConditionScalingActive))
			if pa.DeletionTimestamp != nil {
				require.Nil(t, condition, "deleting autoscaler must stop before updating status")
			} else if guardErr != nil {
				require.NotNil(t, condition)
				require.Equal(t, metav1.ConditionFalse, condition.Status)
				require.Equal(t, "InactiveManagedTarget", condition.Reason)
				require.Equal(t, guardErr.Error(), condition.Message)
			} else {
				require.NotNil(t, condition)
				require.NotEqual(t, "InactiveManagedTarget", condition.Reason)
			}
		})
	}
}

func TestManagedTargetMonitorResetsOnPoolUID(t *testing.T) {
	pa, p := managedFixture()
	r := newTestReconciler(pa, p)
	key := client.ObjectKeyFromObject(pa)
	old := r.getOrCreateMonitorFor(key, pa.UID, pa.Spec.ScaleTargetRef.Name, p.UID)
	old.samples = []sample{{timestamp: time.Now(), available: 99, statusReplicas: 99}}
	old.lastScaleUpAt = time.Now()
	old.lastScaleDownAt = time.Now()
	p.UID = "rebuilt-pool"
	// Production observation and stabilization both carry the actual target UID.
	avail, _, _ := r.observeAndAggregate(t.Context(), pa, 3, 4, p.UID)
	current := r.getOrCreateMonitorFor(key, pa.UID, pa.Spec.ScaleTargetRef.Name, p.UID)
	if current == old || avail != 3 || !current.lastScaleUpAt.IsZero() || !current.lastScaleDownAt.IsZero() {
		t.Fatal("old target history leaked")
	}
	if _, blocked, _ := r.applyStabilizationWindow(pa, 2, 3, p.UID); blocked {
		t.Fatal("old target cooldown leaked")
	}
	if r.getOrCreateMonitorFor(key, pa.UID, pa.Spec.ScaleTargetRef.Name, p.UID) != current {
		t.Fatal("same target reset")
	}
}

func TestScaleTargetWritesObservedSnapshot(t *testing.T) {
	for _, managed := range []bool{true, false} {
		name := "managed"
		if !managed {
			name = "unmanaged"
		}
		t.Run(name, func(t *testing.T) {
			pa, p := managedFixture()
			r := newTestReconciler(pa, p)
			base := r.Client.(client.WithWatch)
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(p), p))
			rv := p.ResourceVersion
			writes := 0
			r.Client = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					t.Fatal("scaling must not reread cached objects")
					return nil
				},
				Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					writes++
					data, err := patch.Data(obj)
					require.NoError(t, err)
					var fields map[string]map[string]any
					require.NoError(t, json.Unmarshal(data, &fields))
					require.Equal(t, types.MergePatchType, patch.Type())
					require.Equal(t, map[string]any{"replicas": float64(5)}, fields["spec"])
					if managed {
						require.NotEmpty(t, rv)
						require.Len(t, fields, 2)
						require.Equal(t, map[string]any{"resourceVersion": rv}, fields["metadata"])
					} else {
						require.Len(t, fields, 1, "unmanaged patch must not gain a resourceVersion precondition")
					}
					return cl.Patch(ctx, obj, patch, opts...)
				},
			})
			require.NoError(t, r.scaleTarget(t.Context(), p, 5, managed))
			require.Equal(t, 1, writes)
			got := &api.SandboxSet{}
			require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(p), got))
			require.Equal(t, p.UID, got.UID)
			require.Equal(t, int32(5), got.Spec.Replicas)
		})
	}
}

func TestManagedScalePatchBindsRVAndRejectsLateReplacement(t *testing.T) {
	pa, p := managedFixture()
	r := newTestReconciler(pa, p)
	base := r.Client.(client.WithWatch)
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(p), p))
	observed := p.DeepCopy()
	r.Client = interceptor.NewClient(base, interceptor.Funcs{Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
		data, err := patch.Data(obj)
		require.NoError(t, err)
		var fields map[string]map[string]any
		require.NoError(t, json.Unmarshal(data, &fields))
		require.Equal(t, types.MergePatchType, patch.Type())
		require.NotEmpty(t, observed.ResourceVersion)
		require.Equal(t, map[string]map[string]any{
			"metadata": {"resourceVersion": observed.ResourceVersion},
			"spec":     {"replicas": float64(5)},
		}, fields)
		require.NoError(t, cl.Delete(ctx, observed))
		// Simulate deletion completion followed by replacement after observation.
		old := &api.SandboxSet{}
		require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(observed), old))
		old.Finalizers = nil
		require.NoError(t, cl.Update(ctx, old))
		replacement := observed.DeepCopy()
		replacement.UID = "replacement"
		replacement.ResourceVersion = ""
		replacement.Finalizers = nil
		replacement.DeletionTimestamp = nil
		require.NoError(t, cl.Create(ctx, replacement))
		// API-server resourceVersions are distinct across same-name pool lifecycles.
		require.NotEmpty(t, replacement.ResourceVersion)
		require.NotEqual(t, observed.ResourceVersion, replacement.ResourceVersion)
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	err := r.scaleTarget(t.Context(), p, 5, true)
	require.True(t, apierrors.IsConflict(err), "late replacement must reject the stale patch with a conflict: %v", err)
	got := &api.SandboxSet{}
	require.NoError(t, base.Get(t.Context(), client.ObjectKeyFromObject(observed), got))
	require.Equal(t, types.UID("replacement"), got.UID)
	require.Equal(t, observed.Spec.Replicas, got.Spec.Replicas, "replacement must remain unchanged")
}
