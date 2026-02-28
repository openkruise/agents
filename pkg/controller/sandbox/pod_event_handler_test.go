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

package sandbox

import (
	"testing"

	"github.com/openkruise/agents/pkg/utils"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Helper function to create a pointer to a bool
func boolPtr(b bool) *bool {
	return &b
}

func TestIsActivePodUpdate(t *testing.T) {
	tests := []struct {
		name        string
		oldPod      *corev1.Pod
		newPod      *corev1.Pod
		expected    bool
		description string
	}{
		{
			name: "Phase changed from Pending to Running",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expected:    true,
			description: "should return true when pod phase changes",
		},
		{
			name: "Phase changed from Running to Failed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
				},
			},
			expected:    true,
			description: "should return true when pod phase changes to failed",
		},
		{
			name: "PodIP changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.2",
				},
			},
			expected:    true,
			description: "should return true when pod IP changes",
		},
		{
			name: "PodIP assigned from empty",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "",
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
				},
			},
			expected:    true,
			description: "should return true when pod IP is assigned",
		},
		{
			name: "PodReady condition status changed from False to True",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expected:    true,
			description: "should return true when PodReady condition status changes",
		},
		{
			name: "PodReady condition reason changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
							Reason: "ContainersReady",
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
							Reason: "PodCompleted",
						},
					},
				},
			},
			expected:    true,
			description: "should return true when PodReady condition reason changes",
		},
		{
			name: "PodReady condition message changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodReady,
							Status:  corev1.ConditionTrue,
							Message: "All containers ready",
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodReady,
							Status:  corev1.ConditionTrue,
							Message: "Containers ready updated",
						},
					},
				},
			},
			expected:    true,
			description: "should return true when PodReady condition message changes",
		},
		{
			name: "PodReady condition added",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:      corev1.PodRunning,
					Conditions: []corev1.PodCondition{},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expected:    true,
			description: "should return true when PodReady condition is added",
		},
		{
			name: "ContainersPaused condition status changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   utils.PodConditionContainersPaused,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   utils.PodConditionContainersPaused,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expected:    true,
			description: "should return true when ContainersPaused condition changes",
		},
		{
			name: "ContainersPaused condition added",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:      corev1.PodRunning,
					Conditions: []corev1.PodCondition{},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   utils.PodConditionContainersPaused,
							Status: corev1.ConditionTrue,
							Reason: "ContainersPaused",
						},
					},
				},
			},
			expected:    true,
			description: "should return true when ContainersPaused condition is added",
		},
		{
			name: "ContainersResumed condition status changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   utils.PodConditionContainersResumed,
							Status: corev1.ConditionFalse,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   utils.PodConditionContainersResumed,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expected:    true,
			description: "should return true when ContainersResumed condition changes",
		},
		{
			name: "ContainersResumed condition reason and message changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:    utils.PodConditionContainersResumed,
							Status:  corev1.ConditionTrue,
							Reason:  "ResumeStarted",
							Message: "Resume in progress",
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:    utils.PodConditionContainersResumed,
							Status:  corev1.ConditionTrue,
							Reason:  "ResumeCompleted",
							Message: "Resume completed successfully",
						},
					},
				},
			},
			expected:    true,
			description: "should return true when ContainersResumed condition reason/message changes",
		},
		{
			name: "No changes - identical pods",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodReady,
							Status:  corev1.ConditionTrue,
							Reason:  "ContainersReady",
							Message: "All containers ready",
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodReady,
							Status:  corev1.ConditionTrue,
							Reason:  "ContainersReady",
							Message: "All containers ready",
						},
					},
				},
			},
			expected:    false,
			description: "should return false when nothing changes",
		},
		{
			name: "No changes - both have all conditions identical",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodReady,
							Status:  corev1.ConditionTrue,
							Reason:  "ContainersReady",
							Message: "Ready",
						},
						{
							Type:    utils.PodConditionContainersPaused,
							Status:  corev1.ConditionFalse,
							Reason:  "NotPaused",
							Message: "Containers not paused",
						},
						{
							Type:    utils.PodConditionContainersResumed,
							Status:  corev1.ConditionTrue,
							Reason:  "Resumed",
							Message: "Containers resumed",
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:    corev1.PodReady,
							Status:  corev1.ConditionTrue,
							Reason:  "ContainersReady",
							Message: "Ready",
						},
						{
							Type:    utils.PodConditionContainersPaused,
							Status:  corev1.ConditionFalse,
							Reason:  "NotPaused",
							Message: "Containers not paused",
						},
						{
							Type:    utils.PodConditionContainersResumed,
							Status:  corev1.ConditionTrue,
							Reason:  "Resumed",
							Message: "Containers resumed",
						},
					},
				},
			},
			expected:    false,
			description: "should return false when all tracked conditions are identical",
		},
		{
			name: "Untracked condition changed - should not trigger update",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   corev1.PodScheduled,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					PodIP: "10.0.0.1",
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   corev1.PodScheduled,
							Status: corev1.ConditionFalse, // Changed but not tracked
						},
					},
				},
			},
			expected:    false,
			description: "should return false when only untracked conditions change",
		},
		{
			name: "Multiple conditions but only tracked ones matter",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   corev1.PodInitialized,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   corev1.ContainersReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-pod",
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
						{
							Type:   corev1.PodInitialized,
							Status: corev1.ConditionFalse, // Changed but not tracked
						},
						{
							Type:   corev1.ContainersReady,
							Status: corev1.ConditionFalse, // Changed but not tracked
						},
					},
				},
			},
			expected:    false,
			description: "should ignore changes in non-tracked conditions",
		},
		{
			name: "ContainerStatuses - container added",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: true,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: true,
						},
						{
							Name:  "container2",
							Ready: true,
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container is added",
		},
		{
			name: "ContainerStatuses - container removed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: true,
						},
						{
							Name:  "container2",
							Ready: true,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: true,
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container is removed",
		},
		{
			name: "ContainerStatuses - container ready status changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: false,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: true,
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container ready status changes",
		},
		{
			name: "ContainerStatuses - container restart count changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "container1",
							Ready:        true,
							RestartCount: 0,
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "container1",
							Ready:        true,
							RestartCount: 1,
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container restart count increases",
		},
		{
			name: "ContainerStatuses - container state changed from Waiting to Running",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: false,
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "ContainerCreating",
								},
							},
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: true,
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container state changes from Waiting to Running",
		},
		{
			name: "ContainerStatuses - container state changed to Terminated",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: true,
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: false,
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{
									ExitCode: 1,
									Reason:   "Error",
								},
							},
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container terminates",
		},
		{
			name: "ContainerStatuses - container image changed (in-place upgrade)",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:    "container1",
							Ready:   true,
							Image:   "nginx:1.19",
							ImageID: "docker-pullable://nginx@sha256:old",
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:    "container1",
							Ready:   true,
							Image:   "nginx:1.20",
							ImageID: "docker-pullable://nginx@sha256:new",
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container image changes (in-place upgrade scenario)",
		},
		{
			name: "ContainerStatuses - container started status changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:    "container1",
							Ready:   false,
							Started: boolPtr(false),
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:    "container1",
							Ready:   true,
							Started: boolPtr(true),
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container started status changes",
		},
		{
			name: "ContainerStatuses - multiple containers with different changes",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "container1",
							Ready:        true,
							RestartCount: 0,
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
						{
							Name:         "container2",
							Ready:        true,
							RestartCount: 0,
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "container1",
							Ready:        true,
							RestartCount: 0,
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
						{
							Name:         "container2",
							Ready:        false,
							RestartCount: 1,
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason: "CrashLoopBackOff",
								},
							},
						},
					},
				},
			},
			expected:    true,
			description: "should return true when one of multiple containers changes",
		},
		{
			name: "ContainerStatuses - identical container statuses",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "container1",
							Ready:        true,
							RestartCount: 0,
							Image:        "nginx:1.19",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:         "container1",
							Ready:        true,
							RestartCount: 0,
							Image:        "nginx:1.19",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			expected:    false,
			description: "should return false when container statuses are identical",
		},
		{
			name: "ContainerStatuses - empty container statuses (both empty)",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase:             corev1.PodPending,
					ContainerStatuses: []corev1.ContainerStatus{},
				},
			},
			expected:    false,
			description: "should return false when both have empty container statuses",
		},
		{
			name: "ContainerStatuses - container state reason changed",
			oldPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: false,
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ImagePullBackOff",
									Message: "Back-off pulling image",
								},
							},
						},
					},
				},
			},
			newPod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  "container1",
							Ready: false,
							State: corev1.ContainerState{
								Waiting: &corev1.ContainerStateWaiting{
									Reason:  "ErrImagePull",
									Message: "Failed to pull image",
								},
							},
						},
					},
				},
			},
			expected:    true,
			description: "should return true when container waiting reason changes",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isActivePodUpdate(tt.oldPod, tt.newPod)
			assert.Equal(t, tt.expected, result, tt.description)
		})
	}
}

