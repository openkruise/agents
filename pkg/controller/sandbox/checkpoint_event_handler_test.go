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

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

// newOwnedCheckpoint builds a Checkpoint owned by the named Sandbox, matching
// what createCheckpoint stamps via metav1.NewControllerRef.
func newOwnedCheckpoint(name, namespace, ownerName string) *agentsv1alpha1.Checkpoint {
	return &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: sandboxControllerKind.GroupVersion().String(),
				Kind:       sandboxControllerKind.Kind,
				Name:       ownerName,
				UID:        types.UID("sbx-uid"),
				Controller: ptr.To(true),
			}},
		},
	}
}

// newForeignOwnedCheckpoint builds a Checkpoint whose controller owner is some
// other kind, which the handler must ignore.
func newForeignOwnedCheckpoint(name, namespace string) *agentsv1alpha1.Checkpoint {
	cp := newOwnedCheckpoint(name, namespace, "not-a-sandbox")
	cp.OwnerReferences[0].Kind = "SandboxSet"
	return cp
}

func TestCheckpointOwnerKey(t *testing.T) {
	owned := newOwnedCheckpoint("cp-1", "ns", "sbx-1")

	noOwner := owned.DeepCopy()
	noOwner.OwnerReferences = nil

	nonController := owned.DeepCopy()
	nonController.OwnerReferences[0].Controller = ptr.To(false)

	wrongGroup := owned.DeepCopy()
	wrongGroup.OwnerReferences[0].APIVersion = "apps/v1"

	tests := []struct {
		name    string
		obj     client.Object
		wantKey string
		wantReq types.NamespacedName
	}{
		{
			name:    "sandbox controller owner resolves to the sandbox key",
			obj:     owned,
			wantKey: "ns/sbx-1",
			wantReq: types.NamespacedName{Namespace: "ns", Name: "sbx-1"},
		},
		{
			name: "no owner reference is ignored",
			obj:  noOwner,
		},
		{
			// A non-controller ownerReference is invisible to
			// GetControllerOfNoCopy, so this must not be mistaken for an owner.
			name: "owner reference that is not the controller is ignored",
			obj:  nonController,
		},
		{
			name: "foreign kind reusing Checkpoint is ignored",
			obj:  newForeignOwnedCheckpoint("cp-1", "ns"),
		},
		{
			name: "owner in another API group is ignored",
			obj:  wrongGroup,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, req := checkpointOwnerKey(tt.obj)
			assert.Equal(t, tt.wantKey, key)
			assert.Equal(t, tt.wantReq, req.NamespacedName)
		})
	}
}

func TestCheckpointEventHandler_CreateObservesExpectation(t *testing.T) {
	cp := newOwnedCheckpoint("cp-1", "ns", "sbx-1")
	key := "ns/sbx-1"
	core.ScaleExpectation.DeleteExpectations(key)
	t.Cleanup(func() { core.ScaleExpectation.DeleteExpectations(key) })

	core.ScaleExpectation.ExpectScale(key, expectations.Create, cp.Name)
	satisfied, _, _ := core.ScaleExpectation.SatisfiedExpectations(key)
	assert.False(t, satisfied, "precondition: the create expectation is outstanding")

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	handler := &CheckpointEventHandler{}
	handler.Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: cp}, q)

	satisfied, _, unmet := core.ScaleExpectation.SatisfiedExpectations(key)
	assert.True(t, satisfied, "create event must settle the expectation, unmet: %v", unmet)
	assert.Equal(t, 1, q.Len(), "the owning sandbox must be enqueued")
	item, _ := q.Get()
	assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "sbx-1"}, item.NamespacedName)
	q.Done(item)
}

