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

package cache_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
)

func makePausedSandbox(name string, paused corev1.ConditionStatus) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPaused,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionStatus(paused),
				},
			},
		},
	}
}

func TestNewSandboxPauseTask_BoundAction(t *testing.T) {
	sbx := makePausedSandbox("sbx-pause-1", corev1.ConditionTrue)
	c, _, err := cachetest.NewTestCache(t, sbx)
	require.NoError(t, err)
	task, err := c.NewSandboxPauseTask(context.Background(), sbx)
	require.NoError(t, err)
	defer task.Release()

	assert.Equal(t, cacheutils.WaitActionPause, task.Action())
	assert.Same(t, sbx, task.Object())

	key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	_, ok := c.GetWaitHooks().Load(key)
	assert.True(t, ok)

	// Already paused means Wait returns nil immediately and releases the hook.
	assert.NoError(t, task.Wait(100*time.Millisecond))
	_, ok = c.GetWaitHooks().Load(key)
	assert.False(t, ok)
}

func TestNewSandboxResumeTask_BoundAction(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sbx-resume-1"},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
	}
	c, _, err := cachetest.NewTestCache(t, sbx)
	require.NoError(t, err)
	task, err := c.NewSandboxResumeTask(context.Background(), sbx)
	require.NoError(t, err)
	defer task.Release()

	assert.Equal(t, cacheutils.WaitActionResume, task.Action())
	assert.Same(t, sbx, task.Object())

	key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	_, ok := c.GetWaitHooks().Load(key)
	assert.True(t, ok)

	// Running state means Wait returns nil immediately and releases the hook.
	assert.NoError(t, task.Wait(100*time.Millisecond))
	_, ok = c.GetWaitHooks().Load(key)
	assert.False(t, ok)
}

func TestNewSandboxWaitReadyTask_BoundAction(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sbx-wait-1", Generation: 1},
		Status: agentsv1alpha1.SandboxStatus{
			ObservedGeneration: 1,
			Phase:              agentsv1alpha1.SandboxRunning,
			PodInfo:            agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
		},
	}
	c, _, err := cachetest.NewTestCache(t, sbx)
	require.NoError(t, err)
	task := c.NewSandboxWaitReadyTask(context.Background(), sbx)
	assert.Equal(t, cacheutils.WaitActionWaitReady, task.Action())
	// Ready → satisfied fast path.
	assert.NoError(t, task.Wait(100*time.Millisecond))
}

func TestNewSandboxWaitReadyTask_StartContainerFailed_ReturnsError(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "sbx-wait-2", Generation: 1},
		Status: agentsv1alpha1.SandboxStatus{
			ObservedGeneration: 1,
			Conditions: []metav1.Condition{
				{
					Type:    string(agentsv1alpha1.SandboxConditionReady),
					Status:  metav1.ConditionFalse,
					Reason:  agentsv1alpha1.SandboxReadyReasonStartContainerFailed,
					Message: "OCI create failed",
				},
			},
		},
	}
	c, _, err := cachetest.NewTestCache(t, sbx)
	require.NoError(t, err)
	task := c.NewSandboxWaitReadyTask(context.Background(), sbx)
	err = task.Wait(100 * time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "start container failed")
}

func TestNewCheckpointTask_Succeeded(t *testing.T) {
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cp-1"},
		Status: agentsv1alpha1.CheckpointStatus{
			Phase:        agentsv1alpha1.CheckpointSucceeded,
			CheckpointId: "ckpt-abc",
		},
	}
	c, _, err := cachetest.NewTestCache(t, cp)
	require.NoError(t, err)
	task := c.NewCheckpointTask(context.Background(), cp)
	assert.Equal(t, cacheutils.WaitActionCheckpoint, task.Action())
	assert.NoError(t, task.Wait(100*time.Millisecond))
}

func TestNewCheckpointTask_Failed_ReturnsError(t *testing.T) {
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "cp-2"},
		Status: agentsv1alpha1.CheckpointStatus{
			Phase:   agentsv1alpha1.CheckpointFailed,
			Message: "disk full",
		},
	}
	c, _, err := cachetest.NewTestCache(t, cp)
	require.NoError(t, err)
	task := c.NewCheckpointTask(context.Background(), cp)
	err = task.Wait(100 * time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "checkpoint default/cp-2 failed")
	assert.Contains(t, err.Error(), "disk full")
}

// --- PVC Task tests ---
func TestNewPVCTask_BoundAction(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pvc-1"},
		Spec: corev1.PersistentVolumeClaimSpec{
			VolumeName: "pv-1",
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}
	c, _, err := cachetest.NewTestCache(t, pvc)
	require.NoError(t, err)
	task := c.NewPVCTask(context.Background(), pvc)
	assert.Equal(t, cacheutils.WaitActionPVCBind, task.Action())
	// Already bound → satisfied fast path.
	assert.NoError(t, task.Wait(100*time.Millisecond))
}

func TestNewPVCTask_PendingTimeout(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pvc-pending"},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimPending,
		},
	}
	c, _, err := cachetest.NewTestCache(t, pvc)
	require.NoError(t, err)
	task := c.NewPVCTask(context.Background(), pvc)
	err = task.Wait(100 * time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "object is not satisfied")
}

