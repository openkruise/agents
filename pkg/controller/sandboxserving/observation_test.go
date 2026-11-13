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

package sandboxserving

import (
	"context"
	"encoding/json"
	"maps"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	agents "github.com/openkruise/agents/api/v1alpha1"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
)

func TestInitialStatusUpdateIncludesRequiredCounts(t *testing.T) {
	for _, unsupported := range []bool{false, true} {
		name := "supported"
		if unsupported {
			name = "unsupported"
		}
		t.Run(name, func(t *testing.T) {
			parent := serving(0)
			if unsupported {
				parent.Spec.Provision.SandboxTemplate.BuildOptions = &agents.SandboxTemplateBuildOptions{}
			}
			updates := 0
			store := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(parent).WithObjects(parent).Build()
			c := interceptor.NewClient(store, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, name string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				data, err := json.Marshal(obj)
				if err != nil {
					return err
				}
				var payload map[string]interface{}
				if err := json.Unmarshal(data, &payload); err != nil {
					return err
				}
				status := payload["status"].(map[string]interface{})
				for _, key := range []string{"replicas", "readyReplicas"} {
					if value, exists := status[key]; !exists || value != float64(0) {
						t.Fatalf("required initial status %s missing or nonzero: %v", key, value)
					}
				}
				updates++
				return c.SubResource(name).Update(ctx, obj, opts...)
			}})
			r := NewReconciler(c)
			reconcile(t, r, parent)
			reconcile(t, r, parent)
			if updates != 1 {
				t.Fatalf("unchanged initial status updated %d times", updates)
			}
		})
	}
}

func TestStatusUpdateRemovesDisappearedRevisionKeys(t *testing.T) {
	for _, keepCurrent := range []bool{false, true} {
		name := "empty map"
		if keepCurrent {
			name = "partial key removal"
		}
		t.Run(name, func(t *testing.T) {
			parent := serving(0)
			s := testScheme(t)
			updates := 0
			store := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent).Build()
			c := interceptor.NewClient(store, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, subresource string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				updates++
				return c.SubResource(subresource).Update(ctx, obj, opts...)
			}})
			r := NewReconciler(c)
			reconcile(t, r, parent)
			if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
				t.Fatal(err)
			}
			want := map[string]int32{}
			if keepCurrent {
				*parent.Spec.Replicas = 1
				if err := store.Update(context.Background(), parent); err != nil {
					t.Fatal(err)
				}
				sbx := member(parent, "live")
				sbx.ResourceVersion = ""
				sbx.Labels[sessionbinding.ServingRevisionLabel] = parent.Status.CurrentRevision
				if err := store.Create(context.Background(), sbx); err != nil {
					t.Fatal(err)
				}
				want[parent.Status.CurrentRevision] = 1
			}
			parent.Status.ReplicasByRevision = maps.Clone(want)
			parent.Status.ReplicasByRevision["gone"] = 1
			if err := store.Status().Update(context.Background(), parent); err != nil {
				t.Fatal(err)
			}
			updates = 0
			reconcile(t, r, parent)
			if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(parent.Status.ReplicasByRevision, want) {
				t.Fatalf("persisted stale revision keys: got=%v want=%v", parent.Status.ReplicasByRevision, want)
			}
			reconcile(t, r, parent)
			if updates != 1 {
				t.Fatalf("repeated update after status key removal: %d", updates)
			}
		})
	}
}

func TestStatusUpdatePreservesRVConflictAndInputs(t *testing.T) {
	parent := serving(0)
	s := testScheme(t)
	store := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(parent).WithObjects(parent).Build()
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	previous := agents.SandboxServingStatus{CurrentRevision: "gone", UpdateRevision: "gone", ReplicasByRevision: map[string]int32{"gone": 1}}
	desired := &agents.SandboxServingStatus{}
	parent.Status = previous
	if err := store.Status().Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	stale := &agents.SandboxServing{}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), stale); err != nil {
		t.Fatal(err)
	}
	staleBefore := stale.DeepCopy()
	parent.Spec.Ports = []int32{8081}
	if err := store.Update(context.Background(), parent); err != nil {
		t.Fatal(err)
	}
	r := NewReconciler(store)
	err := r.updateStatus(context.Background(), stale, desired)
	if !apierrors.IsConflict(err) {
		t.Fatalf("stale status RV was not rejected as Conflict: %v", err)
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	before := previous.DeepCopy()
	desiredBefore := desired.DeepCopy()
	fresh := &agents.SandboxServing{}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), fresh); err != nil {
		t.Fatal(err)
	}
	freshBefore := fresh.DeepCopy()
	if err := r.updateStatus(context.Background(), fresh, desired); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stale, staleBefore) || !reflect.DeepEqual(fresh, freshBefore) {
		t.Fatal("status update mutated cached snapshots")
	}
	if !maps.Equal(previous.ReplicasByRevision, before.ReplicasByRevision) || previous.CurrentRevision != before.CurrentRevision || desired.CurrentRevision != desiredBefore.CurrentRevision {
		t.Fatal("status update mutated its input snapshots")
	}
	if err := store.Get(context.Background(), client.ObjectKeyFromObject(parent), parent); err != nil {
		t.Fatal(err)
	}
	if parent.Status.CurrentRevision != "" || parent.Status.UpdateRevision != "" || len(parent.Status.ReplicasByRevision) != 0 {
		t.Fatalf("omitted status fields survived: %+v", parent.Status)
	}
}
