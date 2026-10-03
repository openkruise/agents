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
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMakeJobName(t *testing.T) {
	cases := []struct {
		name     string
		expected string
	}{
		{"my-snapshot", "commit-my-snapshot-"},
		{"", "commit--"},
		{strings.Repeat("a", 100), "commit-" + strings.Repeat("a", 50) + "-"},
	}
	for _, c := range cases {
		if got := MakeJobName(c.name); got != c.expected {
			t.Errorf("MakeJobName(%q)=%q, want %q", c.name, got, c.expected)
		}
	}
}

func TestIsJobCompleted(t *testing.T) {
	cases := []struct {
		name        string
		conditions  []batchv1.JobCondition
		wantDone    bool
		wantSuccess bool
	}{
		{
			name: "complete=true",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
			},
			wantDone: true, wantSuccess: true,
		},
		{
			name: "failed=true",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
			},
			wantDone: true, wantSuccess: false,
		},
		{
			name: "complete=false (still running)",
			conditions: []batchv1.JobCondition{
				{Type: batchv1.JobComplete, Status: corev1.ConditionFalse},
			},
			wantDone: false, wantSuccess: false,
		},
		{
			name:       "no conditions",
			conditions: nil,
			wantDone:   false, wantSuccess: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			job := &batchv1.Job{Status: batchv1.JobStatus{Conditions: c.conditions}}
			done, success := IsJobCompleted(job)
			if done != c.wantDone || success != c.wantSuccess {
				t.Errorf("IsJobCompleted got (done=%v, success=%v), want (done=%v, success=%v)",
					done, success, c.wantDone, c.wantSuccess)
			}
		})
	}
}

func TestCommitJobExitCodeMap(t *testing.T) {
	// All registered exit codes must map to a non-empty conditionType + conditionReason.
	expected := map[int32]struct{ typ, reason string }{
		ExitCodeSuccess:              {"PushCommittedImage", "PushCommittedImageSuccess"},
		ExitCodeCommitFailed:         {"CommitContainer", "CommitContainerFailed"},
		ExitCodeGetImageSizeFailed:   {"CommitContainer", "GetImageSizeFailed"},
		ExitCodeParseImageSizeFailed: {"CommitContainer", "ParseImageSizeFailed"},
		ExitCodePushFailed:           {"PushCommittedImage", "PushCommittedImageFailed"},
		ExitCodeGetSandboxIDFailed:   {"CommitContainer", "GetSandboxIDFailed"},
	}
	for code, want := range expected {
		got, ok := commitJobExitCodeMap[code]
		if !ok {
			t.Errorf("exit code %d missing from commitJobExitCodeMap", code)
			continue
		}
		if got.conditionType != want.typ || got.conditionReason != want.reason {
			t.Errorf("exit code %d got (%q,%q), want (%q,%q)",
				code, got.conditionType, got.conditionReason, want.typ, want.reason)
		}
	}
}

func TestGetCommitCondition(t *testing.T) {
	tests := []struct {
		name          string
		containerName string
		exitCode      int32
		terminated    bool
		expectNil     bool
		expectType    string
		expectStatus  metav1.ConditionStatus
		expectReason  string
	}{
		{
			name:          "exit code 0 - success",
			containerName: "commit-job",
			exitCode:      ExitCodeSuccess,
			terminated:    true,
			expectNil:     false,
			expectType:    "PushCommittedImage",
			expectStatus:  metav1.ConditionTrue,
			expectReason:  "PushCommittedImageSuccess",
		},
		{
			name:          "exit code 1 - commit failed",
			containerName: "commit-job",
			exitCode:      ExitCodeCommitFailed,
			terminated:    true,
			expectNil:     false,
			expectType:    "CommitContainer",
			expectStatus:  metav1.ConditionFalse,
			expectReason:  "CommitContainerFailed",
		},
		{
			name:          "unknown exit code records generic CommitJob condition",
			containerName: "commit-job",
			exitCode:      999,
			terminated:    true,
			expectNil:     false,
			expectType:    "CommitJob",
			expectStatus:  metav1.ConditionFalse,
			expectReason:  "UnknownExitCode",
		},
		{
			name:          "running container returns nil",
			containerName: "commit-job",
			terminated:    false,
			expectNil:     true,
		},
		{
			name:          "wrong container name returns nil",
			containerName: "other-container",
			exitCode:      ExitCodeSuccess,
			terminated:    true,
			expectNil:     true,
		},
		{
			name:          "empty container statuses returns nil",
			containerName: "",
			terminated:    false,
			expectNil:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Status: corev1.PodStatus{}}
			if tt.containerName != "" {
				cs := corev1.ContainerStatus{Name: tt.containerName}
				if tt.terminated {
					cs.State = corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: tt.exitCode},
					}
				} else {
					cs.State = corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()},
					}
				}
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
			}

			cond := GetCommitCondition(context.TODO(), pod)
			if tt.expectNil {
				if cond != nil {
					t.Errorf("expected nil condition, got %+v", cond)
				}
				return
			}
			if cond == nil {
				t.Fatal("expected non-nil condition, got nil")
			}
			if cond.Type != tt.expectType {
				t.Errorf("expected type %q, got %q", tt.expectType, cond.Type)
			}
			if cond.Status != tt.expectStatus {
				t.Errorf("expected status %q, got %q", tt.expectStatus, cond.Status)
			}
			if cond.Reason != tt.expectReason {
				t.Errorf("expected reason %q, got %q", tt.expectReason, cond.Reason)
			}
		})
	}
}

