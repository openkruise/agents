package sandboxset

import (
	"context"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/expectations"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type SandboxEventHandler struct{}

func (e *SandboxEventHandler) Create(_ context.Context, evt event.TypedCreateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if req, ok := getSandboxSetController(evt.Object); ok {
		scaleExpectation.ObserveScale(req.String(), expectations.Create, evt.Object.GetName())
		w.Add(req)
	}
}

func (e *SandboxEventHandler) Update(_ context.Context, evt event.TypedUpdateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if evt.ObjectOld == nil || evt.ObjectNew == nil {
		return
	}
	req, ok := getSandboxSetController(evt.ObjectOld)
	if !ok {
		return
	}
	oldSbx, ok := evt.ObjectOld.(*agentsv1alpha1.Sandbox)
	if !ok {
		return
	}
	newSbx, ok := evt.ObjectNew.(*agentsv1alpha1.Sandbox)
	if !ok {
		return
	}
	_, oldReason := findSandboxGroup(oldSbx)
	newGroup, newReason := findSandboxGroup(newSbx)
	if oldReason != newReason {
		w.Add(req)
	} else if newGroup == GroupCreating {
		// Only the state transition from creating to available is performed through reconciliation, so here we
		// additionally allow Ready Sandboxes that meet the conditions.
		if checkSandboxReady(newSbx) {
			w.Add(req)
		}
	}
}

func (e *SandboxEventHandler) Delete(_ context.Context, evt event.TypedDeleteEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if req, ok := getSandboxSetController(evt.Object); ok {
		w.Add(req)
	}
}

func (e *SandboxEventHandler) Generic(context.Context, event.TypedGenericEvent[client.Object], workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func getSandboxSetController(obj metav1.Object) (reconcile.Request, bool) {
	if obj == nil {
		return reconcile.Request{}, false
	}
	controller := metav1.GetControllerOf(obj)
	if controller == nil {
		return reconcile.Request{}, false
	} else {
		groupVersion, err := schema.ParseGroupVersion(controller.APIVersion)
		if err != nil {
			return reconcile.Request{}, false
		}
		if controller.Kind != sandboxSetControllerKind.Kind || groupVersion.Group != sandboxSetControllerKind.GroupVersion().Group {
			return reconcile.Request{}, false
		}
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      controller.Name,
		},
	}
	return req, true
}
