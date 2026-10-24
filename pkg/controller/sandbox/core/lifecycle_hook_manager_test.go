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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func lifecycleHookTestSandbox() *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "default", UID: types.UID("sandbox-uid"), Generation: 3},
		Spec: agentsv1alpha1.SandboxSpec{Lifecycle: &agentsv1alpha1.SandboxLifecycle{
			PrePause:   &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"flush"}}},
			PostResume: &agentsv1alpha1.LifecycleAction{Exec: &corev1.ExecAction{Command: []string{"restore"}}},
		}},
	}
}

func hookCondition(t *testing.T, status *agentsv1alpha1.SandboxStatus, conditionType agentsv1alpha1.SandboxConditionType) *metav1.Condition {
	t.Helper()
	cond := utils.GetSandboxCondition(status, string(conditionType))
	require.NotNil(t, cond)
	return cond
}

func TestLifecycleHookManagerBeginPrePause(t *testing.T) {
	tests := []struct {
		name                 string
		postResumeReason     string
		expectPostResumeKept bool
	}{
		{name: "clears pending post resume", postResumeReason: agentsv1alpha1.SandboxLifecycleHookReasonPending},
		{name: "clears running post resume", postResumeReason: agentsv1alpha1.SandboxLifecycleHookReasonRunning},
		{name: "retains successful post resume", postResumeReason: agentsv1alpha1.SandboxLifecycleHookReasonSucceeded, expectPostResumeKept: true},
		{name: "retains failed post resume", postResumeReason: agentsv1alpha1.SandboxLifecycleHookReasonFailed, expectPostResumeKept: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewLifecycleHookManager(1, nil, nil)
			box := lifecycleHookTestSandbox()
			status := &agentsv1alpha1.SandboxStatus{}
			utils.SetSandboxCondition(status, newLifecycleHookCondition(agentsv1alpha1.SandboxConditionPrePauseHook,
				agentsv1alpha1.SandboxLifecycleHookReasonSucceeded, metav1.ConditionTrue, box.Generation, "old result"))
			utils.SetSandboxCondition(status, newLifecycleHookCondition(agentsv1alpha1.SandboxConditionPostResumeHook,
				tt.postResumeReason, metav1.ConditionFalse, box.Generation, "post resume result"))

			manager.BeginPrePause(box, status)

			assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonPending,
				hookCondition(t, status, agentsv1alpha1.SandboxConditionPrePauseHook).Reason)
			postResumeCond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPostResumeHook))
			if tt.expectPostResumeKept {
				require.NotNil(t, postResumeCond)
				assert.Equal(t, tt.postResumeReason, postResumeCond.Reason)
			} else {
				assert.Nil(t, postResumeCond)
			}
		})
	}
}

func TestLifecycleHookManagerBeginPostResumeCycle(t *testing.T) {
	tests := []struct {
		name       string
		configured bool
	}{
		{name: "configured hook starts pending", configured: true},
		{name: "missing hook leaves no condition"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewLifecycleHookManager(1, nil, nil)
			box := lifecycleHookTestSandbox()
			if !tt.configured {
				box.Spec.Lifecycle.PostResume = nil
			}
			status := &agentsv1alpha1.SandboxStatus{}
			utils.SetSandboxCondition(status, newLifecycleHookCondition(agentsv1alpha1.SandboxConditionPrePauseHook,
				agentsv1alpha1.SandboxLifecycleHookReasonSucceeded, metav1.ConditionTrue, box.Generation, "pre pause result"))
			utils.SetSandboxCondition(status, newLifecycleHookCondition(agentsv1alpha1.SandboxConditionPostResumeHook,
				agentsv1alpha1.SandboxLifecycleHookReasonFailed, metav1.ConditionFalse, box.Generation, "old result"))

			manager.BeginPostResume(box, status)

			// The terminal PrePause result is intentionally retained as the
			// record of the completed pause (see the comment in BeginPostResume).
			assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonSucceeded,
				hookCondition(t, status, agentsv1alpha1.SandboxConditionPrePauseHook).Reason)
			postResumeCond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPostResumeHook))
			if tt.configured {
				require.NotNil(t, postResumeCond)
				assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonPending, postResumeCond.Reason)
			} else {
				assert.Nil(t, postResumeCond)
			}
		})
	}
}

