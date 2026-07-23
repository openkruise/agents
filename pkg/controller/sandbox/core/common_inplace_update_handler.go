/*
Copyright 2026.

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

// HandleInPlaceUpdateCommon handles the common inplace update logic.
//
// Return values:
//   - done: true when the inplace update flow has finished (completed, failed
//     terminally, or was a no-op) and the caller may continue with the regular
//     status sync. false means the update is still in progress and the caller
//     must early-return so transient conditions (Ready=False/InplaceUpdate)
//     are not overwritten by syncStatusFromPod.
//   - wrote: true when this call performed an actual write (patched the Pod or
//     set a condition on newStatus that will be persisted). Used by the caller
//     to mark the Reconcile tracing span as a write operation.
func handleInPlaceUpdateCommon(
	ctx context.Context,
	handler InPlaceUpdateHandler,
	pod *corev1.Pod,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) (done bool, wrote bool, err error) {
	_, hashImmutablePart := HashSandbox(box)
	// old Pod do not include Labels[pod-template-hash] and do not support inplace update.
	// Check if inplace update is supported
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == "" {
		return true, false, nil
		// todo, update inplaceupdate condition
	} else if box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] != "" &&
		box.Annotations[agentsv1alpha1.SandboxHashImmutablePart] != hashImmutablePart {
		klog.FromContext(ctx).Info("sandbox hash-immutable-part changed, and does not permit in-place upgrades", "sandbox", klog.KObj(box),
			"old hash", box.Annotations[agentsv1alpha1.SandboxHashImmutablePart],
			"new hash", hashImmutablePart)
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateForbidden",
			"InplaceUpdate only support image, resources, metadata")
		return true, false, nil
	}

	// Check if revision is consistent
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == newStatus.UpdateRevision {
		// Idempotent early exit: if the InplaceUpdate condition is already
		// Succeeded, the inplace update was completed in a previous Reconcile.
		// Skip re-evaluating completion and re-setting the condition. Return
		// done=true so the caller continues with the regular status sync, and
		// wrote=false so this no-op Reconcile is not marked as a write
		// operation (see MarkWrite in the caller), keeping its span eligible
		// for no-op filtering.
		existingCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
		if existingCond != nil && existingCond.Status == metav1.ConditionTrue &&
			existingCond.Reason == agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded {
			return true, false, nil
		}

		// If the InplaceUpdate condition is already in a terminal failure state
		// (e.g., resize subresource not available, infeasible, deferred), skip
		// re-evaluation. When the resize subresource call fails, the pod spec is
		// never updated, so spec==status (both old values) would cause
		// isPodResourceResizeCompleted to falsely report completion.
		if isInplaceUpdateTerminal(newStatus) {
			return true, false, nil
		}

		completed, terminalErr := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod)
		if !completed {
			if terminalErr != nil {
				msg := fmt.Sprintf("in-place resource resize failed: %v", terminalErr)
				klog.FromContext(ctx).Info(msg, "sandbox", klog.KObj(box))
				handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateFailed", msg)
				utils.SetSandboxCondition(newStatus, metav1.Condition{
					Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
					Status:             metav1.ConditionFalse,
					Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
					Message:            msg,
					LastTransitionTime: metav1.Now(),
				})
				return true, true, nil
			}
			return false, false, nil
		}
		cond := metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
			Status:             metav1.ConditionTrue,
			Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded,
			LastTransitionTime: metav1.Now(),
			Message:            "",
		}
		utils.SetSandboxCondition(newStatus, cond)
		return true, true, nil
	}

	// Check if there's already an ongoing update
	state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
	if err != nil {
		return false, false, err
		// state!=nil indicates that an in-place upgrade has already been performed previously.
	} else if state != nil {
		// currently, multiple in-place updates are not supported.
		klog.FromContext(ctx).Info("currently, multiple in-place updates are not supported", "sandbox", klog.KObj(box))
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateForbidden",
			"currently, multiple in-place updates are not supported")
		completed, terminalErr := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod)
		if !completed {
			if terminalErr != nil {
				klog.FromContext(ctx).Info("previous in-place resize is infeasible, skipping", "sandbox", klog.KObj(box), "error", terminalErr)
			}
			return false, false, nil
		}
		return true, false, nil
	}

	// If only metadata (labels/annotations) changed, directly patch the pod metadata
	// without going through the in-place update flow. This avoids unnecessarily
	// setting the InplaceUpdate condition and Ready=False, which would block
	// sandbox readiness for metadata-only changes.
	if isMetadataOnlyChange(pod, box) {
		klog.FromContext(ctx).Info("metadata-only change detected, patching pod metadata directly", "sandbox", klog.KObj(box))
		opts := inplaceupdate.InPlaceUpdateOptions{
			Pod:      pod,
			Box:      box,
			Revision: newStatus.UpdateRevision,
		}
		control := handler.GetInPlaceUpdateControl()
		if _, err := control.Update(ctx, opts); err != nil {
			msg := err.Error()
			handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateFailed", msg)
			return false, false, err
		}
		return true, true, nil
	}

	// Pre-check: reject resize if it would change the pod's QoS class
	origQoS, newQoS, qosChanged := inplaceupdate.CheckResizeQoSChange(box, pod)
	if qosChanged {
		msg := fmt.Sprintf("resource resize would change QoS class from %s to %s, resize rejected", origQoS, newQoS)
		klog.FromContext(ctx).Info(msg, "sandbox", klog.KObj(box))
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateFailed", msg)
		cond := metav1.Condition{
			Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
			Status:             metav1.ConditionFalse,
			Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
			Message:            msg,
			LastTransitionTime: metav1.Now(),
		}
		utils.SetSandboxCondition(newStatus, cond)
		return true, true, nil
	}

	// Start inplace update sandbox
	cond := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionInplaceUpdate),
		Status:             metav1.ConditionFalse,
		Reason:             agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
		LastTransitionTime: metav1.Now(),
	}
	utils.SetSandboxCondition(newStatus, cond)
	readyCond := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxConditionReady),
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Now(),
		Reason:             agentsv1alpha1.SandboxReadyReasonInplaceUpdating,
		Message:            "inplace update is incompleted",
	}
	utils.SetSandboxCondition(newStatus, readyCond)

	opts := inplaceupdate.InPlaceUpdateOptions{
		Pod:      pod,
		Box:      box,
		Revision: newStatus.UpdateRevision,
	}
	control := handler.GetInPlaceUpdateControl()
	changed, err := control.Update(ctx, opts)
	if err != nil {
		msg := err.Error()
		handler.GetRecorder().Eventf(box, corev1.EventTypeWarning, "InplaceUpdateFailed", msg)
		utils.SetSandboxCondition(newStatus, metav1.Condition{
			Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
			Status: metav1.ConditionFalse,
			Reason: agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
			// We need truncate msg here, K8s API errors can embed full PodSpec diffs that are too verbose for conditions.
			Message:            utils.TruncateConditionMessage(msg),
			LastTransitionTime: metav1.Now(),
		})
		// ResizeNotSupportedError is returned when both the pods/resize subresource
		// (K8s 1.33+) and the direct spec patch fallback (K8s 1.27-1.32) fail,
		// which typically means InPlacePodVerticalScaling is not enabled.
		// so we need treat this as terminal.
		var resizeErr *inplaceupdate.ResizeNotSupportedError
		if errors.As(err, &resizeErr) {
			return true, true, nil
		}
		return false, false, err
	} else if !changed {
		return true, false, nil
	}

	// The pod was patched and the in-place update is now in progress: report
	// wrote=true so the Reconcile span is marked as a write, and done=false so
	// the caller early-returns without overwriting transient conditions.
	return false, true, nil
}

// isMetadataOnlyChange returns true if the only difference between the pod and
// the sandbox template is metadata (labels/annotations), with no image or
// resource changes. When this is the case, the controller can directly patch
// the pod metadata without going through the full in-place update flow.
//
// Resource comparison is subset-based: only the resources declared in the
// sandbox template are checked against the pod. Extra resources injected into
// the pod (e.g., by LimitRanger or other admission webhooks) are ignored so
// that metadata-only changes are not mistakenly treated as in-place updates.
func isMetadataOnlyChange(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) bool {
	if box.Spec.Template == nil {
		return false
	}
	originContainers := make(map[string]corev1.Container, len(box.Spec.Template.Spec.Containers))
	for i := range box.Spec.Template.Spec.Containers {
		obj := box.Spec.Template.Spec.Containers[i]
		originContainers[obj.Name] = obj
	}
	for i := range pod.Spec.Containers {
		container := pod.Spec.Containers[i]
		origin, ok := originContainers[container.Name]
		if !ok {
			continue
		}
		if origin.Image != container.Image {
			return false
		}
		if !inplaceupdate.ResourcesEqual(origin.Resources, container.Resources) {
			return false
		}
	}
	return true
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