func TestJobTerminalTime(t *testing.T) {
	now := metav1.Now()
	older := metav1.NewTime(now.Add(-2 * time.Minute))
	completionTime := metav1.NewTime(now.Add(-time.Minute))

	tests := []struct {
		name string
		job  *batchv1.Job
		want time.Time
	}{
		{
			name: "completion time takes precedence",
			job: &batchv1.Job{Status: batchv1.JobStatus{
				CompletionTime: &completionTime,
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: older},
				},
			}},
			want: completionTime.Time,
		},
		{
			name: "falls back to complete condition time",
			job: &batchv1.Job{Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: older},
				},
			}},
			want: older.Time,
		},
		{
			name: "falls back to failed condition time",
			job: &batchv1.Job{Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, LastTransitionTime: older},
				},
			}},
			want: older.Time,
		},
		{
			name: "zero when condition not true",
			job: &batchv1.Job{Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionFalse, LastTransitionTime: older},
				},
			}},
			want: time.Time{},
		},
		{
			name: "zero when no terminal markers",
			job:  &batchv1.Job{Status: batchv1.JobStatus{Active: 1}},
			want: time.Time{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JobTerminalTime(tt.job); !got.Equal(tt.want) {
				t.Errorf("JobTerminalTime()=%v, want %v", got, tt.want)
			}
		})
	}
}

func TestFallbackCommitCondition(t *testing.T) {
	tests := []struct {
		name         string
		job          *batchv1.Job
		expectType   string
		expectStatus metav1.ConditionStatus
		expectReason string
		expectMsg    string
	}{
		{
			name: "completed job maps to push success",
			job: &batchv1.Job{Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobComplete, Status: corev1.ConditionTrue},
				},
			}},
			expectType:   "PushCommittedImage",
			expectStatus: metav1.ConditionTrue,
			expectReason: "PushCommittedImageSuccess",
			expectMsg:    "Commit job completed; container exit code was not observable",
		},
		{
			name: "failed job without message uses generic message",
			job: &batchv1.Job{Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue},
				},
			}},
			expectType:   "CommitJob",
			expectStatus: metav1.ConditionFalse,
			expectReason: "CommitJobFailed",
			expectMsg:    "Commit job failed; container exit code was not observable",
		},
		{
			name: "failed job propagates job condition message",
			job: &batchv1.Job{Status: batchv1.JobStatus{
				Conditions: []batchv1.JobCondition{
					{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "Job was active longer than specified deadline"},
				},
			}},
			expectType:   "CommitJob",
			expectStatus: metav1.ConditionFalse,
			expectReason: "CommitJobFailed",
			expectMsg:    "Job was active longer than specified deadline",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cond := FallbackCommitCondition(tt.job)
			if cond == nil {
				t.Fatal("expected non-nil condition, got nil")
			}
			if cond.Type != tt.expectType {
				t.Errorf("expected type %q, got %q", tt.expectType, cond.Type)
			}
			if cond.Status != tt.expectStatus {
				t.Errorf("expected status %q, got %q", tt.expectStatus, cond.Status)
			}
			if cond.Reason != tt.expectReason {
				t.Errorf("expected reason %q, got %q", tt.expectReason, cond.Reason)
			}
			if cond.Message != tt.expectMsg {
				t.Errorf("expected message %q, got %q", tt.expectMsg, cond.Message)
			}
			if cond.LastTransitionTime.IsZero() {
				t.Error("expected LastTransitionTime to be set")
			}
		})
	}
}
