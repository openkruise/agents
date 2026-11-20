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

package sandbox_manager

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	checkpointSourceLookupTimeout = 2 * time.Second
	checkpointReadbackTimeout     = time.Second
)

// CreateSandboxCheckpoint creates a checkpoint of an owned Running sandbox and
// returns the completed checkpoint as observed by the owner-scoped list.
func (m *SandboxManager) CreateSandboxCheckpoint(ctx context.Context, user string, source infra.GetSandboxOptions, opts infra.CreateCheckpointOptions) (infra.CheckpointInfo, error) {
	lookupCtx, cancelLookup := context.WithTimeout(ctx, checkpointSourceLookupTimeout)
	sandbox, err := m.GetSandbox(lookupCtx, user, nil, source)
	cancelLookup()
	if err != nil {
		if managererrors.GetErrCode(err) == managererrors.ErrorNotAllowed {
			return infra.CheckpointInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "sandbox not found")
		}
		return infra.CheckpointInfo{}, err
	}
	if utils.IsReservedFailedSandbox(sandbox.GetLabels()) {
		return infra.CheckpointInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "sandbox not found")
	}
	state, _ := sandbox.GetState()
	switch state {
	case agentsv1alpha1.SandboxStateRunning:
	case agentsv1alpha1.SandboxStatePaused:
		return infra.CheckpointInfo{}, managererrors.NewError(managererrors.ErrorBadRequest, "sandbox %s is not running", sandbox.GetSandboxID())
	default:
		return infra.CheckpointInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "sandbox not found")
	}

	sandboxID := sandbox.GetSandboxID()
	namespace := sandbox.GetNamespace()
	checkpointID, err := sandbox.CreateCheckpoint(ctx, opts)
	if err != nil {
		return infra.CheckpointInfo{}, err
	}

	readCtx, cancelRead := context.WithTimeout(ctx, checkpointReadbackTimeout)
	defer cancelRead()
	var completed infra.CheckpointInfo
	err = wait.ExponentialBackoffWithContext(readCtx, utils.CacheBackoff, func(ctx context.Context) (bool, error) {
		checkpoints, _, err := m.ListCheckpoints(ctx, infra.SelectSucceededCheckpointsOptions{
			Namespace: namespace,
			User:      user,
		}, nil)
		if err != nil {
			return false, err
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for _, checkpoint := range checkpoints {
			if checkpoint.CheckpointID == checkpointID && checkpoint.SandboxID == sandboxID && checkpoint.Namespace == namespace {
				completed = checkpoint
				return true, nil
			}
		}
		return false, nil
	})
	if err != nil {
		// Native creation succeeded. Keep the checkpoint available for listing
		// and deletion even when the response cannot be built.
		return infra.CheckpointInfo{}, managererrors.WrapError(managererrors.ErrorInternal, err,
			"failed to read completed checkpoint %q: %v", checkpointID, err)
	}
	return completed, nil
}
