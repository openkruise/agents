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

package job

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMakeJobName(t *testing.T) {
	tests := []struct {
		uid      string
		expected string
	}{
		{"abc-def-123", "agent-job-abcdef123"},
		{"nohyphens", "agent-job-nohyphens"},
		{"", "agent-job-"},
		{"a-b-c-d", "agent-job-abcd"},
	}
	for _, tt := range tests {
		got := MakeJobName(tt.uid)
		if got != tt.expected {
			t.Errorf("MakeJobName(%q) = %q, want %q", tt.uid, got, tt.expected)
		}
	}
}

func TestIsJobCompleted(t *testing.T) {
	tests := []struct {
		name       string
		conditions []batchv1.JobCondition
		wantDone   bool
		wantOK     bool
	}{
		{
			name:     "no conditions (still running)",
			wantDone: false,
			wantOK:   false,
		},
		{
			name: "job completed successfully",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
			wantDone: true,
			wantOK:   true,
		},
		{
			name: "job failed",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
			wantDone: true,
			wantOK:   false,
		},
		{
			name: "job complete=false (not done yet)",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
			},
			wantDone: false,
			wantOK:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			job := &batchv1.Job{
				Status: batchv1.JobStatus{Conditions: tt.conditions},
			}
			done, ok := IsJobCompleted(job)
			if done != tt.wantDone || ok != tt.wantOK {
				t.Errorf("IsJobCompleted() = (%v, %v), want (%v, %v)", done, ok, tt.wantDone, tt.wantOK)
			}
		})
	}
}

func TestGetCommitCondition(t *testing.T) {
	tests := []struct {
		name           string
		pod            *corev1.Pod
		expectNil      bool
		expectType     string
		expectStatus   metav1.ConditionStatus
		expectReason   string
	}{
		{
			name: "exit code 0 (success)",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{ExitCode: 0},
							},
						},
					},
				},
			},
			expectType:   "PushCommittedImage",
			expectStatus: metav1.ConditionTrue,
			expectReason: "PushCommittedImageSuccess",
		},
		{
			name: "exit code 1 (commit failed)",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
							},
						},
					},
				},
			},
			expectType:   "CommitContainer",
			expectStatus: metav1.ConditionFalse,
			expectReason: "CommitContainerFailed",
		},
		{
			name: "exit code 2 (push failed)",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{ExitCode: 2},
							},
						},
					},
				},
			},
			expectType:   "PushCommittedImage",
			expectStatus: metav1.ConditionFalse,
			expectReason: "PushCommittedImageFailed",
		},
		{
			name: "no terminated container",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
						},
					},
				},
			},
			expectNil: true,
		},
		{
			name: "no container statuses",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{},
			},
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := GetCommitCondition(tt.pod)
			if tt.expectNil {
				if cond != nil {
					t.Errorf("expected nil condition, got %+v", cond)
				}
				return
			}
			if cond == nil {
				t.Fatal("expected non-nil condition")
			}
			if cond.Type != tt.expectType {
				t.Errorf("Type = %s, want %s", cond.Type, tt.expectType)
			}
			if cond.Status != tt.expectStatus {
				t.Errorf("Status = %s, want %s", cond.Status, tt.expectStatus)
			}
			if cond.Reason != tt.expectReason {
				t.Errorf("Reason = %s, want %s", cond.Reason, tt.expectReason)
			}
		})
	}
}
