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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestSandboxEventHandler_Update_WithLabel(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxUpdateOps: "my-ops",
			},
		},
	}

	evt := event.TypedUpdateEvent[client.Object]{
		ObjectOld: sbx,
		ObjectNew: sbx,
	}

	handler.Update(context.Background(), evt, queue)

	// Should have enqueued one item
	assert.Equal(t, 1, queue.Len())
	item, shutdown := queue.Get()
	assert.False(t, shutdown)
	assert.Equal(t, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "my-ops",
		},
	}, item)
}

func TestSandboxEventHandler_Update_WithoutLabel(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{},
		},
	}

	evt := event.TypedUpdateEvent[client.Object]{
		ObjectOld: sbx,
		ObjectNew: sbx,
	}

	handler.Update(context.Background(), evt, queue)

	// Should not have enqueued anything
	assert.Equal(t, 0, queue.Len())
}

func TestSandboxEventHandler_Update_NilObjectNew(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	evt := event.TypedUpdateEvent[client.Object]{
		ObjectOld: &agentsv1alpha1.Sandbox{},
		ObjectNew: nil,
	}

	handler.Update(context.Background(), evt, queue)

	assert.Equal(t, 0, queue.Len())
}

func TestSandboxEventHandler_Create_NoAction(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxUpdateOps: "my-ops",
			},
		},
	}

	handler.Create(context.Background(), event.TypedCreateEvent[client.Object]{Object: sbx}, queue)

	// Wait a tiny bit and check nothing was enqueued
	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 0, queue.Len())
}

func TestSandboxEventHandler_Delete_WithLabel(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxUpdateOps: "my-ops",
			},
		},
	}

	handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: sbx}, queue)

	// Delete now enqueues a reconcile.Request for the associated ops
	assert.Equal(t, 1, queue.Len())
	item, shutdown := queue.Get()
	assert.False(t, shutdown)
	assert.Equal(t, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "my-ops",
		},
	}, item)
}

func TestSandboxEventHandler_Delete_WithoutLabel(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels:    map[string]string{},
		},
	}

	handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: sbx}, queue)

	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 0, queue.Len())
}

func TestSandboxEventHandler_Delete_NilObject(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	handler.Delete(context.Background(), event.TypedDeleteEvent[client.Object]{Object: nil}, queue)

	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 0, queue.Len())
}

func TestSandboxEventHandler_Update_DeletionTimestamp(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	now := metav1.Now()
	oldSbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxUpdateOps: "my-ops",
			},
		},
	}
	newSbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "sbx-1",
			Namespace:         "default",
			DeletionTimestamp: &now,
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxUpdateOps: "my-ops",
			},
		},
	}

	evt := event.TypedUpdateEvent[client.Object]{
		ObjectOld: oldSbx,
		ObjectNew: newSbx,
	}

	handler.Update(context.Background(), evt, queue)

	assert.Equal(t, 1, queue.Len())
	item, shutdown := queue.Get()
	assert.False(t, shutdown)
	assert.Equal(t, reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: "default",
			Name:      "my-ops",
		},
	}, item)
}

func TestSandboxEventHandler_Update_NilObjectOld(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	evt := event.TypedUpdateEvent[client.Object]{
		ObjectOld: nil,
		ObjectNew: &agentsv1alpha1.Sandbox{},
	}

	handler.Update(context.Background(), evt, queue)

	assert.Equal(t, 0, queue.Len())
}

func TestSandboxEventHandler_Generic_NoAction(t *testing.T) {
	handler := &SandboxEventHandler{}
	queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
	defer queue.ShutDown()

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-1",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxUpdateOps: "my-ops",
			},
		},
	}

	handler.Generic(context.Background(), event.TypedGenericEvent[client.Object]{Object: sbx}, queue)

	time.Sleep(10 * time.Millisecond)
	assert.Equal(t, 0, queue.Len())
}
