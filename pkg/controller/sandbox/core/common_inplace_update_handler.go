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
	"k8s.io/klog/v2"
)

// ---------------------------------------------------------------------------
// Shared in-place update engine
// ---------------------------------------------------------------------------
//
// This file is the single implementation of the in-place update state
// machine shared by the claim path (handleInPlaceUpdateCommon in
// common_control.go) and the upgrade path (performInplaceUpgrade in
// upgrade_control.go).
// runInplaceUpdateStep only establishes facts about the pod versus the target
// template: it never writes Sandbox conditions and never emits events —
// condition and event handling belongs to each caller's adapter, never here.
// Terminal failures are reported as a single error type, inplaceUpdateError:
// its class tells each caller's adapter how to react and its message carries
// the details for logs, events and conditions (InplaceUpdate/Ready conditions
// for the claim path, the Upgrading condition failMsg for the upgrade path).

// inplaceErrorClass groups step failures by the handling they require, so
// each adapter dispatches with a single switch. Classes are named after the
// handling strategy, not after the pod's condition, because dispatching is
// their whole purpose.
type inplaceErrorClass int

const (
	// inplaceClassTransient: a transient failure (conflict, network, API
	// error) worth a requeue. This is also the class of every error that is
	// not an inplaceUpdateError, so an unclassified failure degrades to
	// retry, never to a wrong terminal state.
	inplaceClassTransient inplaceErrorClass = iota
	// inplaceClassUntrackedPod: the pod predates the template-hash label and
	// cannot be tracked through an in-place update; the pod is untouched and
	// healthy.
	inplaceClassUntrackedPod
	// inplaceClassUnsupportedChange: the template change modifies the
	// hash-immutable part (anything beyond container images, resources and
	// template metadata); the pod is untouched and healthy.
	inplaceClassUnsupportedChange
	// inplaceClassTerminalRejected: retrying the same patch cannot succeed
	// (QoS class change, kubelet-side terminal failure, resize unsupported
	// by the cluster).
	inplaceClassTerminalRejected
	// inplaceClassStateCorrupted: the in-place update state annotation cannot
	// be parsed; progress is undeterminable.
	inplaceClassStateCorrupted
	// inplaceClassMetadataTransient: a transient failure of the metadata-only
	// fast path; the pod was never made not-ready.
	inplaceClassMetadataTransient
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

// classifyInplaceError returns the handling class of a step error. Anything
// that is not an inplaceUpdateError is transient — except
// inplaceupdate.ResizeNotSupportedError, which lives in a shared package that
// must not know controller-internal classes and is folded into the terminal
// class here (both the pods/resize subresource and the spec-patch fallback
// failed, typically because InPlacePodVerticalScaling is not enabled).
func classifyInplaceError(err error) inplaceErrorClass {
	var ie *inplaceUpdateError
	if errors.As(err, &ie) {
		return ie.Class
	}
	var resizeErr *inplaceupdate.ResizeNotSupportedError
	if errors.As(err, &resizeErr) {
		return inplaceClassTerminalRejected
	}
	return inplaceClassTransient
}

// inplaceUpdateStepResult describes a non-failure outcome of one engine pass.
type inplaceUpdateStepResult int

const (
	// inplaceUpdateStepInProgress: a delivered round is still being applied by
	// the kubelet (or a previous round must finish before a new one may
	// start). The pod was not mutated in this pass.
	inplaceUpdateStepInProgress inplaceUpdateStepResult = iota
	// inplaceUpdateStepPatchDelivered: this pass delivered a full in-place
	// patch and recorded a fresh completion baseline on the pod.
	inplaceUpdateStepPatchDelivered
	// inplaceUpdateStepSucceeded: the pod carries the target revision and the
	// round completed.
	inplaceUpdateStepSucceeded
	// inplaceUpdateStepMetadataPatched: the metadata-only fast path patched the
	// pod; no full round (and no completion baseline) was started.
	inplaceUpdateStepMetadataPatched
	// inplaceUpdateStepNoChange: the pod already matches the target template;
	// nothing was patched.
	inplaceUpdateStepNoChange
)

// runInplaceUpdateStep executes one pass of the shared in-place update state
// machine for the given target revision.
//
// The second return value is a wait diagnostic: when the result is
// inplaceUpdateStepInProgress and pod status explains why the round has not
// completed (e.g. a container stuck in ImagePullBackOff), it carries that
// reason so callers can pass it through to the user. Empty otherwise.
//
// MAINTAINERS: when the target revision changes while a previous round is
// still in progress, this function deliberately WAITS instead of re-patching
// for the new target (fail-stop). An earlier design had a per-caller "restart"
// policy that re-patched immediately so a rollback would not be blocked behind
// a stuck round; it was removed on purpose. Re-patching cannot rescue a stuck
// round anyway (completion is judged by an ImageID change and a container in
// ImagePullBackOff never restarts — see the WARNING on performInplaceUpgrade),
// and both callers have their liveness exits outside the engine: the claim
// path times out and replaces the sandbox from the pool, and the upgrade path
// is recovered by deleting the SUO and creating a Recreate/CheckpointRestore
// SUO. Waiting keeps the completion baseline correct and makes stuck rounds
// diagnosable through the wait reason instead of self-rescue.
//
// Classified failures are returned as *inplaceUpdateError; dispatch on
// classifyInplaceError:
//   - inplaceClassUntrackedPod, inplaceClassUnsupportedChange: the template
//     change cannot be applied in place; the pod is untouched.
//   - inplaceClassTerminalRejected: retrying the same patch cannot succeed
//     (QoS change, kubelet-side terminal failure, resize unsupported).
//   - inplaceClassStateCorrupted: the state annotation cannot be parsed.
//   - inplaceClassMetadataTransient: transient metadata fast-path failure.
//   - inplaceClassTransient (any other error): a transient patch failure
//     worth a requeue.
func runInplaceUpdateStep(
	ctx context.Context,
	control *inplaceupdate.InPlaceUpdateControl,
	pod *corev1.Pod,
	box *agentsv1alpha1.Sandbox,
	targetRevision string,
) (inplaceUpdateStepResult, string, error) {
	logger := klog.FromContext(ctx)

	// Old pods do not carry Labels[pod-template-hash] and cannot be tracked
	// through an in-place update.
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == "" {
		return inplaceUpdateStepInProgress, "", newInplaceError(inplaceClassUntrackedPod,
			"pod has no template-hash label and does not support in-place update")
	}
	// The in-place path can only change container images, resources and
	// template metadata; any other change flips the hash-immutable part.
	_, hashImmutablePart := HashSandbox(box)
	if recorded := box.Annotations[agentsv1alpha1.SandboxHashImmutablePart]; recorded != "" && recorded != hashImmutablePart {
		logger.Info("sandbox hash-immutable-part changed, and does not permit in-place upgrades", "sandbox", klog.KObj(box),
			"old hash", recorded, "new hash", hashImmutablePart)
		return inplaceUpdateStepInProgress, "", newInplaceError(inplaceClassUnsupportedChange,
			"in-place update only supports changing container images, resources and template metadata")
	}

	// Parse the in-place update state annotation once up front: both the
	// target-revision branch and the previous-round check below consume it.
	// A parse failure here is pure defensive programming — the annotation is
	// written by this controller — so surface it and stop.
	state, stateErr := inplaceupdate.GetPodInPlaceUpdateState(pod)
	if stateErr != nil {
		return inplaceUpdateStepInProgress, "", wrapInplaceError(inplaceClassStateCorrupted,
			"cannot determine in-place update progress", stateErr)
	}

	// The pod already carries the target revision: the patch was delivered in
	// a previous reconcile, so judge progress from the recorded state.
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == targetRevision {
		completed, terminalErr := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod, state)
		if completed {
			return inplaceUpdateStepSucceeded, "", nil
		}
		if terminalErr != nil {
			return inplaceUpdateStepInProgress, "", wrapInplaceError(inplaceClassTerminalRejected,
				"in-place pod update failed", terminalErr)
		}
		// Patch applied, waiting for the kubelet to finish; report what the pod
		// status says is holding the round up (e.g. ImagePullBackOff).
		return inplaceUpdateStepInProgress, describeInplaceWaitReason(pod), nil
	}

	// The target revision differs from the pod's: a still-running previous
	// round blocks the new one (fail-stop; see the MAINTAINERS note above).
	if state != nil {
		completed, terminalErr := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod, state)
		if !completed && terminalErr == nil {
			// The previous round is still in progress. Starting a new round
			// now would rebuild the completion baseline from a pod whose
			// containers are mid-transition; wait for it to finish and report
			// what is holding it up so the user can decide how to recover
			// (e.g. delete the SUO and switch to Recreate for a stuck image).
			return inplaceUpdateStepInProgress, describeInplaceWaitReason(pod), nil
		}
		// The previous round has either completed or terminally failed
		// (e.g. infeasible resize); the pod is stable either way, so start
		// a new round with a correct completion baseline. This lets a
		// corrected template recover a terminally failed resize instead of
		// leaving the sandbox permanently failed.
		if terminalErr != nil {
			// Exception: a stuck image pull is terminal for reporting, but the
			// pod is NOT stable — its container is mid-pull. Re-patching cannot
			// rescue it (a container in ImagePullBackOff does not pick up a
			// changed spec.image; E2E-verified) and rebuilding the completion
			// baseline from a mid-pull pod would corrupt the next round's
			// judgement, so keep waiting (fail-stop). The liveness exits stay
			// outside the engine: claim timeout with pool replacement, or
			// deleting the SUO and switching to Recreate.
			var pullErr *inplaceupdate.ImagePullFailedError
			if errors.As(terminalErr, &pullErr) {
				return inplaceUpdateStepInProgress, describeInplaceWaitReason(pod), nil
			}
			logger.Info("previous in-place update terminally failed, starting a new round",
				"sandbox", klog.KObj(box), "error", terminalErr)
		}
	}

	// If only metadata (labels/annotations) changed, patch the pod metadata
	// directly without starting a full in-place round, so callers do not have
	// to treat the change as an update in progress.
	if isMetadataOnlyChange(pod, box) {
		logger.Info("metadata-only change detected, patching pod metadata directly", "sandbox", klog.KObj(box))
		if _, err := deliverInplacePatch(ctx, control, pod, box, targetRevision); err != nil {
			return inplaceUpdateStepInProgress, "", wrapInplaceError(inplaceClassMetadataTransient, "", err)
		}
		return inplaceUpdateStepMetadataPatched, "", nil
	}

	// Pre-check: reject a resize that would change the pod's QoS class, which
	// Kubernetes does not allow for in-place resource updates.
	if origQoS, newQoS, qosChanged := inplaceupdate.CheckResizeQoSChange(box, pod); qosChanged {
		return inplaceUpdateStepInProgress, "", newInplaceError(inplaceClassTerminalRejected,
			fmt.Sprintf("resource resize would change QoS class from %s to %s, resize rejected", origQoS, newQoS))
	}

	// Deliver the full patch (resize + metadata/image patch). Update() records
	// the completion baseline for the new target in the state annotation.
	changed, err := deliverInplacePatch(ctx, control, pod, box, targetRevision)
	if err != nil {
		// Returned raw: classifyInplaceError folds ResizeNotSupportedError into
		// the terminal class and treats everything else as transient.
		return inplaceUpdateStepInProgress, "", err
	}
	if !changed {
		return inplaceUpdateStepNoChange, "", nil
	}
	return inplaceUpdateStepPatchDelivered, "", nil
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