func TestLifecycleHookManagerClearActivePostResume(t *testing.T) {
	tests := []struct {
		name       string
		reason     string
		expectKept bool
	}{
		{name: "pending is cleared", reason: agentsv1alpha1.SandboxLifecycleHookReasonPending},
		{name: "running is cleared", reason: agentsv1alpha1.SandboxLifecycleHookReasonRunning},
		{name: "succeeded is retained", reason: agentsv1alpha1.SandboxLifecycleHookReasonSucceeded, expectKept: true},
		{name: "failed is retained", reason: agentsv1alpha1.SandboxLifecycleHookReasonFailed, expectKept: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewLifecycleHookManager(1, nil, nil)
			box := lifecycleHookTestSandbox()
			status := &agentsv1alpha1.SandboxStatus{}
			utils.SetSandboxCondition(status, newLifecycleHookCondition(agentsv1alpha1.SandboxConditionPostResumeHook,
				tt.reason, metav1.ConditionFalse, box.Generation, "result"))

			manager.ClearActivePostResume(box, status)

			cond := utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPostResumeHook))
			if tt.expectKept {
				require.NotNil(t, cond)
				assert.Equal(t, tt.reason, cond.Reason)
			} else {
				assert.Nil(t, cond)
			}
		})
	}
}

func TestLifecycleHookManagerTerminalConditionDoesNotExecute(t *testing.T) {
	tests := []struct {
		name   string
		reason string
		status metav1.ConditionStatus
	}{
		{name: "succeeded", reason: agentsv1alpha1.SandboxLifecycleHookReasonSucceeded, status: metav1.ConditionTrue},
		{name: "failed", reason: agentsv1alpha1.SandboxLifecycleHookReasonFailed, status: metav1.ConditionFalse},
		{name: "skipped", reason: agentsv1alpha1.SandboxLifecycleHookReasonSkipped, status: metav1.ConditionFalse},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			manager := NewLifecycleHookManager(1, func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.LifecycleAction) (int32, string, string, error) {
				calls++
				return 0, "", "", nil
			}, nil)
			box := lifecycleHookTestSandbox()
			status := &agentsv1alpha1.SandboxStatus{}
			utils.SetSandboxCondition(status, newLifecycleHookCondition(agentsv1alpha1.SandboxConditionPrePauseHook,
				tt.reason, tt.status, box.Generation, "terminal result"))

			progress := manager.ReconcilePrePause(t.Context(), box, &corev1.Pod{}, status)
			assert.True(t, progress.Terminal)
			assert.False(t, progress.ConditionChanged)
			assert.Zero(t, calls)
		})
	}
}

func TestLifecycleHookManagerRemovedActionClearsProgress(t *testing.T) {
	manager := NewLifecycleHookManager(1, nil, nil)
	box := lifecycleHookTestSandbox()
	status := &agentsv1alpha1.SandboxStatus{}
	manager.BeginPrePause(box, status)
	box.Spec.Lifecycle.PrePause = nil

	progress := manager.ReconcilePrePause(t.Context(), box, &corev1.Pod{}, status)
	assert.True(t, progress.Terminal)
	assert.True(t, progress.ConditionChanged)
	assert.Nil(t, utils.GetSandboxCondition(status, string(agentsv1alpha1.SandboxConditionPrePauseHook)))
}

func TestLifecycleHookFailureSummary(t *testing.T) {
	tests := []struct {
		name     string
		exitCode int32
		err      error
		want     string
	}{
		{name: "timeout", exitCode: -1, err: context.DeadlineExceeded, want: "timeout"},
		{name: "execution error", exitCode: -1, err: errors.New("connection reset"), want: "execution error"},
		{name: "exit code", exitCode: 17, want: "process exited with code 17"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, lifecycleHookFailureSummary(tt.exitCode, tt.err))
		})
	}
}

