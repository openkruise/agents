package bypass_sandbox

import (
	"context"

	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// BypassPodEventHandler 监控 Pod 协议深休眠语法糖的更新事件
type BypassPodEventHandler struct{}

func (e *BypassPodEventHandler) Create(_ context.Context, evt event.TypedCreateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	pod, ok := evt.Object.(*corev1.Pod)
	if !ok {
		return
	}
	if !utils.NeedsBypassSandbox(pod) {
		return
	}
	w.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(evt.Object)})
}

func (e *BypassPodEventHandler) Update(_ context.Context, evt event.TypedUpdateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	var newPod, oldPod *corev1.Pod
	newPod, ok := evt.ObjectNew.(*corev1.Pod)
	if !ok {
		return
	}
	oldPod, ok = evt.ObjectOld.(*corev1.Pod)
	if !ok {
		return
	}
	if !utils.NeedsBypassSandbox(newPod) {
		return
	}
	if newPod.Annotations[utils.PodAnnotationSandboxPause] == oldPod.Annotations[utils.PodAnnotationSandboxPause] {
		// 只处理 sandbox-paused 的更新
		return
	}
	w.Add(reconcile.Request{NamespacedName: client.ObjectKeyFromObject(evt.ObjectNew)})
}

func (e *BypassPodEventHandler) Delete(_ context.Context, _ event.TypedDeleteEvent[client.Object], _ workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}

func (e *BypassPodEventHandler) Generic(context.Context, event.TypedGenericEvent[client.Object], workqueue.TypedRateLimitingInterface[reconcile.Request]) {
}