func TestIsPodConditionEqual(t *testing.T) {
	tests := []struct {
		name        string
		condA       *corev1.PodCondition
		condB       *corev1.PodCondition
		expected    bool
		description string
	}{
		{
			name:        "both nil - should be equal",
			condA:       nil,
			condB:       nil,
			expected:    true,
			description: "two nil conditions should be considered equal",
		},
		{
			name:  "first nil, second not nil",
			condA: nil,
			condB: &corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			},
			expected:    false,
			description: "nil and non-nil conditions should not be equal",
		},
		{
			name: "first not nil, second nil",
			condA: &corev1.PodCondition{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			},
			condB:       nil,
			expected:    false,
			description: "non-nil and nil conditions should not be equal",
		},
		{
			name: "identical conditions",
			condA: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "ContainersReady",
				Message: "All containers ready",
			},
			condB: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "ContainersReady",
				Message: "All containers ready",
			},
			expected:    true,
			description: "conditions with same Status, Reason, and Message should be equal",
		},
		{
			name: "different status",
			condA: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "ContainersReady",
				Message: "Ready",
			},
			condB: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionFalse,
				Reason:  "ContainersReady",
				Message: "Ready",
			},
			expected:    false,
			description: "conditions with different Status should not be equal",
		},
		{
			name: "different reason",
			condA: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "ContainersReady",
				Message: "Ready",
			},
			condB: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "PodCompleted",
				Message: "Ready",
			},
			expected:    false,
			description: "conditions with different Reason should not be equal",
		},
		{
			name: "different message",
			condA: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "ContainersReady",
				Message: "All containers ready",
			},
			condB: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "ContainersReady",
				Message: "Containers are ready",
			},
			expected:    false,
			description: "conditions with different Message should not be equal",
		},
		{
			name: "same Status, Reason, Message but different Type (not compared)",
			condA: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "Ready",
				Message: "Pod is ready",
			},
			condB: &corev1.PodCondition{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionTrue,
				Reason:  "Ready",
				Message: "Pod is ready",
			},
			expected:    true,
			description: "Type field is not compared, only Status, Reason, and Message",
		},
		{
			name: "same Status, Reason, Message but different LastTransitionTime (not compared)",
			condA: &corev1.PodCondition{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				Reason:             "Ready",
				Message:            "Pod is ready",
				LastTransitionTime: metav1.Now(),
			},
			condB: &corev1.PodCondition{
				Type:               corev1.PodReady,
				Status:             corev1.ConditionTrue,
				Reason:             "Ready",
				Message:            "Pod is ready",
				LastTransitionTime: metav1.Time{},
			},
			expected:    true,
			description: "LastTransitionTime is not compared",
		},
		{
			name: "empty strings should be equal",
			condA: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "",
				Message: "",
			},
			condB: &corev1.PodCondition{
				Type:    corev1.PodReady,
				Status:  corev1.ConditionTrue,
				Reason:  "",
				Message: "",
			},
			expected:    true,
			description: "conditions with empty Reason and Message should be equal",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isPodConditionEqual(tt.condA, tt.condB)
			assert.Equal(t, tt.expected, result, tt.description)
		})
	}
}
