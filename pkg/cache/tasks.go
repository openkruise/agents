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

package cache

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

// --- Sandbox: shared Update closure -----------------------------------------

func (c *Cache) SandboxUpdateFunc(ctx context.Context) cacheutils.UpdateFunc[*agentsv1alpha1.Sandbox] {
	return func(sbx *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
		key := types.NamespacedName{Namespace: sbx.Namespace, Name: sbx.Name}
		got := &agentsv1alpha1.Sandbox{}
		if err := utils.GetFromInformerOrApiServer(ctx, got, key, c.client, c.reader); err != nil {
			return nil, err
		}
		return got, nil
	}
}

func (c *Cache) CheckpointUpdateFunc(ctx context.Context) cacheutils.UpdateFunc[*agentsv1alpha1.Checkpoint] {
	return func(cp *agentsv1alpha1.Checkpoint) (*agentsv1alpha1.Checkpoint, error) {
		return c.refreshCheckpoint(ctx, cp)
	}
}

// --- Factories --------------------------------------------------------------

// NewSandboxPauseTask builds a WaitTask that succeeds when the sandbox reports
// status.conditions[type=Paused].status == True.
func (c *Cache) NewSandboxPauseTask(ctx context.Context, sbx *agentsv1alpha1.Sandbox) *cacheutils.WaitTask[*agentsv1alpha1.Sandbox] {
	check := func(s *agentsv1alpha1.Sandbox) (bool, error) {
		cond := utils.GetSandboxCondition(&s.Status, string(agentsv1alpha1.SandboxConditionPaused))
		if cond == nil {
			return false, nil
		}
		return cond.Status == metav1.ConditionTrue, nil
	}
	return cacheutils.NewWaitTask[*agentsv1alpha1.Sandbox](
		ctx, c.waitHooks, cacheutils.WaitActionPause, sbx, c.SandboxUpdateFunc(ctx), check,
	)
}

// NewSandboxResumeTask builds a WaitTask that succeeds when the sandbox reaches
// SandboxStateRunning.
func (c *Cache) NewSandboxResumeTask(ctx context.Context, sbx *agentsv1alpha1.Sandbox) *cacheutils.WaitTask[*agentsv1alpha1.Sandbox] {
	check := func(s *agentsv1alpha1.Sandbox) (bool, error) {
		state, _ := sandboxutils.GetSandboxState(s)
		return state == agentsv1alpha1.SandboxStateRunning, nil
	}
	return cacheutils.NewWaitTask[*agentsv1alpha1.Sandbox](
		ctx, c.waitHooks, cacheutils.WaitActionResume, sbx, c.SandboxUpdateFunc(ctx), check,
	)
}

// NewSandboxWaitReadyTask builds a WaitTask that encapsulates the readiness check
func (c *Cache) NewSandboxWaitReadyTask(ctx context.Context, sbx *agentsv1alpha1.Sandbox) *cacheutils.WaitTask[*agentsv1alpha1.Sandbox] {
	check := func(s *agentsv1alpha1.Sandbox) (bool, error) {
		if s.Status.ObservedGeneration != s.Generation {
			return false, nil
		}
		readyCond := utils.GetSandboxCondition(&s.Status, string(agentsv1alpha1.SandboxConditionReady))
		if readyCond != nil && readyCond.Reason == agentsv1alpha1.SandboxReadyReasonStartContainerFailed {
			return false, fmt.Errorf("sandbox start container failed: %s", readyCond.Message)
		}
		inplaceCond := utils.GetSandboxCondition(&s.Status, string(agentsv1alpha1.SandboxConditionInplaceUpdate))
		if inplaceCond != nil && inplaceCond.Reason == agentsv1alpha1.SandboxInplaceUpdateReasonInplaceUpdating {
			return false, nil
		}
		state, _ := sandboxutils.GetSandboxState(s)
		return state == agentsv1alpha1.SandboxStateRunning && s.Status.PodInfo.PodIP != "", nil
	}
	return cacheutils.NewWaitTask[*agentsv1alpha1.Sandbox](
		ctx, c.waitHooks, cacheutils.WaitActionWaitReady, sbx, c.SandboxUpdateFunc(ctx), check,
	)
}

// NewCheckpointTask builds a WaitTask that succeeds when the checkpoint's
// Status.Phase transitions to CheckpointSucceeded; returns error on Terminating / Failed.
func (c *Cache) NewCheckpointTask(ctx context.Context, cp *agentsv1alpha1.Checkpoint) *cacheutils.WaitTask[*agentsv1alpha1.Checkpoint] {
	check := func(cp *agentsv1alpha1.Checkpoint) (bool, error) {
		switch cp.Status.Phase {
		case agentsv1alpha1.CheckpointTerminating, agentsv1alpha1.CheckpointFailed:
			return false, fmt.Errorf("checkpoint %s/%s failed: %s", cp.Namespace, cp.Name, cp.Status.Message)
		case agentsv1alpha1.CheckpointSucceeded:
			if cp.Status.CheckpointId == "" {
				return false, fmt.Errorf("checkpoint %s/%s has no checkpoint id", cp.Namespace, cp.Name)
			}
			return true, nil
		default:
			return false, nil
		}
	}
	return cacheutils.NewWaitTask[*agentsv1alpha1.Checkpoint](
		ctx, c.waitHooks, cacheutils.WaitActionCheckpoint, cp, c.CheckpointUpdateFunc(ctx), check,
	)
}
