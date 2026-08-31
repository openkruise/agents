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
		name   string
		pod    *corev1.Pod
		failed bool
	}{
		{name: "nil pod"},
		{name: "running container", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}},
		{name: "create container config error", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting(waitingReasonCreateContainerConfigError)}}, failed: true},
		{name: "create container error", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting(waitingReasonCreateContainerError)}}, failed: true},
		{name: "run container error", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting(waitingReasonRunContainerError)}}, failed: true},
		{name: "crash loop backoff is transient", pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: containerWaiting("CrashLoopBackOff")}}},
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
				assert.Equal(t, "kubelet message", message)
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
			name:           "transient reason clears recovered startup failure",
			waitingReason:  "ContainerCreating",
			previousReason: agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
			expectReason:   agentsv1alpha1.SandboxReadyReasonPodReady,
		},
		{
			name:           "existing pod clears recovered pod create failure",
			waitingReason:  "ContainerCreating",
			previousReason: agentsv1alpha1.SandboxReadyReasonPodCreateFailed,
			expectReason:   agentsv1alpha1.SandboxReadyReasonPodReady,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionFalse,
				}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: "main",
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
						Reason: tt.waitingReason, Message: "kubelet message",
					}},
				}},
			}}
			status := &agentsv1alpha1.SandboxStatus{Conditions: []metav1.Condition{{
				Type:    string(agentsv1alpha1.SandboxConditionReady),
				Status:  metav1.ConditionFalse,
				Reason:  tt.previousReason,
				Message: "previous failure",
			}}}

			defaultSyncStatusFromPod(pod, status, true)

			condition := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionReady))
			assert.Equal(t, tt.expectReason, condition.Reason)
			assert.Equal(t, tt.expectMessage, condition.Message)
		})
	}
}