func TestLifecycleHookManagerSuccess(t *testing.T) {
	calls := 0
	manager := NewLifecycleHookManager(1, func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.LifecycleAction) (int32, string, string, error) {
		calls++
		return 0, "secret output", "", nil
	}, record.NewFakeRecorder(10))
	box := lifecycleHookTestSandbox()
	status := &agentsv1alpha1.SandboxStatus{}
	pod := &corev1.Pod{}

	manager.BeginPrePause(box, status)
	progress := manager.ReconcilePrePause(t.Context(), box, pod, status)
	assert.True(t, progress.ConditionChanged)
	assert.Equal(t, 0, calls)
	assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonRunning,
		hookCondition(t, status, agentsv1alpha1.SandboxConditionPrePauseHook).Reason)

	progress = manager.ReconcilePrePause(t.Context(), box, pod, status)
	assert.True(t, progress.Terminal)
	assert.Equal(t, 1, calls)
	cond := hookCondition(t, status, agentsv1alpha1.SandboxConditionPrePauseHook)
	assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonSucceeded, cond.Reason)
	assert.NotContains(t, cond.Message, "secret")
	assert.Equal(t, box.Generation, cond.ObservedGeneration)
}

func TestLifecycleHookManagerFailureIsTerminalAndPassesThrough(t *testing.T) {
	calls := 0
	manager := NewLifecycleHookManager(1, func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.LifecycleAction) (int32, string, string, error) {
		calls++
		return 7, "token=secret", "password=secret", nil
	}, record.NewFakeRecorder(10))
	box := lifecycleHookTestSandbox()
	status := &agentsv1alpha1.SandboxStatus{}

	manager.BeginPostResume(box, status)
	manager.reconcilePostResume(t.Context(), box, &corev1.Pod{}, status) // persist Running
	progress := manager.reconcilePostResume(t.Context(), box, &corev1.Pod{}, status)

	assert.True(t, progress.Terminal)
	assert.Equal(t, 1, calls, "a failed hook must not be retried")
	cond := hookCondition(t, status, agentsv1alpha1.SandboxConditionPostResumeHook)
	assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonFailed, cond.Reason)
	assert.Contains(t, cond.Message, "process exited with code 7")
	assert.Contains(t, cond.Message, "lifecycle transition continues")
	assert.NotContains(t, cond.Message, "secret")

	// A subsequent reconcile observes the terminal condition and executes nothing.
	progress = manager.reconcilePostResume(t.Context(), box, &corev1.Pod{}, status)
	assert.True(t, progress.Terminal)
	assert.False(t, progress.ConditionChanged)
	assert.Equal(t, 1, calls)
}

func TestLifecycleHookManagerNoSlotDoesNotExecute(t *testing.T) {
	manager := NewLifecycleHookManager(1, func(context.Context, *agentsv1alpha1.Sandbox, *agentsv1alpha1.LifecycleAction) (int32, string, string, error) {
		t.Fatal("hook must not run without a slot")
		return 0, "", "", nil
	}, nil)
	manager.slots <- struct{}{}
	defer func() { <-manager.slots }()
	box := lifecycleHookTestSandbox()
	status := &agentsv1alpha1.SandboxStatus{}
	manager.BeginPrePause(box, status)
	manager.ReconcilePrePause(t.Context(), box, &corev1.Pod{}, status)
	progress := manager.ReconcilePrePause(t.Context(), box, &corev1.Pod{}, status)
	assert.Equal(t, lifecycleHookNoSlotRequeue, progress.RequeueAfter)
}

func TestLifecycleHookManagerPrePauseSkipsMissingPod(t *testing.T) {
	manager := NewLifecycleHookManager(1, nil, nil)
	box := lifecycleHookTestSandbox()
	status := &agentsv1alpha1.SandboxStatus{}
	manager.BeginPrePause(box, status)
	progress := manager.ReconcilePrePause(t.Context(), box, nil, status)
	assert.True(t, progress.Terminal)
	assert.Equal(t, agentsv1alpha1.SandboxLifecycleHookReasonSkipped,
		hookCondition(t, status, agentsv1alpha1.SandboxConditionPrePauseHook).Reason)
}