func TestNewPVCTask_LostPhase_ReturnsError(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pvc-lost"},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimLost,
		},
	}
	c, _, err := cachetest.NewTestCache(t, pvc)
	require.NoError(t, err)
	task := c.NewPVCTask(context.Background(), pvc)
	err = task.Wait(100 * time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "is in Lost phase")
}

func TestNewPVCTask_FailureCondition_ReturnsError(t *testing.T) {
	tests := []struct {
		name          string
		conditionType corev1.PersistentVolumeClaimConditionType
		reason        string
		expectError   string
	}{
		{
			name:          "ControllerResizeError",
			conditionType: corev1.PersistentVolumeClaimControllerResizeError,
			reason:        "some-reason",
			expectError:   "ControllerResizeError",
		},
		{
			name:          "NodeResizeError",
			conditionType: corev1.PersistentVolumeClaimNodeResizeError,
			reason:        "some-reason",
			expectError:   "NodeResizeError",
		},
		{
			name:          "ModifyVolumeError",
			conditionType: corev1.PersistentVolumeClaimVolumeModifyVolumeError,
			reason:        "some-reason",
			expectError:   "ModifyVolumeError",
		},
		{
			name:          "ClaimLost condition",
			conditionType: corev1.PersistentVolumeClaimConditionType(corev1.ClaimLost),
			reason:        "volume deleted",
			expectError:   "ClaimLost",
		},
		{
			name:          "unknown condition with ResizeFailed reason",
			conditionType: "UnknownType",
			reason:        "ResizeFailed",
			expectError:   "ResizeFailed",
		},
		{
			name:          "unknown condition with VolumeResizeFailed reason",
			conditionType: "UnknownType",
			reason:        "VolumeResizeFailed",
			expectError:   "VolumeResizeFailed",
		},
		{
			name:          "unknown condition with FileSystemResizeFailed reason",
			conditionType: "UnknownType",
			reason:        "FileSystemResizeFailed",
			expectError:   "FileSystemResizeFailed",
		},
		{
			name:          "unknown condition with VolumeModifyFailed reason",
			conditionType: "UnknownType",
			reason:        "VolumeModifyFailed",
			expectError:   "VolumeModifyFailed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pvc-fail-" + tt.name},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimPending,
					Conditions: []corev1.PersistentVolumeClaimCondition{
						{
							Type:    tt.conditionType,
							Status:  corev1.ConditionTrue,
							Reason:  tt.reason,
							Message: "something went wrong",
						},
					},
				},
			}
			c, _, err := cachetest.NewTestCache(t, pvc)
			require.NoError(t, err)
			task := c.NewPVCTask(context.Background(), pvc)
			err = task.Wait(100 * time.Millisecond)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}

func TestNewPVCTask_NonFailureConditions_DoNotFailFast(t *testing.T) {
	tests := []struct {
		name          string
		conditionType corev1.PersistentVolumeClaimConditionType
		reason        string
	}{
		{
			name:          "Resizing condition",
			conditionType: corev1.PersistentVolumeClaimResizing,
			reason:        "some-reason",
		},
		{
			name:          "FileSystemResizePending condition",
			conditionType: corev1.PersistentVolumeClaimFileSystemResizePending,
			reason:        "some-reason",
		},
		{
			name:          "unknown condition with benign reason",
			conditionType: "UnknownType",
			reason:        "BenignReason",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pvc := &corev1.PersistentVolumeClaim{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pvc-nonfail-" + tt.name},
				Status: corev1.PersistentVolumeClaimStatus{
					Phase: corev1.ClaimPending,
					Conditions: []corev1.PersistentVolumeClaimCondition{
						{
							Type:   tt.conditionType,
							Status: corev1.ConditionTrue,
							Reason: tt.reason,
						},
					},
				},
			}
			c, _, err := cachetest.NewTestCache(t, pvc)
			require.NoError(t, err)
			task := c.NewPVCTask(context.Background(), pvc)
			err = task.Wait(100 * time.Millisecond)
			// Should timeout, not fail-fast with a specific error
			assert.Error(t, err)
			assert.Contains(t, err.Error(), "object is not satisfied")
		})
	}
}

func TestNewPVCTask_FailureConditionWithFalseStatus_DoesNotFailFast(t *testing.T) {
	// A failure condition with Status=False should not trigger fail-fast
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pvc-cond-false"},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimPending,
			Conditions: []corev1.PersistentVolumeClaimCondition{
				{
					Type:   corev1.PersistentVolumeClaimControllerResizeError,
					Status: corev1.ConditionFalse,
					Reason: "some-reason",
				},
			},
		},
	}
	c, _, err := cachetest.NewTestCache(t, pvc)
	require.NoError(t, err)
	task := c.NewPVCTask(context.Background(), pvc)
	err = task.Wait(100 * time.Millisecond)
	// Should timeout, not fail-fast
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "object is not satisfied")
}

func TestNewPVCTask_BoundWithoutVolumeName_NotSatisfied(t *testing.T) {
	// Bound phase but no VolumeName — should not be considered satisfied
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "pvc-bound-no-vol"},
		Status: corev1.PersistentVolumeClaimStatus{
			Phase: corev1.ClaimBound,
		},
	}
	c, _, err := cachetest.NewTestCache(t, pvc)
	require.NoError(t, err)
	task := c.NewPVCTask(context.Background(), pvc)
	err = task.Wait(100 * time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "object is not satisfied")
}
