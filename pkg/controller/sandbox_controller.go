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

package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

var (
	sandboxControllerKind = agentsv1alpha1.GroupVersion.WithKind("Sandbox")
)

// SandboxReconciler reconciles a Sandbox object
type SandboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=agents.kruise.io,resources=sandboxes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core,resources=events,verbs=create

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Sandbox object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *SandboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// Fetch the sandbox instance
	box := &agentsv1alpha1.Sandbox{}
	err := r.Get(context.TODO(), req.NamespacedName, box)
	if err != nil {
		if errors.IsNotFound(err) {
			// Object not found, return.  Created objects are automatically garbage collected.
			// For additional cleanup logic use finalizers.
			return reconcile.Result{}, nil
		}
		// Error reading the object - requeue the request.
		return reconcile.Result{}, err
	} else if box.Status.Phase == agentsv1alpha1.SandboxFailed || box.Status.Phase == agentsv1alpha1.SandboxSucceeded {
		return reconcile.Result{}, nil
	}
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

	// 添加 finalizer
	if box.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(box, utils.SandboxFinalizer) {
		err = utils.UpdateFinalizer(r.Client, box, utils.AddFinalizerOpType, utils.SandboxFinalizer)
		if err != nil {
			logger.Error(err, "update sandbox finalizer failed")
			return reconcile.Result{}, err
		}
		logger.Info("add sandbox finalizer success")
	}

	logger.Info("Began to process Sandbox for reconcile")
	newStatus := box.Status.DeepCopy()
	newStatus.ObservedGeneration = box.Generation
	if newStatus.Phase == "" {
		newStatus.Phase = agentsv1alpha1.SandboxPending
	}

	// fetch pod
	pod := &corev1.Pod{}
	err = r.Get(ctx, client.ObjectKey{Namespace: box.Namespace, Name: box.Name}, pod)
	if err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "Get Pod failed")
		return reconcile.Result{}, err
	} else if errors.IsNotFound(err) {
		pod = nil
	} else if !pod.DeletionTimestamp.IsZero() {
		// Pod 正在删除过程中，等待 Pod 删除完成
		return reconcile.Result{RequeueAfter: time.Second * 3}, nil
	} else if pod.Status.Phase == corev1.PodSucceeded {
		newStatus.Phase = agentsv1alpha1.SandboxSucceeded
		return ctrl.Result{}, r.updateSandboxStatus(ctx, *newStatus, box)
	} else if pod.Status.Phase == corev1.PodFailed &&
		box.Status.Phase != agentsv1alpha1.SandboxPaused &&
		!NeedsBypassSandbox(pod) {
		// paused过程中，Pod会变为 Failed 状态，需要忽略。
		// NeedsBypassSandbox 用于忽略旁路 Sandbox 场景下 Pod 由于深休眠提前进入 Failed 的情况。
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		return ctrl.Result{}, r.updateSandboxStatus(ctx, *newStatus, box)
	}

	// 当 sandbox 被删除，并且没有处于 pausing 或 resuming 过程中，进入到 Terminating 阶段
	// 主要是考虑到 pausing 或 resuming 过程中Corner Case比较多，所以 pause 或 resume 完成再进入到 terminating 阶段
	if !box.DeletionTimestamp.IsZero() && !sandboxInPausingOrResuming(newStatus) {
		newStatus.Phase = agentsv1alpha1.SandboxTerminating
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		if cond != nil && cond.Status == metav1.ConditionTrue {
			cond.Status = metav1.ConditionFalse
			cond.LastTransitionTime = metav1.Now()
			utils.SetSandboxCondition(newStatus, *cond)
		}
		// 如果是 paused ，首先将 sandbox 设置为 Paused 状态
	} else if box.Spec.Paused && box.Status.Phase == agentsv1alpha1.SandboxRunning {
		newStatus.Phase = agentsv1alpha1.SandboxPaused
		// 进入到resume阶段
	} else if !box.Spec.Paused && newStatus.Phase == agentsv1alpha1.SandboxPaused {
		// delete paused condition
		utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
		newStatus.Phase = agentsv1alpha1.SandboxResuming
		cond := metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionResumed),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxResumeReasonCreatePod,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, cond)
	}

	if pod != nil {
		newStatus.PodInfo = agentsv1alpha1.PodInfo{
			PodIP:    pod.Status.PodIP,
			NodeName: pod.Spec.NodeName,
		}
		newStatus.PodInfo.Annotations = map[string]string{
			utils.PodAnnotationAcsInstanceId: pod.Annotations[utils.PodAnnotationAcsInstanceId],
			utils.PodAnnotationSourcePodUID:  string(pod.UID),
		}
	}

	switch newStatus.Phase {
	case agentsv1alpha1.SandboxPending:
		err = r.handlerPhasePending(ctx, pod, box, newStatus)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{}, r.updateSandboxStatus(ctx, *newStatus, box)

	case agentsv1alpha1.SandboxRunning:
		err = r.handlerPhaseRunning(ctx, pod, newStatus)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{}, r.updateSandboxStatus(ctx, *newStatus, box)

	case agentsv1alpha1.SandboxPaused:
		err = r.handlerPhasePaused(ctx, pod, box, newStatus)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{}, r.updateSandboxStatus(ctx, *newStatus, box)

	case agentsv1alpha1.SandboxResuming:
		err = r.handlerPhaseResume(ctx, pod, box, newStatus)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{}, r.updateSandboxStatus(ctx, *newStatus, box)

	case agentsv1alpha1.SandboxTerminating:
		err = r.handlerPhaseTerminating(ctx, pod, box, newStatus)
		if err != nil {
			return reconcile.Result{}, err
		}
		return ctrl.Result{}, r.updateSandboxStatus(ctx, *newStatus, box)
	}
	logger.Info("sandbox status phase is invalid", "phase", box.Status.Phase)
	return ctrl.Result{}, nil
}

