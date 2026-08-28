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
// Keep this list deliberately narrow. Other states are handled by kubelet
// retries and the SandboxSet ResourcePending timeout path.
const (
	waitingReasonCreateContainerConfigError = "CreateContainerConfigError"
	waitingReasonCreateContainerError       = "CreateContainerError"
	waitingReasonRunContainerError          = "RunContainerError"
	waitingReasonCrashLoopBackOff           = "CrashLoopBackOff"
	waitingReasonInvalidImageName           = "InvalidImageName"
	waitingReasonErrImageNeverPull          = "ErrImageNeverPull"
)

// classifyStartupFailure inspects pod status and reports whether the pod has
// entered a definitive startup failure. Unknown and transient Waiting reasons
// are ignored so kubelet retries and the ResourcePending timeout can handle
// them.
//
// The first hit wins. The returned reason is always
// SandboxReadyReasonStartContainerFailed to preserve the existing downstream
// contract (SandboxSet ScalingLimited, wait-ready task, claim short-circuit
// all key off this single reason). The returned message carries the
// originating pod- or container-level detail for diagnostics.
//
// If no definitive failure is present, failed is false and callers must leave
// the Ready condition reason unchanged.
func classifyStartupFailure(pod *corev1.Pod) (reason, message string, failed bool) {
	if pod == nil {
		return "", "", false
	}

	for i := range pod.Status.ContainerStatuses {
		cs := &pod.Status.ContainerStatuses[i]
		if cs.State.Waiting == nil {
			continue
		}
		if !isDefinitiveWaitingReason(cs.State.Waiting.Reason) {
			continue
		}
		return agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
			cs.State.Waiting.Message,
			true
	}
	return "", "", false
}

func isDefinitiveWaitingReason(reason string) bool {
	switch reason {
	case waitingReasonCreateContainerConfigError,
		waitingReasonCreateContainerError,
		waitingReasonRunContainerError,
		waitingReasonCrashLoopBackOff,
		waitingReasonInvalidImageName,
		waitingReasonErrImageNeverPull:
		return true
	default:
		return false
	}
}
