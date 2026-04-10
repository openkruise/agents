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
	task := c.NewSandboxPauseTask(context.Background(), sbx)
	assert.Equal(t, cacheutils.WaitActionPause, task.Action())
	assert.Same(t, sbx, task.Object())
	// Already paused → Wait returns nil immediately.
	assert.NoError(t, task.Wait(100*time.Millisecond))
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
	task := c.NewSandboxResumeTask(context.Background(), sbx)
	assert.Equal(t, cacheutils.WaitActionResume, task.Action())
	// Running state → satisfied fast path.
	assert.NoError(t, task.Wait(100*time.Millisecond))
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
