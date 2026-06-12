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
	cases := []struct {
		uid      string
		expected string
	}{
		{"abc123", "agent-job-abc123"},
		{"abc-123", "agent-job-abc123"},
		{"abc-123-def-456", "agent-job-abc123def456"},
		{"", "agent-job-"},
	}
	for _, c := range cases {
		if got := MakeJobName(c.uid); got != c.expected {
			t.Errorf("MakeJobName(%q)=%q, want %q", c.uid, got, c.expected)
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
		got, ok := CommitJobExitCodeMap[code]
		if !ok {
			t.Errorf("exit code %d missing from CommitJobExitCodeMap", code)
			continue
		}
		if got.conditionType != want.typ || got.conditionReason != want.reason {
			t.Errorf("exit code %d got (%q,%q), want (%q,%q)",
				code, got.conditionType, got.conditionReason, want.typ, want.reason)
		}
	}
}

func TestGetCommitCondition_Success(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent-job",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: ExitCodeSuccess},
					},
				},
			},
		},
	}
	cond := GetCommitCondition(pod)
	if cond == nil {
		t.Fatal("expected non-nil condition for success exit code")
	}
	if cond.Type != "PushCommittedImage" || cond.Reason != "PushCommittedImageSuccess" {
		t.Errorf("unexpected condition: %+v", cond)
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("expected Status=True for exit code 0, got %s", cond.Status)
	}
}

func TestGetCommitCondition_Failure(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent-job",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: ExitCodeCommitFailed},
					},
				},
			},
		},
	}
	cond := GetCommitCondition(pod)
	if cond == nil {
		t.Fatal("expected non-nil condition for failure exit code")
	}
	if cond.Status != metav1.ConditionFalse {
		t.Errorf("expected Status=False for non-zero exit code, got %s", cond.Status)
	}
	if cond.Type != "CommitContainer" || cond.Reason != "CommitContainerFailed" {
		t.Errorf("unexpected condition: %+v", cond)
	}
}

// TestGetCommitCondition_UnknownExitCode covers the early-return branch when the
// container's terminated exit code is not registered in CommitJobExitCodeMap.
func TestGetCommitCondition_UnknownExitCode(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent-job",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{ExitCode: 999},
					},
				},
			},
		},
	}
	if cond := GetCommitCondition(pod); cond != nil {
		t.Errorf("expected nil condition for unknown exit code, got %+v", cond)
	}
}

func TestGetCommitCondition_NotTerminated(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "agent-job",
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: metav1.Now()},
					},
				},
			},
		},
	}
	if cond := GetCommitCondition(pod); cond != nil {
		t.Errorf("expected nil condition for running container, got %+v", cond)
	}
}

func TestGetCommitCondition_EmptyContainerStatuses(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{}}
	if cond := GetCommitCondition(pod); cond != nil {
		t.Errorf("expected nil condition for pod without container statuses, got %+v", cond)
	}
}
