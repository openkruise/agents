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
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils/inplaceupdate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// 共享引擎处理 Pod 更新并观察完成状态；不读取或写入 Sandbox Condition。
// 事件、终态策略和最终 Ready 由调用方处理，错误分类本身不代表旧 Pod 健康。
type inplaceErrorClass int

const (
	inplaceClassUpdateFailed inplaceErrorClass = iota
	inplaceClassUntrackedPod
	inplaceClassUnsupportedChange
	inplaceClassQoSRejected
	inplaceClassStateCorrupted
)

// inplaceUpdateError is the single error type the engine returns for
// classified step failures: a class for dispatch plus a message for humans.
// The underlying cause, when there is one, stays reachable through Unwrap
// and is what requeuing branches hand back to the reconciler.
type inplaceUpdateError struct {
	Class inplaceErrorClass
	Cause error
	msg   string
}

func (e *inplaceUpdateError) Error() string {
	switch {
	case e.msg != "" && e.Cause != nil:
		return e.msg + ": " + e.Cause.Error()
	case e.msg != "":
		return e.msg
	case e.Cause != nil:
		return e.Cause.Error()
	}
	return "in-place update failed"
}

func (e *inplaceUpdateError) Unwrap() error { return e.Cause }

// newInplaceError builds a classified failure with a fixed message.
func newInplaceError(class inplaceErrorClass, msg string) *inplaceUpdateError {
	return &inplaceUpdateError{Class: class, msg: msg}
}

// wrapInplaceError builds a classified failure around an underlying cause;
// prefix may be empty when the cause message stands on its own.
func wrapInplaceError(class inplaceErrorClass, prefix string, cause error) *inplaceUpdateError {
	return &inplaceUpdateError{Class: class, msg: prefix, Cause: cause}
}

// Unclassified errors also fall under UpdateFailed, keeping the original cause
// for the caller to decide how to handle it.
func classifyInplaceError(err error) inplaceErrorClass {
	var ie *inplaceUpdateError
	if errors.As(err, &ie) {
		return ie.Class
	}
	return inplaceClassUpdateFailed
}

// inplaceUnderlyingError removes the engine's classification wrapper when a
// legacy adapter must preserve the underlying error text in a Condition.
func inplaceUnderlyingError(err error) error {
	var ie *inplaceUpdateError
	if errors.As(err, &ie) && ie.Cause != nil {
		return ie.Cause
	}
	return err
}

// Claim 与 SUO 原地 adapter 使用此分类映射终态；重建保留原有错误处理。
func isTerminalInplaceError(err error) bool {
	if classifyInplaceError(err) != inplaceClassUpdateFailed {
		return true
	}
	var resizeErr *inplaceupdate.ResizeNotSupportedError
	var applyErr *inplaceupdate.ResizeInfeasibleError
	return errors.As(err, &resizeErr) || errors.As(err, &applyErr) ||
		apierrors.IsForbidden(err) ||
		apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) || apierrors.IsMethodNotSupported(err)
}

func isUnsupportedResizeError(err error) bool {
	var resizeErr *inplaceupdate.ResizeNotSupportedError
	return errors.As(err, &resizeErr)
}

// error 非空时 step 仍有效；Succeeded 表示完成观察通过或 metadata 快速路径完成，
// 不代表 Pod Ready。部分写入重试误入快速路径的问题仍按 proposal 暂缓修复。
type inplaceUpdateStepResult int

const (
	// Pre-check failed or the current update is not yet complete; this write has
	// not been issued yet.
	inplaceUpdateStepInProgress inplaceUpdateStepResult = iota
	// A write has been attempted or is waiting to take effect, including write
	// failures and partially successful writes.
	inplaceUpdateStepPatchDelivered
	inplaceUpdateStepSucceeded
)

// The QoS pre-check is deliberately not part of validation: it only guards a
// write, so it runs immediately before the update is attempted.
func validateInplaceUpdate(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) (*inplaceupdate.InPlaceUpdateState, error) {
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == "" {
		return nil, newInplaceError(inplaceClassUntrackedPod, "pod has no template-hash label and does not support in-place update")
	}
	_, immutable := HashSandbox(box)
	if recorded := box.Annotations[agentsv1alpha1.SandboxHashImmutablePart]; recorded != "" && recorded != immutable {
		return nil, newInplaceError(inplaceClassUnsupportedChange, "in-place update only supports changing container images, resources and template metadata")
	}
	state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
	if err != nil {
		return nil, wrapInplaceError(inplaceClassStateCorrupted, "cannot determine in-place update progress", err)
	}
	return state, nil
}

