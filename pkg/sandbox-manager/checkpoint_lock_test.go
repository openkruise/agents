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
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestSandboxManagerCreateCheckpoint(t *testing.T) {
	tests := []struct {
		name             string
		opts             CreateCheckpointOptions
		lockErr          error
		beforeErr        error
		checkpointErr    error
		releaseErr       error
		expectTTL        time.Duration
		expectErr        string
		expectCode       managererrors.ErrorCode
		expectBefore     bool
		expectCheckpoint bool
		expectReleased   bool
	}{
		{
			name:             "default wait timeout locks for 120 seconds",
			opts:             CreateCheckpointOptions{Operation: CheckpointOperationSnapshot},
			expectTTL:        2 * time.Minute,
			expectCheckpoint: true,
			expectReleased:   true,
		},
		{
			name: "custom wait timeout extends the lock",
			opts: CreateCheckpointOptions{
				Operation: CheckpointOperationSnapshot,
				Infra:     infra.CreateCheckpointOptions{WaitSuccessTimeout: 300 * time.Second},
			},
			expectTTL:        6 * time.Minute,
			expectCheckpoint: true,
			expectReleased:   true,
		},
		{
			name:       "lock conflict creates nothing",
			opts:       CreateCheckpointOptions{Operation: CheckpointOperationSnapshot},
			lockErr:    managererrors.NewError(managererrors.ErrorConflict, "fork checkpoint in progress"),
			expectTTL:  2 * time.Minute,
			expectErr:  "fork checkpoint in progress",
			expectCode: managererrors.ErrorConflict,
		},
		{
			name:           "before checkpoint failure releases the lock",
			opts:           CreateCheckpointOptions{Operation: CheckpointOperationFork},
			beforeErr:      errors.New("source paused"),
			expectTTL:      2 * time.Minute,
			expectErr:      "source paused",
			expectBefore:   true,
			expectReleased: true,
		},
		{
			name:             "checkpoint failure releases the lock",
			opts:             CreateCheckpointOptions{Operation: CheckpointOperationFork},
			checkpointErr:    errors.New("checkpoint failed"),
			expectTTL:        2 * time.Minute,
			expectErr:        "checkpoint failed",
			expectBefore:     true,
			expectCheckpoint: true,
			expectReleased:   true,
		},
		{
			name:             "release failure keeps the checkpoint result",
			opts:             CreateCheckpointOptions{Operation: CheckpointOperationSnapshot},
			releaseErr:       errors.New("release failed"),
			expectTTL:        2 * time.Minute,
			expectCheckpoint: true,
			expectReleased:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newForkOrchestrationSandbox("source", "source-uid")
			sbx.checkpointID = "checkpoint-id"
			sbx.lockErr = tt.lockErr
			sbx.checkpointErr = tt.checkpointErr
			sbx.releaseErr = tt.releaseErr
			beforeCalled := false
			if tt.expectBefore || tt.beforeErr != nil {
				tt.opts.BeforeCheckpoint = func(ctx context.Context, locked infra.Sandbox) error {
					beforeCalled = true
					assert.Same(t, sbx, locked)
					deadline, ok := ctx.Deadline()
					require.True(t, ok)
					workTimeout := tt.expectTTL - checkpointLockCleanupAllowance - checkpointLockMargin
					assert.False(t, deadline.After(time.Now().Add(workTimeout)))
					assert.True(t, deadline.After(time.Now().Add(workTimeout-time.Second)))
					return tt.beforeErr
				}
			}

			checkpointID, err := (&SandboxManager{}).CreateCheckpoint(t.Context(), sbx, tt.opts)

			require.Len(t, sbx.lockOps, 1)
			assert.Equal(t, infra.CheckpointLockOptions{Operation: tt.opts.Operation, TTL: tt.expectTTL}, sbx.lockOps[0])
			assert.Equal(t, tt.expectBefore, beforeCalled)
			if tt.expectCheckpoint {
				assert.Equal(t, []infra.CreateCheckpointOptions{tt.opts.Infra}, sbx.checkpointOps)
			} else {
				assert.Empty(t, sbx.checkpointOps)
			}
			if tt.expectReleased {
				require.Len(t, sbx.released, 1)
				assert.Equal(t, "holder", sbx.released[0].Holder)
			} else {
				assert.Empty(t, sbx.released)
			}
			if tt.expectErr != "" {
				require.ErrorContains(t, err, tt.expectErr)
				if tt.expectCode != "" {
					assert.Equal(t, tt.expectCode, managererrors.GetErrCode(err))
				}
				assert.Empty(t, checkpointID)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, "checkpoint-id", checkpointID)
		})
	}
}
