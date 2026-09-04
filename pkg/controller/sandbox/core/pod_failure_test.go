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
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func TestClassifyStartupFailure(t *testing.T) {
	containerWaiting := func(reason string) []corev1.ContainerStatus {
		return []corev1.ContainerStatus{{
			Name: "main",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason:  reason,
				Message: "kubelet message",
			}},
		}}
	}

	tests := []struct {
		name          string
		pod           *corev1.Pod
		failed        bool
		expectMessage string
	}{
		{name: "nil pod"},
		{name: "running container", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}},
		{name: "create container config error", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting(waitingReasonCreateContainerConfigError)}}, failed: true},
		{name: "create container error", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting(waitingReasonCreateContainerError)}}, failed: true},
		{name: "run container error", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting(waitingReasonRunContainerError)}}, failed: true},
		{name: "crash loop backoff is transient", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting("CrashLoopBackOff")}}},
		{
			name: "restart count below threshold is transient",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", RestartCount: definitiveRestartThreshold - 1,
			}}}},
		},
		{
			name: "restart count at threshold is definitive",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", RestartCount: definitiveRestartThreshold,
			}}}},
			failed:        true,
			expectMessage: `container "main" has restarted 5 times and is not ready`,
		},
		{
			name: "running container at restart threshold is definitive",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", RestartCount: definitiveRestartThreshold,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}}},
			failed:        true,
			expectMessage: `container "main" has restarted 5 times and is not ready`,
		},
		{
			name: "ready container at restart threshold is not a failure",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name: "main", RestartCount: definitiveRestartThreshold, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}}},
		},
		{
			name: "init container at restart threshold is definitive",
			pod: &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name: "init", RestartCount: definitiveRestartThreshold,
			}}}},
			failed:        true,
			expectMessage: `container "init" has restarted 5 times and is not ready`,
		},
		{name: "invalid image name", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting(waitingReasonInvalidImageName)}}, failed: true},
		{name: "image never pull", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting(waitingReasonErrImageNeverPull)}}, failed: true},
		{name: "container creating is transient", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting("ContainerCreating")}}},
		{name: "image pull backoff is transient", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting("ImagePullBackOff")}}},
		{name: "err image pull is transient", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting("ErrImagePull")}}},
		{name: "unknown waiting reason is transient", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting("FutureKubeletReason")}}},
		{
			name: "unschedulable is a definitive failure",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  corev1.PodReasonUnschedulable,
				Message: "kubelet message",
			}}}},
			failed: true,
		},
		{
			name: "scheduler error is transient",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "SchedulerError",
			}}}},
		},
		{
			name: "pod scheduled true is not a failure",
			pod: &corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionTrue,
			}}}},
		},
		{
			name:   "init container config error is a definitive failure",
			pod:    &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: containerWaiting(waitingReasonCreateContainerConfigError)}},
			failed: true,
		},
		{
			name: "init container failure wins over app container",
			pod: &corev1.Pod{Status: corev1.PodStatus{
				InitContainerStatuses: containerWaiting(waitingReasonInvalidImageName),
				ContainerStatuses:     containerWaiting(waitingReasonCreateContainerError),
			}},
			failed: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, message, failed := classifyStartupFailure(tt.pod)
			assert.Equal(t, tt.failed, failed)
			if tt.failed {
				assert.Equal(t, agentsv1alpha1.SandboxReadyReasonStartContainerFailed, reason)
				if tt.expectMessage != "" {
					assert.Equal(t, tt.expectMessage, message)
				} else {
					assert.Equal(t, "kubelet message", message)
				}
			} else {
				assert.Empty(t, reason)
				assert.Empty(t, message)
			}
		})
	}
}

func TestDefaultSyncStatusFromPodStartupFailure(t *testing.T) {
	tests := []struct {
		name           string
		waitingReason  string
		restartCount   int32
		containerReady bool
		previousReason string
		expectReason   string
		expectMessage  string
	}{
		{
			name:          "definitive waiting reason sets startup failure",
			waitingReason: waitingReasonCreateContainerError,
			expectReason:  agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
			expectMessage: "kubelet message",
		},
		{
			name:           "transient reason clears prior startup failure",
			waitingReason:  "ContainerCreating",
			previousReason: agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
			expectReason:   agentsv1alpha1.SandboxReadyReasonPodReady,
			expectMessage:  "",
		},
		{
			name:           "existing pod keeps prior pod create failure",
			waitingReason:  "ContainerCreating",
			previousReason: agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
			expectReason:   agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
			expectMessage:  "previous failure",
		},
		{
			name:          "restart threshold sets startup failure",
			waitingReason: "CrashLoopBackOff",
			restartCount:  definitiveRestartThreshold,
			expectReason:  agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
			expectMessage: `container "main" has restarted 5 times and is not ready`,
		},
		{
			name:           "ready container clears restart failure",
			waitingReason:  "",
			restartCount:   definitiveRestartThreshold,
			containerReady: true,
			previousReason: agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
			expectReason:   agentsv1alpha1.SandboxReadyReasonPodReady,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			podReadyStatus := corev1.ConditionFalse
			if tt.containerReady {
				podReadyStatus = corev1.ConditionTrue
			}
			containerState := corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			if tt.waitingReason != "" {
				containerState = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
					Reason: tt.waitingReason, Message: "kubelet message",
				}}
			}
			pod := &corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: podReadyStatus,
				}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:         "main",
					State:        containerState,
					Ready:        tt.containerReady,
					RestartCount: tt.restartCount,
				}},
			}}
			status := &agentsv1alpha1.SandboxStatus{Conditions: []metav1.Condition{{
				Type:    string(agentsv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  tt.previousReason,
				Message: "previous failure",
			}}}

			defaultSyncStatusFromPod(pod, status, true, classifyStartupFailure)

			condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
			assert.Equal(t, tt.expectReason, condition.Reason)
			assert.Equal(t, tt.expectMessage, condition.Message)
		})
	}
}
