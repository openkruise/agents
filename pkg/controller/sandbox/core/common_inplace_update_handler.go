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
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
)

// The shared engine updates Pods and observes completion without accessing Sandbox conditions.
// inplaceupdate classifies errors; callers own events, terminal failure policy, and final readiness.
// The engine emits MemoryDownscaleSkipped directly as the sole exception.

// inplaceUnderlyingError removes the engine's classification wrapper when a
// legacy adapter must preserve the underlying error text in a Condition.
func inplaceUnderlyingError(err error) error {
	var ie *inplaceupdate.Error
	if errors.As(err, &ie) && ie.Cause != nil {
		return ie.Cause
	}
	return err
}

// isTerminalInplaceError classifies write errors for the SUO in-place adapter.
func isTerminalInplaceError(err error) bool {
	if inplaceupdate.ClassifyError(err) != inplaceupdate.InplaceUpdateFailed {
		return true
	}
	var resizeErr *inplaceupdate.ResizeNotSupportedError
	return errors.As(err, &resizeErr) ||
		apierrors.IsForbidden(err) ||
		apierrors.IsUnauthorized(err) || apierrors.IsInvalid(err) ||
		apierrors.IsBadRequest(err) || apierrors.IsMethodNotSupported(err)
}

// The step remains valid with a non-nil error; Succeeded means observation passed or no write was needed, not Pod Ready.
type inplaceUpdateStepResult int

const (
	// The update has not completed, including write or observation failures.
	// Failure and retry policy are determined by the accompanying error.
	inplaceUpdateStepInProgress inplaceUpdateStepResult = iota
	// Completion observation passed or nothing needed writing; this does not
	// imply Pod Ready.
	inplaceUpdateStepSucceeded
	// Validation, the resize pre-check, or completion observation reported a
	// terminal failure for the current update round; the accompanying error
	// describes the cause.
	inplaceUpdateStepFailed
)

// The QoS pre-check is deliberately not part of validation: it only guards a
// write, so it runs immediately before the update is attempted.
func validateInplaceUpdate(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) (*inplaceupdate.InPlaceUpdateState, error) {
	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] == "" {
		return nil, inplaceupdate.NewError(inplaceupdate.InplaceUpdateInvalidPodState, "pod has no template-hash label and does not support in-place update", nil)
	}
	_, immutable := HashSandbox(box)
	if recorded := box.Annotations[agentsv1alpha1.SandboxHashImmutablePart]; recorded != "" && recorded != immutable {
		return nil, inplaceupdate.NewError(inplaceupdate.InplaceUpdateUnsupportedChange, "in-place update only supports changing container images, resources and template metadata", nil)
	}
	state, err := inplaceupdate.GetPodInPlaceUpdateState(pod)
	if err != nil {
		return nil, inplaceupdate.NewError(inplaceupdate.InplaceUpdateInvalidPodState, "cannot determine in-place update progress", err)
	}
	return state, nil
}

// Configuration completion and final readiness are checked separately; callers handle status using the shared step and error.
// Successful writes update the caller-owned pod with the response for readiness checks, status synchronization, and wait diagnostics.
func handleInPlaceUpdateCommon(ctx context.Context, control *inplaceupdate.InPlaceUpdateControl, recorder record.EventRecorder,
	pod *corev1.Pod, box *agentsv1alpha1.Sandbox, targetRevision string,
) (inplaceUpdateStepResult, error) {
	// Check that the pod has a template-hash label, the sandbox's recorded
	// immutable template hash (if present) matches the current template, and
	// any existing pod in-place update state annotation can be parsed.
	state, err := validateInplaceUpdate(pod, box)
	if err != nil {
		return inplaceUpdateStepFailed, err
	}

	if pod.Labels[agentsv1alpha1.PodLabelTemplateHash] != targetRevision {
		logger := klog.FromContext(ctx)
		opts := inplaceupdate.InPlaceUpdateOptions{
			Pod:      pod,
			Box:      box,
			Revision: targetRevision,
		}
		// Memory downscale is rejected at the claim write path (SetResources) and
		// at claim admission; here we only surface an advisory event. A hard
		// failure would misclassify pods whose memory was raised by the
		// environment (LimitRange minimums, admission webhooks, VPA actuation) as
		// downscales and permanently block unrelated image/metadata rollouts.
		if downscaleErr := inplaceupdate.CheckMemoryDownscale(box, pod); downscaleErr != nil {
			logger.Info("skipping memory downscale", "sandbox", klog.KObj(box), "reason", downscaleErr.Error())
			if recorder != nil {
				recorder.Eventf(box, corev1.EventTypeWarning, "MemoryDownscaleSkipped", "%s", downscaleErr.Error())
			}
		}
		metadataOnly := isMetadataOnlyChange(pod, box)
		if metadataOnly {
			logger.Info("metadata-only change detected, patching pod metadata directly", "sandbox", klog.KObj(box))
		}
		// The QoS guard only protects a resize, so it is skipped when the
		// template resources are already satisfied (e.g. a metadata-only change).
		if len(inplaceupdate.DefaultBuildResizeContainers(opts)) > 0 {
			if orig, target, changed := inplaceupdate.CheckResizeQoSChange(box, pod); changed {
				return inplaceUpdateStepFailed, inplaceupdate.NewError(inplaceupdate.InplaceUpdateResizeRejected,
					fmt.Sprintf("resource resize would change QoS class from %s to %s, resize rejected", orig, target), nil)
			}
		}
		// The latest target supersedes any earlier round still recorded on the pod.
		patchCtx, span := tracing.StartControllerSpan(ctx, tracing.SpanControllerPatchPod)
		patched, err := control.Update(patchCtx, opts)
		tracing.EndSpan(patchCtx, span, err)
		if err != nil {
			return inplaceUpdateStepInProgress, inplaceupdate.NewError(inplaceupdate.InplaceUpdateFailed, "cannot deliver in-place update", err)
		}
		// Nothing needed writing: the pod already matches the target, so there is
		// no later observation to wait for.
		if patched == nil {
			return inplaceUpdateStepSucceeded, nil
		}
		// Register the final written version based on this update's intent, not prior state retained by a metadata-only update.
		if !metadataOnly {
			ResourceVersionExpectations.Expect(patched)
		}
		// Observe the delivered pod with the same check used once the hash
		// matches. A metadata-only write keeps the state of an earlier round, so
		// an unfinished resize still reports InProgress.
		patched.DeepCopyInto(pod)
		if state, err = inplaceupdate.GetPodInPlaceUpdateState(pod); err != nil {
			return inplaceUpdateStepFailed, inplaceupdate.NewError(inplaceupdate.InplaceUpdateInvalidPodState, "cannot determine in-place update progress", err)
		}
	}

	completed, err := inplaceupdate.IsInplaceUpdateCompleted(ctx, pod, state)
	if err != nil {
		return inplaceUpdateStepFailed, err
	}
	if completed {
		return inplaceUpdateStepSucceeded, nil
	}
	return inplaceUpdateStepInProgress, nil
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
