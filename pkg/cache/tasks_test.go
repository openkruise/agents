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
	task := c.NewSandboxWaitReadyTask(context.Background(), sbx, false)
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
	task := c.NewSandboxWaitReadyTask(context.Background(), sbx, false)
	err = task.Wait(100 * time.Millisecond)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "start container failed")
}

func TestNewSandboxWaitReadyTask_RequireInplaceUpdateCompletion(t *testing.T) {
	tests := []struct {
		name        string
		sbxName     string
		inplaceCond *metav1.Condition
		expectError string
	}{
		{
			name:        "inplace update condition nil - waits until timeout",
			sbxName:     "sbx-inplace-nil",
			inplaceCond: nil,
			expectError: "not satisfied",
		},
		{
			name:    "inplace update condition InplaceUpdating - waits until timeout",
			sbxName: "sbx-inplace-updating",
			inplaceCond: &metav1.Condition{
				Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
				Status: metav1.ConditionFalse,
				Reason: agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
			},
			expectError: "not satisfied",
		},
		{
			name:    "inplace update condition Succeeded - ready immediately",
			sbxName: "sbx-inplace-succeeded",
			inplaceCond: &metav1.Condition{
				Type:   string(agentsv1alpha1.SandboxConditionInplaceUpdate),
				Status: metav1.ConditionTrue,
				Reason: agentsv1alpha1.SandboxInplaceUpdateReasonSucceeded,
			},
			expectError: "",
		},
		{
			name:    "inplace update condition Failed - returns error immediately",
			sbxName: "sbx-inplace-failed",
			inplaceCond: &metav1.Condition{
				Type:    string(agentsv1alpha1.SandboxConditionInplaceUpdate),
				Status:  metav1.ConditionFalse,
				Reason:  agentsv1alpha1.SandboxInplaceUpdateReasonFailed,
				Message: "QoS class changed",
			},
			expectError: "in-place update failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: tt.sbxName, Generation: 1},
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
			if tt.inplaceCond != nil {
				sbx.Status.Conditions = append(sbx.Status.Conditions, *tt.inplaceCond)
			}
			c, _, err := cachetest.NewTestCache(t, sbx)
			require.NoError(t, err)
			task := c.NewSandboxWaitReadyTask(context.Background(), sbx, true)
			err = task.Wait(200 * time.Millisecond)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				assert.NoError(t, err)
			}
		})
	}
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
