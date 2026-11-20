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
	stderrors "errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

type checkpointTestBackend struct {
	infra.Infrastructure
	lookup func(context.Context, infra.GetSandboxOptions) (infra.Sandbox, error)
	list   func(context.Context, infra.SelectSucceededCheckpointsOptions) ([]infra.CheckpointInfo, error)
}

func (b checkpointTestBackend) GetSandbox(ctx context.Context, opts infra.GetSandboxOptions) (infra.Sandbox, error) {
	return b.lookup(ctx, opts)
}

func (b checkpointTestBackend) SelectSucceededCheckpoints(ctx context.Context, opts infra.SelectSucceededCheckpointsOptions) ([]infra.CheckpointInfo, error) {
	return b.list(ctx, opts)
}

type checkpointTestSandbox struct {
	infra.Sandbox
	create func(context.Context, infra.CreateCheckpointOptions) (string, error)
}

func (s checkpointTestSandbox) CreateCheckpoint(ctx context.Context, opts infra.CreateCheckpointOptions) (string, error) {
	return s.create(ctx, opts)
}

func TestCreateSandboxCheckpointLookupFailure(t *testing.T) {
	manager := &SandboxManager{infra: checkpointTestBackend{
		lookup: func(context.Context, infra.GetSandboxOptions) (infra.Sandbox, error) {
			return nil, stderrors.New("cache unavailable")
		},
	}}
	_, err := manager.CreateSandboxCheckpoint(t.Context(), testUser, infra.GetSandboxOptions{}, infra.CreateCheckpointOptions{})
	require.Error(t, err)
	assert.Equal(t, managererrors.ErrorInternal, managererrors.GetErrCode(err))
}

func TestCreateSandboxCheckpointReadback(t *testing.T) {
	for _, tt := range []struct {
		name       string
		cancel     bool
		neverReady bool
		wantErr    error
	}{
		{name: "retry stale cache with canonical source"},
		{name: "readback misses within budget", neverReady: true, wantErr: context.DeadlineExceeded},
		{name: "cancellation after success keeps checkpoint", cancel: true, wantErr: context.Canceled},
	} {
		t.Run(tt.name, func(t *testing.T) {
			requestCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			source := runningCheckpointTestSandbox()
			sandbox := sandboxcr.AsSandbox(source, nil)
			record := infra.CheckpointInfo{
				Name:              "checkpoint-cr",
				Namespace:         source.Namespace,
				CheckpointID:      "artifact-id",
				SandboxID:         sandbox.GetSandboxID(),
				Phase:             string(agentsv1alpha1.CheckpointSucceeded),
				CreationTimestamp: "2026-11-15T10:00:00Z",
			}
			requestSource := infra.GetSandboxOptions{SandboxID: "request-id"}
			creates, reads := 0, 0
			backend := checkpointTestBackend{
				lookup: func(ctx context.Context, opts infra.GetSandboxOptions) (infra.Sandbox, error) {
					assert.Equal(t, requestSource, opts)
					deadline, bounded := ctx.Deadline()
					require.True(t, bounded)
					assert.LessOrEqual(t, time.Until(deadline), checkpointSourceLookupTimeout)
					return checkpointTestSandbox{Sandbox: sandbox, create: func(ctx context.Context, _ infra.CreateCheckpointOptions) (string, error) {
						creates++
						assert.True(t, ctx == requestCtx, "native creation must use the original request context")
						if tt.cancel {
							cancel()
						}
						return record.CheckpointID, nil
					}}, nil
				},
				list: func(ctx context.Context, opts infra.SelectSucceededCheckpointsOptions) ([]infra.CheckpointInfo, error) {
					reads++
					assert.Equal(t, infra.SelectSucceededCheckpointsOptions{Namespace: source.Namespace, User: testUser}, opts)
					deadline, bounded := ctx.Deadline()
					require.True(t, bounded)
					assert.LessOrEqual(t, time.Until(deadline), checkpointReadbackTimeout)
					if tt.neverReady || reads == 1 {
						wrongSource := record
						wrongSource.SandboxID = requestSource.SandboxID
						wrongNamespace := record
						wrongNamespace.Namespace = "another-namespace"
						wrongArtifact := record
						wrongArtifact.CheckpointID = "different-id"
						return []infra.CheckpointInfo{wrongSource, wrongNamespace, wrongArtifact}, nil
					}
					return []infra.CheckpointInfo{record}, nil
				},
			}
			manager := &SandboxManager{infra: backend}
			actual, err := manager.CreateSandboxCheckpoint(requestCtx, testUser, requestSource, infra.CreateCheckpointOptions{})

			assert.Equal(t, 1, creates)
			if tt.wantErr == nil {
				require.NoError(t, err)
				assert.Equal(t, record, actual)
				assert.Equal(t, 2, reads)
				return
			}
			require.ErrorIs(t, err, tt.wantErr)
			assert.Empty(t, actual)
			assert.Equal(t, managererrors.ErrorInternal, managererrors.GetErrCode(err))
			assert.ErrorContains(t, err, record.CheckpointID)
			if tt.cancel {
				assert.Zero(t, reads)
			}
		})
	}
}

func runningCheckpointTestSandbox() *agentsv1alpha1.Sandbox {
	return getSandboxForApiTest("checkpoint-source", func(s *agentsv1alpha1.Sandbox) {
		s.Status.Conditions = []metav1.Condition{{
			Type:   string(agentsv1alpha1.SandboxConditionReady),
			Status: metav1.ConditionTrue,
		}}
	})
}