// deliverInplacePatch invokes control.Update under a PatchPod tracing span.
// Write retention is handled by the write-tracking client underneath.
func deliverInplacePatch(
	ctx context.Context,
	control *inplaceupdate.InPlaceUpdateControl,
	pod *corev1.Pod,
	box *agentsv1alpha1.Sandbox,
	targetRevision string,
) (bool, error) {
	opts := inplaceupdate.InPlaceUpdateOptions{
		Pod:      pod,
		Box:      box,
		Revision: targetRevision,
	}
	patchCtx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerPatchPod)
	changed, err := control.Update(patchCtx, opts)
	tracing.EndSpan(patchCtx, span, err)
	return changed, err
}

// isMetadataOnlyChange returns true if the only difference between the pod and
// the sandbox template is metadata (labels/annotations), with no image or
// resource changes. When this is the case, the controller can directly patch
// the pod metadata without going through the full in-place update flow.
//
// Resource comparison is exact for every resource declared in the sandbox
// template: the pod must have the same value for each. Extra resources
// injected into the pod (e.g., by LimitRanger or other admission webhooks) are
// ignored so that metadata-only changes are not mistakenly treated as
// in-place updates. Using exact comparison (not >=) ensures that lowering a
// resource in the template is detected as a real change requiring an in-place
// resize, not a no-op metadata patch that would bypass the QoS guard.
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
		if !inplaceupdate.ResourcesExactlyEqual(origin.Resources, container.Resources) {
			return false
		}
	}
	return true
}