// Configuration taking effect and final Ready are judged separately; the wait
// diagnostics are produced by the adapter based on Pod status.
func handleInPlaceUpdateCommon(ctx context.Context, control *inplaceupdate.InPlaceUpdateControl,
	pod *corev1.Pod, box *agentsv1alpha1.Sandbox, targetRevision string,
) (inplaceUpdateStepResult, error) {
	if err := ctx.Err(); err != nil {
		return inplaceUpdateStepInProgress, wrapInplaceError(inplaceClassUpdateFailed, "update cancelled", err)
	}

	state, err := validateInplaceUpdate(pod, box)
	if err != nil {
		return inplaceUpdateStepInProgress, err
	}

	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == targetRevision {
		return observeInplaceUpdate(ctx, pod, state)
	}

	// An earlier round may still be recorded on the pod. The latest target
	// supersedes it for both Claim and SUO. Keep track of pending state only so
	// metadata delivery cannot report success before the replacement target has
	// taken effect.
	previousPending := false
	if state != nil {
		completed, completedErr := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod, state)
		previousPending = !completed || completedErr != nil
	}

	metadataOnly := isMetadataOnlyChange(pod, box)
	if !metadataOnly {
		if orig, target, changed := inplaceupdate.CheckResizeQoSChange(box, pod); changed {
			return inplaceUpdateStepInProgress, newInplaceError(inplaceClassQoSRejected,
				fmt.Sprintf("resource resize would change QoS class from %s to %s, resize rejected", orig, target))
		}
	}
	// 直接下发新目标，保留现有底层更新模式及 metadata 快速路径。
	opts := inplaceupdate.InPlaceUpdateOptions{
		Pod:      pod,
		Box:      box,
		Revision: targetRevision,
	}
	patchCtx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerPatchPod)
	changed, err := control.Update(patchCtx, opts)
	tracing.EndSpan(patchCtx, span, err)
	if err != nil {
		return inplaceUpdateStepPatchDelivered, wrapInplaceError(inplaceClassUpdateFailed, "cannot deliver in-place update", err)
	}
	// Nothing needed writing: the pod already matches the target, so there is
	// no later observation to wait for.
	if !changed || (metadataOnly && !previousPending) {
		return inplaceUpdateStepSucceeded, nil
	}
	return inplaceUpdateStepPatchDelivered, nil
}

func observeInplaceUpdate(ctx context.Context, pod *corev1.Pod, state *inplaceupdate.InPlaceUpdateState) (inplaceUpdateStepResult, error) {
	completed, err := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod, state)
	if err != nil {
		return inplaceUpdateStepPatchDelivered, wrapInplaceError(inplaceClassUpdateFailed, "in-place pod update failed", err)
	}
	if completed {
		return inplaceUpdateStepSucceeded, nil
	}
	return inplaceUpdateStepPatchDelivered, nil
}

// describeInplaceWaitReason reports, from pod status facts only, why an
// in-flight in-place round may not be progressing: containers stuck in a
// waiting state (e.g. ImagePullBackOff, ErrImagePull) with the kubelet's
// reason and message. It returns "" when nothing abnormal is visible, so
// callers can distinguish "normally progressing" from "visibly stuck".
func describeInplaceWaitReason(pod *corev1.Pod) string {
	var parts []string
	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.State.Waiting == nil {
			continue
		}
		reason := cs.State.Waiting.Reason
		if cs.State.Waiting.Message != "" {
			reason += ": " + cs.State.Waiting.Message
		}
		parts = append(parts, fmt.Sprintf("container %s waiting: %s", cs.Name, reason))
	}
	return strings.Join(parts, "; ")
}

// isMetadataOnlyChange returns true if the only difference between the pod and
// sandbox template is metadata. Template-declared resources are compared as a
// subset so admission-injected extras do not turn metadata-only changes into
// an in-place update.
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
		if !inplaceupdate.IsResourceSatisfied(origin.Resources, container.Resources) {
			return false
		}
	}
	return true
}
