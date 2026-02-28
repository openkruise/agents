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
	"reflect"

	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
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
	oldObj := evt.ObjectOld.(*corev1.Pod)
	newObj := evt.ObjectNew.(*corev1.Pod)
	if newObj.Annotations[utils.PodAnnotationCreatedBy] != "" && isActivePodUpdate(oldObj, newObj) {
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

func isActivePodUpdate(oldObj, newObj *corev1.Pod) bool {
	if oldObj.Status.Phase != newObj.Status.Phase || oldObj.Status.PodIP != newObj.Status.PodIP {
		return true
	}
	rCond1 := utils.GetPodCondition(&oldObj.Status, corev1.PodReady)
	rCond2 := utils.GetPodCondition(&newObj.Status, corev1.PodReady)
	if !isPodConditionEqual(rCond1, rCond2) {
		return true
	}
	pCond1 := utils.GetPodCondition(&oldObj.Status, utils.PodConditionContainersPaused)
	pCond2 := utils.GetPodCondition(&newObj.Status, utils.PodConditionContainersPaused)
	if !isPodConditionEqual(pCond1, pCond2) {
		return true
	}
	cCond1 := utils.GetPodCondition(&oldObj.Status, utils.PodConditionContainersResumed)
	cCond2 := utils.GetPodCondition(&newObj.Status, utils.PodConditionContainersResumed)
	if !isPodConditionEqual(cCond1, cCond2) {
		return true
	}
	// for in-place upgrade scenarios
	if !reflect.DeepEqual(oldObj.Status.ContainerStatuses, newObj.Status.ContainerStatuses) {
		return true
	}
	return false
}

// isPodConditionEqual compares two PodConditions by Status, Reason, and Message fields.
func isPodConditionEqual(a, b *corev1.PodCondition) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Status == b.Status && a.Reason == b.Reason && a.Message == b.Message
}
