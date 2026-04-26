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

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// SandboxEventHandler enqueues the associated SandboxUpdateOps when a Sandbox changes.
type SandboxEventHandler struct{}

func (e *SandboxEventHandler) Create(_ context.Context, _ event.TypedCreateEvent[client.Object], _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	// No action on create events
}

func (e *SandboxEventHandler) Update(_ context.Context, evt event.TypedUpdateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	obj := evt.ObjectNew
	old := evt.ObjectOld
	if obj == nil || old == nil {
		return
	}
	opsName := obj.GetLabels()[agentsv1alpha1.LabelSandboxUpdateOps]
	if opsName == "" {
		return
	}
	if old.GetDeletionTimestamp().IsZero() && !obj.GetDeletionTimestamp().IsZero() {
		ResourceVersionExpectations.Delete(obj)
	}
	w.Add(reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      opsName,
		},
	})
}

func (e *SandboxEventHandler) Delete(_ context.Context, evt event.TypedDeleteEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	obj := evt.Object
	if obj == nil {
		return
	}
	opsName := obj.GetLabels()[agentsv1alpha1.LabelSandboxUpdateOps]
	if opsName == "" {
		return
	}
	ResourceVersionExpectations.Delete(obj)
	w.Add(reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      opsName,
		},
	})
}

func (e *SandboxEventHandler) Generic(_ context.Context, _ event.TypedGenericEvent[client.Object], _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}