func (r *SandboxReconciler) handlerPhasePending(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error {
	// 如果 Pod 不存在，首先需要创建 Pod
	if pod == nil {
		return r.createPod(ctx, box, newStatus)
	} else if pod.Status.Phase == corev1.PodRunning || NeedsBypassSandbox(pod) {
		// Sandbox 托管 Pod 进入到 Running 才会使 Sandbox 进入 Running 阶段
		// 旁路 Sandbox Pod 由于状态不可预测，将会尽快使 Sandbox 进入 Running 阶段
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		if cond == nil {
			cond = &metav1.Condition{
				Type:               string(agentsv1alpha1.SandboxConditionReady),
				Status:             metav1.ConditionFalse,
				LastTransitionTime: metav1.Now(),
				Reason:             agentsv1alpha1.SandboxReadyReasonPodReady,
			}
		}
		if pCond != nil && string(pCond.Status) != string(cond.Status) {
			cond.Status = metav1.ConditionStatus(pCond.Status)
			cond.LastTransitionTime = pCond.LastTransitionTime
		}
		utils.SetSandboxCondition(newStatus, *cond)
	}
	return nil
}

func (r *SandboxReconciler) createPod(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	if box.Annotations[utils.SandboxAnnotationDisablePodCreation] == utils.True {
		logger.Info("pod creation disabled")
		return nil
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       box.Namespace,
			Name:            box.Name,
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(box, sandboxControllerKind)},
			Labels:          box.Spec.Template.Labels,
			Annotations:     box.Spec.Template.Annotations,
		},
		Spec: box.Spec.Template.Spec,
	}
	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations[utils.PodAnnotationEnablePaused] = utils.True
	pod.Annotations[utils.PodAnnotationCreatedBy] = utils.CreatedBySandbox
	// 当sandbox在resume状态时，需要带上一些额外的annotations
	if newStatus.Phase == agentsv1alpha1.SandboxResuming {
		utils.InjectResumedPod(box, pod)
		pod.Annotations[utils.PodAnnotationRecoverFromInstanceID] = box.Status.PodInfo.Annotations[utils.PodAnnotationAcsInstanceId]
	}
	err := r.Create(ctx, pod)
	if err != nil && errors.IsAlreadyExists(err) {
		logger.Error(err, "create pod failed", "Pod", klog.KObj(pod))
		return err
	}
	logger.Info("Create pod success", "Pod", klog.KObj(pod))
	return nil
}

func (r *SandboxReconciler) handlerPhaseRunning(ctx context.Context, pod *corev1.Pod, newStatus *agentsv1alpha1.SandboxStatus) error {
	if pod == nil {
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		newStatus.Message = "Sandbox Pod Not Found"
	} else {
		pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
		cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		if pCond != nil && string(pCond.Status) != string(cond.Status) {
			cond.Status = metav1.ConditionStatus(pCond.Status)
			cond.LastTransitionTime = pCond.LastTransitionTime
		}
		utils.SetSandboxCondition(newStatus, *cond)
		if pod.Annotations[utils.PodAnnotationRecreating] != "" {
			return r.Patch(ctx, pod, client.RawPatch(types.MergePatchType,
				[]byte(fmt.Sprintf(`{"metadata":{"annotations":{"%s":null}}}`, utils.PodAnnotationRecreating))))
		}
	}
	return nil
}

