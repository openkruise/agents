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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/openkruise/agents/pkg/controller/sandbox/core"
	"github.com/openkruise/agents/pkg/utils/expectations"
)

// CheckpointEventHandler watches Checkpoint CRs owned by Sandbox and
// observes ScaleExpectation on create/delete events.
type CheckpointEventHandler struct{}

func (e *CheckpointEventHandler) Create(_ context.Context, evt event.TypedCreateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	key, req := checkpointOwnerKey(evt.Object)
	if key == "" {
		return
	}
	core.ScaleExpectation.ObserveScale(key, expectations.Create, evt.Object.GetName())
	w.Add(req)
}

func (e *CheckpointEventHandler) Update(_ context.Context, evt event.TypedUpdateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	_, req := checkpointOwnerKey(evt.ObjectNew)
	if req.Name == "" {
		return
	}
	w.Add(req)
}

func (e *CheckpointEventHandler) Delete(_ context.Context, evt event.TypedDeleteEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	key, req := checkpointOwnerKey(evt.Object)
	if key == "" {
		return
	}
	core.ScaleExpectation.ObserveScale(key, expectations.Delete, evt.Object.GetName())
	w.Add(req)
}

func (e *CheckpointEventHandler) Generic(context.Context, event.TypedGenericEvent[client.Object], workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

// checkpointOwnerKey extracts the owning Sandbox's controller key and reconcile request
// from the Checkpoint's controller ownerReference. Returns empty values when the
// controller owner is missing or refers to a kind other than Sandbox (e.g. a
// foreign CRD reusing the Checkpoint type).
func checkpointOwnerKey(obj client.Object) (string, reconcile.Request) {
	owner := metav1.GetControllerOfNoCopy(obj)
	if owner == nil {
		return "", reconcile.Request{}
	}
	if owner.APIVersion != sandboxControllerKind.GroupVersion().String() || owner.Kind != sandboxControllerKind.Kind {
		return "", reconcile.Request{}
	}
	nn := types.NamespacedName{Namespace: obj.GetNamespace(), Name: owner.Name}
	return nn.String(), reconcile.Request{NamespacedName: nn}
}
