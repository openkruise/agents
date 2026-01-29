package core

import (
	"context"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
)

// CommonInPlaceUpdateHandler implements the inplace update handler for common controller
type CommonInPlaceUpdateHandler struct {
	control  *inplaceupdate.InPlaceUpdateControl
	recorder record.EventRecorder
}

func (h *CommonInPlaceUpdateHandler) GetInPlaceUpdateControl() *inplaceupdate.InPlaceUpdateControl {
	return h.control
}

func (h *CommonInPlaceUpdateHandler) GetRecorder() record.EventRecorder {
	return h.recorder
}

func (h *CommonInPlaceUpdateHandler) GetLogger(ctx context.Context, box *agentsv1alpha1.Sandbox) logr.Logger {
	return logf.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
}

// HandleInPlaceUpdateCommon handles the common inplace update logic
func handleInPlaceUpdateCommon(
	ctx context.Context,
	handler InPlaceUpdateHandler,
	pod *corev1.Pod,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) (bool, error) {
	logger := handler.GetLogger(ctx, box)

	_, hashWithoutImageAndResource := HashSandbox(box)
	// old Pod do not include Labels[pod-template-hash] and do not support inplace update.
	// Check if inplace update is supported
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == "" {
		return true, nil
		// todo, update inplaceupdate condition
	} else if box.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources] != hashWithoutImageAndResource {
		logger.Info("sandbox hash-without-image-resources changed, and does not permit in-place upgrades",
			"old hash", box.Annotations[agentsv1alpha1.SandboxHashWithoutImageAndResources],
			"new hash", hashWithoutImageAndResource)
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateForbidden",
			"InplaceUpdate only support image, resources")
		return true, nil
	}

	// Check if revision is consistent
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == newStatus.UpdateRevision {
		// inplace update is incompleted
		if !inplaceupdate.IsInplaceUpdateCompleted(ctx, pod) {
			return false, nil
		}
		cond := metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
			Status:             metav1.ConditionTrue,
			Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, cond)
		return true, nil
	}

	// Check if there's already an ongoing update
	state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
	if err != nil {
		return false, err
		// state!=nil indicates that an in-place upgrade has already been performed previously.
	} else if state != nil {
		// currently, multiple in-place updates are not supported.
		logger.Info("currently, multiple in-place updates are not supported")
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateForbidden",
			"currently, multiple in-place updates are not supported")
		// inplace update is incompleted
		if !inplaceupdate.IsInplaceUpdateCompleted(ctx, pod) {
			return false, nil
		}
		return true, nil
	}

	// Start inplace update sandbox
	opts := inplaceupdate.InPlaceUpdateOptions{Pod: pod, Box: box, Revision: newStatus.UpdateRevision}
	control := handler.GetInPlaceUpdateControl()
	changed, err := control.Update(ctx, opts)
	if err != nil {
		return false, err
	} else if !changed {
		return true, nil
	}

	// Update sandbox status to in-progress
	cond := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
		LastTransitionTime: metav1.Now(),
	}
	utils.SetSandboxCondition(newStatus, cond)

	// Update ready condition to in-progress
	readyCond := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
		Message:            "inplace update is incompleted",
	}
	utils.SetSandboxCondition(newStatus, readyCond)

	return false, nil
}