func TestCheckpointEventHandler_DeleteObservesExpectation(t *testing.T) {
	cp := newOwnedCheckpoint("cp-1", "ns", "sbx-1")
	key := "ns/sbx-1"
	core.ScaleExpectation.DeleteExpectations(key)
	t.Cleanup(func() { core.ScaleExpectation.DeleteExpectations(key) })

	core.ScaleExpectation.ExpectScale(key, expectations.Delete, cp.Name)
	satisfied, _, _ := core.ScaleExpectation.SatisfiedExpectations(key)
	assert.False(t, satisfied, "precondition: the delete expectation is outstanding")

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	handler := &CheckpointEventHandler{}
	handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: cp}, q)

	satisfied, _, unmet := core.ScaleExpectation.SatisfiedExpectations(key)
	assert.True(t, satisfied, "delete event must settle the expectation, unmet: %v", unmet)
	assert.Equal(t, 1, q.Len(), "the owning sandbox must be enqueued")
	item, _ := q.Get()
	assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "sbx-1"}, item.NamespacedName)
	q.Done(item)
}

// TestCheckpointEventHandler_ForeignOwnerLeavesExpectationsAlone pins that a
// Checkpoint owned by another kind neither settles an expectation nor enqueues.
// The key it would otherwise compute collides with a real Sandbox key, so
// settling on it would clear an unrelated Sandbox's expectation.
func TestCheckpointEventHandler_ForeignOwnerLeavesExpectationsAlone(t *testing.T) {
	cp := newForeignOwnedCheckpoint("cp-1", "ns")
	key := "ns/not-a-sandbox"
	core.ScaleExpectation.DeleteExpectations(key)
	t.Cleanup(func() { core.ScaleExpectation.DeleteExpectations(key) })

	core.ScaleExpectation.ExpectScale(key, expectations.Delete, cp.Name)

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	handler := &CheckpointEventHandler{}
	handler.Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: cp}, q)
	handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: cp}, q)

	satisfied, _, _ := core.ScaleExpectation.SatisfiedExpectations(key)
	assert.False(t, satisfied, "a foreign owner must not settle another key's expectation")
	assert.Equal(t, 0, q.Len(), "a foreign owner must not enqueue")
}

func TestCheckpointEventHandler_UpdateEnqueuesOwner(t *testing.T) {
	tests := []struct {
		name          string
		obj           client.Object
		expectEnqueue bool
	}{
		{
			name:          "owned checkpoint enqueues its sandbox",
			obj:           newOwnedCheckpoint("cp-1", "ns", "sbx-1"),
			expectEnqueue: true,
		},
		{
			name:          "foreign owner is ignored",
			obj:           newForeignOwnedCheckpoint("cp-1", "ns"),
			expectEnqueue: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			defer q.ShutDown()

			handler := &CheckpointEventHandler{}
			handler.Update(context.Background(), event.TypedUpdateEvent[client.Object]{
				ObjectOld: tt.obj,
				ObjectNew: tt.obj,
			}, q)

			assert.Equal(t, tt.expectEnqueue, q.Len() > 0)
			if tt.expectEnqueue {
				item, _ := q.Get()
				assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "sbx-1"}, item.NamespacedName)
				q.Done(item)
			}
		})
	}
}

// TestCheckpointEventHandler_GenericIsInert pins that Generic does nothing, so
// a synthetic event cannot settle an expectation or enqueue a reconcile.
func TestCheckpointEventHandler_GenericIsInert(t *testing.T) {
	cp := newOwnedCheckpoint("cp-1", "ns", "sbx-1")
	key := "ns/sbx-1"
	core.ScaleExpectation.DeleteExpectations(key)
	t.Cleanup(func() { core.ScaleExpectation.DeleteExpectations(key) })

	core.ScaleExpectation.ExpectScale(key, expectations.Delete, cp.Name)

	q := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer q.ShutDown()

	handler := &CheckpointEventHandler{}
	handler.Generic(context.Background(), event.TypedGenericEvent[client.Object]{Object: cp}, q)

	satisfied, _, _ := core.ScaleExpectation.SatisfiedExpectations(key)
	assert.False(t, satisfied, "Generic must not settle expectations")
	assert.Equal(t, 0, q.Len(), "Generic must not enqueue")
}
