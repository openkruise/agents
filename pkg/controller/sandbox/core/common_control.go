package core

import (
	"context"
	"fmt"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

const CommonControlName = "common"

type commonControl struct {
	client.Client
}

func (r *commonControl) EnsureSandboxPhasePending(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	// If the Pod does not exist, it must first be created.
	if pod == nil {
		if _, err := r.createPod(ctx, box, newStatus); err != nil {
			return err
		}
	} else if pod.Status.Phase == corev1.PodRunning {
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

func (r *commonControl) EnsureSandboxPhaseRunning(ctx context.Context, args EnsureFuncArgs) error {
	pod, _, newStatus := args.Pod, args.Box, args.NewStatus
	// If a Pod is no longer present in the Running state, it should be considered an abnormal situation.
	if pod == nil {
		newStatus.Phase = agentsv1alpha1.SandboxFailed
		newStatus.Message = "Sandbox Pod Not Found"
		return nil
	}

	newStatus.PodInfo = agentsv1alpha1.PodInfo{
		PodIP:    pod.Status.PodIP,
		NodeName: pod.Spec.NodeName,
	}
	pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
	if pCond != nil && string(pCond.Status) != string(cond.Status) {
		cond.Status = metav1.ConditionStatus(pCond.Status)
		cond.LastTransitionTime = pCond.LastTransitionTime
	}
	utils.SetSandboxCondition(newStatus, *cond)
	return nil
}

func (r *commonControl) EnsureSandboxPhasePaused(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box), "pod", klog.KObj(pod))
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if cond == nil {
		cond = &metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionPaused),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxPausedReasonDeletePod,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, *cond)
	} else if cond.Status == metav1.ConditionTrue {
		return nil
	}

	// The paused phase sets condition ready to false.
	if rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady)); rCond.Status == metav1.ConditionTrue {
		rCond.Status = metav1.ConditionFalse
		rCond.LastTransitionTime = metav1.Now()
		utils.SetSandboxCondition(newStatus, *rCond)
	}

	// Pod deletion completed, paused completed
	if pod == nil {
		cond.Status = metav1.ConditionTrue
		utils.SetSandboxCondition(newStatus, *cond)
		return nil
	}
	// Pod deletion incomplete, waiting
	if !pod.DeletionTimestamp.IsZero() {
		logger.Info("Sandbox wait pod paused")
		return nil
	}
	err := r.Delete(ctx, pod, &client.DeleteOptions{GracePeriodSeconds: pointer.Int64(30)})
	if err != nil {
		logger.Error(err, "Delete pod failed")
		return err
	}
	logger.Info("Delete pod success")
	return nil
}

func (r *commonControl) EnsureSandboxPhaseResuming(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, newStatus := args.Pod, args.Box, args.NewStatus
	// first create pod
	var err error
	if pod == nil {
		_, err = r.createPod(ctx, box, newStatus)
		return err
	}

	if pod.Status.Phase == corev1.PodRunning {
		newStatus.Phase = agentsv1alpha1.SandboxRunning
		pCond := utils.GetPodCondition(&pod.Status, corev1.PodReady)
		rCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionReady))
		if pCond != nil && string(pCond.Status) != string(rCond.Status) {
			rCond.Status = metav1.ConditionStatus(pCond.Status)
			rCond.LastTransitionTime = pCond.LastTransitionTime
		}
		utils.SetSandboxCondition(newStatus, *rCond)
	}
	return nil
}

func (r *commonControl) EnsureSandboxPhaseTerminating(ctx context.Context, args EnsureFuncArgs) error {
	pod, box, _ := args.Pod, args.Box, args.NewStatus
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	var err error
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

func (r *commonControl) createPod(ctx context.Context, box *agentsv1alpha1.Sandbox, newStatus *agentsv1alpha1.SandboxStatus) (*corev1.Pod, error) {
	logger := logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))

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
	pod.Annotations[utils.PodAnnotationCreatedBy] = utils.CreatedBySandbox
	err := r.Create(ctx, pod)
	if err != nil && !errors.IsAlreadyExists(err) {
		logger.Error(err, "create pod failed")
		return nil, err
	}
	logger.Info("Create pod success", "Body", utils.DumpJson(pod))
	return pod, nil
}