func (r *SandboxReconciler) handlerPhasePaused(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box), "pod", klog.KObj(pod))
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if cond == nil {
		cond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionPaused),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxPausedReasonSetPause,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *cond)
	} else if cond.Status == metav1.ConditionTrue {
		return nil
	}

	// paused 阶段将 ready 设置为 false
	if rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady)); rCond.Status == metav1.ConditionTrue {
		rCond.Status = metav1.ConditionFalse
		rCond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *rCond)
	}

	var err error
	switch cond.Reason {
	case agentsv1alpha1.SandboxPausedReasonSetPause:
		if pod == nil {
			newStatus.Phase = agentsv1alpha1.SandboxFailed
			newStatus.Message = "Sandbox Pod Not Found"
			return nil
		}
		// patch pod paused
		if value, ok := pod.Annotations[utils.PodAnnotationSandboxPause]; !ok || value != utils.True {
			clone := pod.DeepCopy()
			clone.Annotations[utils.PodAnnotationSandboxPause] = utils.True
			clone.Annotations[utils.PodAnnotationReserveInstance] = utils.True
			patchBody := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
			err = r.Patch(ctx, clone, patchBody)
			if err != nil {
				logger.Error(err, "Patch Pod Annotation Failed",
					"at", agentsv1alpha1.SandboxPausedReasonSetPause)
				return err
			}
			logger.Info("Patch pod annotations success",
				"at", agentsv1alpha1.SandboxPausedReasonSetPause)
			return nil
		}

		podCond := utils.GetPodCondition(&pod.Status, utils.PodConditionContainersPaused)
		if podCond == nil || podCond.Status == corev1.ConditionFalse {
		} else {
			cond.Reason = agentsv1alpha1.SandboxPausedReasonDeletePod
			utils.SetSandboxCondition(newStatus, *cond)
		}

	case agentsv1alpha1.SandboxPausedReasonDeletePod:
		// delete pod
		var second int64 = 30
		if box.Annotations[utils.SandboxAnnotationDisablePodDeletion] == utils.True {
			// 关闭 Pod 生命周期管理则跳过 Pod 删除阶段
			cond.Status = metav1.ConditionTrue
			utils.SetSandboxCondition(newStatus, *cond)
		} else if pod != nil && pod.DeletionTimestamp.IsZero() {
			// 打开 Pod 生命周期管理，先删除 Pod
			err = r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: &second})
			if err != nil {
				logger.Error(err, "Delete pod failed")
				return err
			}
			logger.Info("Delete pod success")
		} else if pod == nil {
			// Pod 删除完成，休眠完成
			cond.Status = metav1.ConditionTrue
			utils.SetSandboxCondition(newStatus, *cond)
		} else {
			// Pod 删除未完成，等待
			logger.Info("Sandbox wait pod paused")
		}
	}
	return nil
}

func (r *SandboxReconciler) handlerPhaseResume(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box), "pod", klog.KObj(pod))

	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
	switch cond.Reason {
	case agentsv1alpha1.SandboxResumeReasonCreatePod:
		// 首先创建 Pod
		if pod == nil {
			return r.createPod(ctx, box, newStatus)
		}
		// 需要pod condition paused=true之后，才能 resume Pod
		podCond := utils.GetPodCondition(&pod.Status, utils.PodConditionContainersPaused)
		if podCond == nil {
			// 对于非 sandbox 创建的 Pod，需要 patch 外部创建的 annotate
			if box.Annotations[utils.SandboxAnnotationDisablePodCreation] == utils.True {
				// 现在的 VK 状态同步存在问题：
				// 1. VK 需要 Pod 的一次 Update 事件来触发同步管控 Condition 的逻辑（ContainersPaused）并设置 Pod 休眠状态
				// 2. Pod 创建后较长时间内 Update 会被忽略（原因排查中）
				// 因而，需要在创建后延迟足够时间更新 Pod，触发后续流程。
				// 这段代码只在通过 Pod 协议的语法糖中生效。
				if pod.Annotations[utils.PodAnnotationCreatedBy] == "" {
					// 给外部创建的 pod 加上标记，确保能够触发 VK 的更新逻辑刷新 pause/resume 的 condition
					err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
						clone := &corev1.Pod{}
						err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), clone)
						if err != nil {
							return err
						}
						clone.Annotations[utils.PodAnnotationCreatedBy] = utils.CreatedByExternal
						return r.Update(ctx, clone)
					})
					if err != nil {
						logger.Error(err, "update pod annotation[created-by=external] Failed",
							"at", agentsv1alpha1.SandboxResumeReasonCreatePod)
					} else {
						logger.Info("update pod annotation[created-by=external] success",
							"at", agentsv1alpha1.SandboxResumeReasonCreatePod)
					}
				}
			}
		} else if podCond.Status == corev1.ConditionFalse {
			logger.Info("Sandbox wait pod paused condition=True")
			return nil
		} else {
			cond.Reason = agentsv1alpha1.SandboxResumeReasonResumePod
			utils.SetSandboxCondition(newStatus, *cond)
		}

	case agentsv1alpha1.SandboxResumeReasonResumePod:
		if pod == nil {
			newStatus.Phase = agentsv1alpha1.SandboxFailed
			newStatus.Message = "Sandbox Pod Not Found"
			return nil
		}
		if value, ok := pod.Annotations[utils.PodAnnotationSandboxPause]; ok && value == utils.True {
			clone := pod.DeepCopy()
			clone.Annotations[utils.PodAnnotationSandboxPause] = utils.False
			patchBody := client.MergeFromWithOptions(pod, client.MergeFromWithOptimisticLock{})
			err := r.Patch(ctx, clone, patchBody)
			if err != nil {
				logger.Error(err, "Patch Pod Annotation Failed",
					"at", agentsv1alpha1.SandboxResumeReasonResumePod)
				return err
			}
			logger.Info("Patch pod annotations[alibabacloud.com/pause=false] success",
				"at", agentsv1alpha1.SandboxPausedReasonSetPause)
		} else if pod.Status.Phase == corev1.PodRunning {
			newStatus.Phase = agentsv1alpha1.SandboxRunning
			pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
			rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
			if pCond != nil && string(pCond.Status) != string(rCond.Status) {
				rCond.Status = metav1.ConditionStatus(pCond.Status)
				rCond.LastTransitionTime = pCond.LastTransitionTime
			}
			utils.SetSandboxCondition(newStatus, *rCond)
			utils.RemoveSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionResumed))
		}
	}
	return nil
}

