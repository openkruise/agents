package core

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"

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

// HandleInPlaceUpdateCommon handles the common inplace update logic
func handleInPlaceUpdateCommon(
	ctx context.Context,
	handler InPlaceUpdateHandler,
	pod *corev1.Pod,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) (bool, error) {
	_, hashImmutablePart := HashSandbox(box)
	// old Pod do not include Labels[pod-template-hash] and do not support inplace update.
	// Check if inplace update is supported
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == "" {
		return true, nil
		// todo, update inplaceupdate condition
	} else if box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] != hashImmutablePart {
		klog.InfoS("sandbox hash-immutable-part changed, and does not permit in-place upgrades", "sandbox", klog.KObj(box),
			"old hash", box.Annotations[agentsv1alpha1.SandboxHashImmutablePart],
			"new hash", hashImmutablePart)
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateForbidden",
			"InplaceUpdate only support image, resources, metadata")
		return true, nil
	}

	// Check if revision is consistent
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == newStatus.UpdateRevision {
		// If the InplaceUpdate condition is already in a terminal failure state
		// (e.g., resize subresource not available, infeasible, deferred), skip
		// re-evaluation. When the resize subresource call fails, the pod spec is
		// never updated, so spec==status (both old values) would cause
		// isPodResourceResizeCompleted to falsely report completion.
		if isInplaceUpdateTerminal(newStatus) {
			return true, nil
		}

		completed, terminalErr := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod)
		if !completed {
			if terminalErr != nil {
				msg := fmt.Sprintf("in-place resource resize failed: %v", terminalErr)
				klog.InfoS(msg, "sandbox", klog.KObj(box))
				handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateFailed", msg)
				utils.SetSandboxCondition(newStatus, metav1.Condition{
					Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status:             metav1.ConditionFalse,
					Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					Message:            msg,
					LastTransitionTime: metav1.Now(),
				})
				return true, nil
			}
			return false, nil
		}
		cond := metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
			Status:             metav1.ConditionTrue,
			Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded,
			LastTransitionTime: metav1.Now(),
			Message:            "",
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
		klog.InfoS("currently, multiple in-place updates are not supported", "sandbox", klog.KObj(box))
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateForbidden",
			"currently, multiple in-place updates are not supported")
		completed, terminalErr := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod)
		if !completed {
			if terminalErr != nil {
				klog.InfoS("previous in-place resize is infeasible, skipping", "sandbox", klog.KObj(box), "error", terminalErr)
			}
			return false, nil
		}
		return true, nil
	}

	// Pre-check: reject resize if it would change the pod's QoS class
	origQoS, newQoS, qosChanged := inplaceupdate.CheckResizeQoSChange(box, pod)
	if qosChanged {
		msg := fmt.Sprintf("resource resize would change QoS class from %s to %s, resize rejected", origQoS, newQoS)
		klog.InfoS(msg, "sandbox", klog.KObj(box))
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateFailed", msg)
		cond := metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, cond)
		return true, nil
	}

	// Start inplace update sandbox
	markInProgress := func() {
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
	}
	opts := inplaceupdate.InPlaceUpdateOptions{
		Pod:        pod,
		Box:        box,
		Revision:   newStatus.UpdateRevision,
		OnProgress: markInProgress,
	}
	control := handler.GetInPlaceUpdateControl()
	changed, err := control.Update(ctx, opts)
	if err != nil {
		// ResizeNotSupportedError is returned when both the pods/resize subresource
		// (K8s 1.33+) and the direct spec patch fallback (K8s 1.27-1.32) fail,
		// which typically means InPlacePodVerticalScaling is not enabled.
		// so we need treat this as terminal.
		var resizeErr *inplaceupdate.ResizeNotSupportedError
		if errors.As(err, &resizeErr) {
			msg := resizeErr.Error()
			klog.InfoS(msg, "sandbox", klog.KObj(box))
			handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateFailed", msg)
			utils.SetSandboxCondition(newStatus, metav1.Condition{
				Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
				Status: metav1.ConditionFalse,
				Reason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
				// We need truncate msg here, K8s API errors can embed full PodSpec diffs that are too verbose for conditions.
				Message:            utils.TruncateConditionMessage(msg),
				LastTransitionTime: metav1.Now(),
			})
			return true, nil
		}
		return false, err
	} else if !changed {
		return true, nil
	}

	return false, nil
}

// isInplaceUpdateTerminal returns true if the InplaceUpdate condition has already
// reached a terminal state that should not be re-evaluated.
func isInplaceUpdateTerminal(newStatus *agentsv1alpha1.SandboxStatus) bool {
	cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
	if cond == nil {
		return false
	}
	switch cond.Reason {
	case agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
		agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded:
		return true
	}
	return false
}
