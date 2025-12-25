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

package sandbox

import (
	"context"

	"github.com/openkruise/agents/pkg/utils"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// SandboxPodEventHandler watches Pods created by the Sandbox controller.
type SandboxPodEventHandler struct{}

func (e *SandboxPodEventHandler) Create(_ context.Context, evt event.TypedCreateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if evt.Object.GetAnnotations()[utils.PodAnnotationCreatedBy] != "" {
		w.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(evt.Object)})
	}
}

func (e *SandboxPodEventHandler) Update(_ context.Context, evt event.TypedUpdateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if evt.ObjectNew.GetAnnotations()[utils.PodAnnotationCreatedBy] != "" {
		w.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(evt.ObjectNew)})
	}
}

func (e *SandboxPodEventHandler) Delete(_ context.Context, evt event.TypedDeleteEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if evt.Object.GetAnnotations()[utils.PodAnnotationCreatedBy] != "" {
		w.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(evt.Object)})
	}
}

func (e *SandboxPodEventHandler) Generic(context.Context, event.TypedGenericEvent[client.Object], workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}
