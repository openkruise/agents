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
