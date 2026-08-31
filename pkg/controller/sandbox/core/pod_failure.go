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
	"fmt"

	corev1 "k8s.io/api/core/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// Container Waiting.Reason values that indicate a definitive startup failure.
//
// These strings are the Err().Error() values written by kubelet into
// ContainerStateWaiting.Reason. Their canonical definitions live in
// k8s.io/kubernetes/pkg/kubelet/{container,kuberuntime,images}, which is an
// internal module of kube and is not exposed as a stable API. We mirror the
// exact string values here so we do not take a dependency on k8s.io/kubernetes.
//
// Keep this list deliberately narrow. Transient Waiting reasons are not
// failures by themselves. Repeated failures are detected separately through
// the monotonic RestartCount so classification does not flap when a container
// switches between CrashLoopBackOff and Running.
const (
	// RestartCount currently spans the Pod lifetime. If Sandbox reuse is
	// introduced, compare against a per-lease restart baseline instead so
	// restarts from an earlier lease do not classify the next one as failed.
	definitiveRestartThreshold              = 5
	waitingReasonCreateContainerConfigError = "CreateContainerConfigError"
	waitingReasonCreateContainerError       = "CreateContainerError"
	waitingReasonRunContainerError          = "RunContainerError"
	waitingReasonInvalidImageName           = "InvalidImageName"
	waitingReasonErrImageNeverPull          = "ErrImageNeverPull"
)

// classifyStartupFailure inspects pod status and reports whether the pod has
// entered a definitive startup failure. Unknown and transient Waiting reasons
// are ignored until a non-ready container reaches the restart threshold.
//
// The first hit wins, and init containers are checked before app containers
// since they run first and block the app containers just as definitively.
// The returned reason is always SandboxReadyReasonStartContainerFailed to
// preserve the existing downstream contract (SandboxSet ScalingLimited,
// wait-ready task, claim short-circuit all key off this single reason). The
// returned message carries the originating pod- or container-level detail
// for diagnostics.
//
// If no definitive failure is present, failed is false and callers must leave
// the Ready condition reason unchanged. Since callers key off the returned
// failed flag, transient recoveries (e.g. scheduler finding a node, kubelet
// succeeding on a retry) will naturally clear the Ready reason on the next
// sync via the caller's recovery path.
func classifyStartupFailure(pod *corev1.Pod) (reason, message string, failed bool) {
	if pod == nil {
		return "", "", false
	}

	// PodScheduled=False with Reason=Unschedulable is treated as a definitive
	// startup failure. The scheduler keeps retrying, so if capacity or
	// tolerations change the condition flips back to True and the caller's
	// recovery path clears the reason. Other PodScheduled reasons (e.g.
	// SchedulerError) are transient scheduler-internal errors and are left
	// to the ResourcePending timeout.
	for i := range pod.Status.Conditions {
		c := &pod.Status.Conditions[i]
		if c.Type != corev1.PodScheduled {
			continue
		}
		if c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
				c.Message,
				true
		}
		break
	}

	if msg, ok := firstDefinitiveWaitingMessage(pod.Status.InitContainerStatuses); ok {
		return agentsv1alpha1.SandboxReadyReasonStartContainerFailed, msg, true
	}
	if msg, ok := firstDefinitiveWaitingMessage(pod.Status.ContainerStatuses); ok {
		return agentsv1alpha1.SandboxReadyReasonStartContainerFailed, msg, true
	}
	return "", "", false
}

// firstDefinitiveWaitingMessage returns a diagnostic for the first container
// that has repeatedly restarted while not Ready or whose Waiting.Reason is on
// the definitive-failure allow-list.
func firstDefinitiveWaitingMessage(statuses []corev1.ContainerStatus) (string, bool) {
	for i := range statuses {
		cs := &statuses[i]
		// RestartCount is monotonic and remains observable while the container is
		// briefly Running between CrashLoopBackOff periods.
		if cs.RestartCount >= definitiveRestartThreshold && !cs.Ready {
			return fmt.Sprintf("container %q has restarted %d times and is not ready", cs.Name, cs.RestartCount), true
		}
		if cs.State.Waiting == nil {
			continue
		}
		if !isDefinitiveWaitingReason(cs.State.Waiting.Reason) {
			continue
		}
		return cs.State.Waiting.Message, true
	}
	return "", false
}

func isDefinitiveWaitingReason(reason string) bool {
	switch reason {
	case waitingReasonCreateContainerConfigError,
		waitingReasonCreateContainerError,
		waitingReasonRunContainerError,
		waitingReasonInvalidImageName,
		waitingReasonErrImageNeverPull:
		return true
	default:
		return false
	}
}
