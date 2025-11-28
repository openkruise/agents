package sandbox

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
	if !NeedsBypassSandbox(pod) {
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
	if !NeedsBypassSandbox(newPod) {
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

// SandboxPodEventHandler 监控由 Sandbox 控制器创建的 Pod
type SandboxPodEventHandler struct{}

func (e *SandboxPodEventHandler) Create(_ context.Context, evt event.TypedCreateEvent[client.Object], w workqueue.TypedRateLimitingInterface[reconcile.Request]) {
	if evt.Object.GetAnnotations()[utils.PodAnnotationEnablePaused] != "" {
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

// NeedsBypassSandbox 判断 pod 是否启用旁路 sandbox 能力，即需要控制器根据 pod 上的 pause 协议创建、修改相应的 Sandbox 资源
func NeedsBypassSandbox(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	// 使用旁路 Sandbox 必须要启用 pause 功能
	if pod.Annotations[utils.PodAnnotationEnablePaused] != utils.True {
		return false
	}
	// 使用旁路 Sandbox 必须要带有 webhook 过滤标签
	if pod.Labels[utils.PodLabelEnableAutoCreateSandbox] != utils.True {
		return false
	}
	pausedCond := utils.GetPodCondition(&pod.Status, utils.PodConditionContainersPaused)
	resumeCond := utils.GetPodCondition(&pod.Status, utils.PodConditionContainersResumed)
	if pausedCond != nil || resumeCond != nil {
		// pause / resume 流程中的 Pod 跳过状态判断
		return true
	} else {
		// 非 pause / resume 流程中的 Pod 需要判断状态
		return corev1.PodSucceeded != pod.Status.Phase && corev1.PodFailed != pod.Status.Phase
	}
}