func (r *SandboxReconciler) handlerPhaseTerminating(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box), "pod", klog.KObj(pod))

	var err error
	// 当 sandbox 处于 Paused 阶段没有 Pod 实体，所以需要VK调用PLM删除实例
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if cond != nil {
		if value, ok := box.Annotations[utils.SandboxAnnotationEnableVKDeleteInstance]; !ok || value == utils.True {
			clone := box.DeepCopy()
			if clone.Annotations == nil {
				clone.Annotations = map[string]string{}
			}
			clone.Annotations[utils.SandboxAnnotationEnableVKDeleteInstance] = utils.True
			patchBody := client.MergeFromWithOptions(box, client.MergeFromWithOptimisticLock{})
			err = r.Patch(ctx, clone, patchBody)
			if err != nil {
				logger.Error(err, "update sandbox annotation[alibabacloud.com/enable-vk-delete-instance] failed")
				return err
			}
			logger.Info("update sandbox annotation[alibabacloud.com/enable-vk-delete-instance]=true success")
		}
		logger.Info("Waiting VK delete instance")
		return nil
	}

	// 其它阶段，直接删除Pod即可
	if pod == nil {
		err = utils.UpdateFinalizer(r.Client, box, utils.RemoveFinalizerOpType, utils.SandboxFinalizer)
		if err != nil {
			fmt.Println(err.Error())
			logger.Error(err, "update sandbox finalizer failed")
			return err
		}
		logger.Info("remove sandbox finalizer success")
		return nil
	} else if !pod.DeletionTimestamp.IsZero() {
		logger.Info("Pod is in deleting, and wait a moment")
		return nil
	}

	err = r.Delete(ctx, pod)
	if err != nil {
		logger.Error(err, "delete pod failed")
		return err
	}
	logger.Info("delete pod success")
	return nil
}

func (r *SandboxReconciler) updateSandboxStatus(ctx context.Context, newStatus agentsv1alpha1.SandboxStatus, box *agentsv1alpha1.Sandbox) error {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	boxClone := box.DeepCopy()
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		if err := r.Get(context.TODO(), client.ObjectKey{Namespace: box.Namespace, Name: box.Name}, boxClone); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			logger.Error(err, "Failed to get updated Sandbox from client")
			return err
		}
		if reflect.DeepEqual(boxClone.Status, newStatus) {
			return nil
		}
		boxClone.Status = newStatus
		return r.Status().Update(ctx, boxClone)
	}); err != nil {
		logger.Error(err, "update Sandbox status failed")
		return err
	}
	klog.InfoS("update sandbox status success", "status", utils.DumpJson(boxClone.Status))
	return nil
}

func sandboxInPausingOrResuming(newStatus *agentsv1alpha1.SandboxStatus) bool {
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if newStatus.Phase == agentsv1alpha1.SandboxPaused && cond.Status == metav1.ConditionFalse {
		return true
	}
	if newStatus.Phase == agentsv1alpha1.SandboxResuming {
		return true
	}
	return false
}

// SetupWithManager sets up the controller with the Manager.
func (r *SandboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&agentsv1alpha1.Sandbox{}).
		Named("sandbox").
		Watches(&agentsv1alpha1.Sandbox{}, &handler.EnqueueRequestForObject{}).
		Watches(&corev1.Pod{}, &SandboxPodEventHandler{}).
		Complete(r)
}
