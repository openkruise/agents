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

package sandboxutils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestGetSandboxState(t *testing.T) {
	now := metav1.Now()
	pastTime := metav1.NewTime(now.Add(-time.Hour))
	futureTime := metav1.NewTime(now.Add(time.Hour))

	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		expectedState  string
		expectedReason string
	}{
		{
			name: "Sandbox with DeletionTimestamp",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ResourceDeleted",
		},
		{
			name: "Sandbox with expired ShutdownTime",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &pastTime,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ShutdownTimeReached",
		},
		{
			name: "Sandbox with future ShutdownTime",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{},
				Spec: agentsv1alpha1.SandboxSpec{
					ShutdownTime: &futureTime,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateRunning,
			expectedReason: "RunningResourceClaimedAndReady",
		},
		{
			name: "Sandbox in Pending phase",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateCreating,
			expectedReason: "ResourcePending",
		},
		{
			name: "Sandbox in Succeeded phase",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxSucceeded,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ResourceSucceeded",
		},
		{
			name: "Sandbox in Failed phase",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxFailed,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ResourceFailed",
		},
		{
			name: "Sandbox in Terminating phase",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxTerminating,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "ResourceTerminating",
		},
		{
			name: "Sandbox controlled by SandboxSet and Ready",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.kruise.io/v1alpha1",
							Kind:       "SandboxSet",
							Controller: &[]bool{true}[0],
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateAvailable,
			expectedReason: "ResourceControlledBySbsAndReady",
		},
		{
			name: "Sandbox controlled by SandboxSet but not Ready",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.kruise.io/v1alpha1",
							Kind:       "SandboxSet",
							Controller: &[]bool{true}[0],
						},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionFalse,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateCreating,
			expectedReason: "ResourceControlledBySbsButNotReady",
		},
		{
			name: "Running Sandbox claimed and Ready",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					}},
			},
			expectedState:  agentsv1alpha1.SandboxStateRunning,
			expectedReason: "RunningResourceClaimedAndReady",
		},
		{
			name: "Running Sandbox claimed but not Ready and Paused",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionFalse,
						},
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStatePaused,
			expectedReason: "RunningResourceClaimedAndPaused",
		},
		{
			name: "Running Sandbox claimed but not Ready and not Paused",
			sandbox: &agentsv1alpha1.Sandbox{
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionFalse,
						},
					},
				},
			},
			expectedState:  agentsv1alpha1.SandboxStateDead,
			expectedReason: "RunningResourceClaimedButNotReady",
		},
		{
			name: "Not Running Sandbox claimed",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
				},
			},
			expectedState:  agentsv1alpha1.SandboxStatePaused,
			expectedReason: "NotRunningResourceClaimed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state, reason := GetSandboxState(tt.sandbox)
			assert.Equal(t, tt.expectedState, state)
			assert.Equal(t, tt.expectedReason, reason)
		})
	}
}

func TestIsControlledBySandboxCR(t *testing.T) {
	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected bool
	}{
		{
			name: "Sandbox controlled by SandboxSet",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.kruise.io/v1alpha1",
							Kind:       "SandboxSet",
							Controller: &[]bool{true}[0],
						},
					},
				},
			},
			expected: true,
		},
		{
			name: "Sandbox not controlled by anything",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{},
				},
			},
			expected: false,
		},
		{
			name: "Sandbox controlled by non-SandboxSet resource",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "apps/v1",
							Kind:       "Deployment",
							Controller: &[]bool{true}[0],
						},
					},
				},
			},
			expected: false,
		},
		{
			name: "Sandbox with nil controller reference",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					OwnerReferences: []metav1.OwnerReference{
						{
							APIVersion: "agents.kruise.io/v1alpha1",
							Kind:       "SandboxSet",
							Controller: nil,
						},
					},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsControlledBySandboxSet(tt.sandbox)
			if result != tt.expected {
				t.Errorf("IsControlledBySandboxSet() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestGetSandboxID(t *testing.T) {
	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected string
	}{
		{
			name: "Standard namespace and name",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-namespace",
					Name:      "test-name",
				},
			},
			expected: "test-namespace--test-name",
		},
		{
			name: "Empty namespace",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "",
					Name:      "test-name",
				},
			},
			expected: "--test-name",
		},
		{
			name: "Empty name",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "test-namespace",
					Name:      "",
				},
			},
			expected: "test-namespace--",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := GetSandboxID(tt.sandbox)
			if result != tt.expected {
				t.Errorf("GetSandboxID() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestIsSandboxPausable(t *testing.T) {
	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		expectedResult bool
		expectedReason string
	}{
		{
			name: "Running sandbox is pausable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsRunningOrPaused",
		},
		{
			name: "Paused sandbox is pausable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsRunningOrPaused",
		},
		{
			name: "Pending sandbox is not pausable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPending,
				},
			},
			expectedResult: false,
			expectedReason: "SandboxPhaseNotAllowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, reason := IsSandboxPausable(tt.sandbox)
			assert.Equal(t, tt.expectedResult, result)
			assert.Equal(t, tt.expectedReason, reason)
		})
	}
}

func TestIsSandboxResumable(t *testing.T) {
	tests := []struct {
		name           string
		sandbox        *agentsv1alpha1.Sandbox
		expectedResult bool
		expectedReason string
	}{
		{
			name: "Running sandbox is resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsRunningOrResuming",
		},
		{
			name: "Resuming sandbox is resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxResuming,
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsRunningOrResuming",
		},
		{
			name: "Paused sandbox with paused condition is resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionPaused),
							Status: metav1.ConditionTrue,
						},
					},
				},
			},
			expectedResult: true,
			expectedReason: "SandboxIsPaused",
		},
		{
			name: "Paused sandbox without paused condition is not resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxPaused,
				},
			},
			expectedResult: false,
			expectedReason: "SandboxIsPausing",
		},
		{
			name: "Succeeded sandbox is not resumable",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxSucceeded,
				},
			},
			expectedResult: false,
			expectedReason: "SandboxPhaseNotAllowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, reason := IsSandboxResumable(tt.sandbox)
			assert.Equal(t, tt.expectedResult, result)
			assert.Equal(t, tt.expectedReason, reason)
		})
	}
}
