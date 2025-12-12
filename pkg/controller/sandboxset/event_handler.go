package sandboxset

import (
	"context"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/expectations"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

type SandboxEventHandler struct{}

func (e *SandboxEventHandler) Create(_ context.Context, evt event.TypedCreateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if req, ok := getSandboxSetController(evt.Object); ok {
		scaleUpExpectation.ObserveScale(req.String(), expectations.Create, evt.Object.GetName())
		w.Add(req)
	}
}

func (e *SandboxEventHandler) Update(ctx context.Context, evt event.TypedUpdateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
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
	oldState, _ := stateutils.GetSandboxState(oldSbx)
	newState, _ := stateutils.GetSandboxState(newSbx)
	if oldState != newState {
		w.Add(req)
	}
	if oldState == agentsv1alpha1.SandboxStateCreating && newState == agentsv1alpha1.SandboxStateAvailable {
		cond := utils.GetSandboxCondition(&newSbx.Status, string(agentsv1alpha1.SandboxConditionReady))
		var afterReady, readyCost, totalCost time.Duration
		now := time.Now()
		if cond != nil {
			afterReady = now.Sub(cond.LastTransitionTime.Time)
			readyCost = cond.LastTransitionTime.Sub(newSbx.CreationTimestamp.Time)
			totalCost = now.Sub(newSbx.CreationTimestamp.Time)
		}
		logf.FromContext(ctx).Info("sandbox available", "sandbox", klog.KObj(newSbx), "now", now,
			"readyCost", readyCost, "watchedAfterReady", afterReady, "totalCost", totalCost)
	}
}

func (e *SandboxEventHandler) Delete(_ context.Context, evt event.TypedDeleteEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if req, ok := getSandboxSetController(evt.Object); ok {
		scaleDownExpectation.ObserveScale(req.String(), expectations.Delete, evt.Object.GetName())
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
	}

	groupVersion, err := schema.ParseGroupVersion(controller.APIVersion)
	if err != nil {
		return reconcile.Request{}, false
	}
	if controller.Kind != agentsv1alpha1.SandboxSetControllerKind.Kind || groupVersion.Group != agentsv1alpha1.SandboxSetControllerKind.GroupVersion().Group {
		return reconcile.Request{}, false
	}

	req := reconcile.Request{
		NamespacedName: types.NamespacedName{
			Namespace: obj.GetNamespace(),
			Name:      controller.Name,
		},
	}
	return req, true
}
