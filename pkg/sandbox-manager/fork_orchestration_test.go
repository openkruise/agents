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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

type forkOrchestrationSandbox struct {
	*sandboxcr.Sandbox

	mu            sync.Mutex
	checkpointID  string
	checkpointErr error
	checkpointOps []infra.CreateCheckpointOptions
	networkPolicy *infra.SandboxNetworkConfig
	networkErr    error
	refreshErr    error
	state         string
	reason        string
	saveErr       error
	saveOps       []infra.SaveTimeoutOptions
	killErr       error
	killed        int
}

func newForkOrchestrationSandbox(name string, uid types.UID) *forkOrchestrationSandbox {
	return &forkOrchestrationSandbox{Sandbox: sandboxcr.AsSandbox(&v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      name,
			UID:       uid,
			Annotations: map[string]string{
				v1alpha1.AnnotationOwner: testUser,
			},
		},
		Spec: v1alpha1.SandboxSpec{
			AutoPausePolicy: &v1alpha1.AutoPausePolicy{},
		},
		Status: v1alpha1.SandboxStatus{Phase: v1alpha1.SandboxRunning},
	}, nil)}
}

func (s *forkOrchestrationSandbox) GetState() (string, string) {
	if s.state == "" {
		return v1alpha1.SandboxStateRunning, ""
	}
	return s.state, s.reason
}

func (s *forkOrchestrationSandbox) RefreshForExclusiveOperation(context.Context) error {
	return s.refreshErr
}

func (s *forkOrchestrationSandbox) SelectNetworkPolicy(context.Context) (*infra.SandboxNetworkConfig, error) {
	return s.networkPolicy, s.networkErr
}

func (s *forkOrchestrationSandbox) CreateCheckpoint(_ context.Context, opts infra.CreateCheckpointOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpointOps = append(s.checkpointOps, opts)
	return s.checkpointID, s.checkpointErr
}

func (s *forkOrchestrationSandbox) SaveTimeoutWithPolicy(_ context.Context, opts infra.SaveTimeoutOptions, _ timeout.UpdatePolicy) (infra.TimeoutUpdateResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saveOps = append(s.saveOps, opts)
	return infra.TimeoutUpdateResult{}, s.saveErr
}

func (s *forkOrchestrationSandbox) Kill(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killed++
	return s.killErr
}

type forkCheckpointCleanup struct {
	namespace    string
	sandboxUID   string
	checkpointID string
}

type forkOrchestrationInfra struct {
	infra.Infrastructure

	source       infra.Sandbox
	childSaveErr error
	cloneErrors  []error
	cloneStarted chan<- struct{}
	cloneGate    <-chan struct{}
	cleanupErr   error

	mu       sync.Mutex
	cloneOps []infra.CloneSandboxOptions
	children []*forkOrchestrationSandbox
	cleanups []forkCheckpointCleanup
}

func (i *forkOrchestrationInfra) GetSandbox(context.Context, infra.GetSandboxOptions) (infra.Sandbox, error) {
	return i.source, nil
}

func (i *forkOrchestrationInfra) CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions) (infra.Sandbox, infra.CloneMetrics, error) {
	child := newForkOrchestrationSandbox("child", "child-uid")
	child.saveErr = i.childSaveErr
	if opts.Modifier != nil {
		if err := opts.Modifier(child); err != nil {
			return nil, infra.CloneMetrics{}, err
		}
	}

	i.mu.Lock()
	call := len(i.cloneOps)
	i.cloneOps = append(i.cloneOps, opts)
	i.children = append(i.children, child)
	cloneErr := error(nil)
	if call < len(i.cloneErrors) {
		cloneErr = i.cloneErrors[call]
	}
	i.mu.Unlock()

	if i.cloneStarted != nil {
		i.cloneStarted <- struct{}{}
	}
	if i.cloneGate != nil {
		<-i.cloneGate
		if err := ctx.Err(); err != nil {
			return nil, infra.CloneMetrics{}, err
		}
	}
	if cloneErr != nil {
		return nil, infra.CloneMetrics{}, cloneErr
	}
	return child, infra.CloneMetrics{}, nil
}

func (i *forkOrchestrationInfra) DeleteForkCheckpoint(_ context.Context, namespace, sandboxUID, checkpointID string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.cleanups = append(i.cleanups, forkCheckpointCleanup{
		namespace:    namespace,
		sandboxUID:   sandboxUID,
		checkpointID: checkpointID,
	})
	return i.cleanupErr
}

func TestClassifyForkSourceError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code managererrors.ErrorCode
	}{
		{name: "preserves nil", err: nil, code: managererrors.ErrorUnknown},
		{name: "preserves typed error", err: managererrors.NewError(managererrors.ErrorConflict, "conflict"), code: managererrors.ErrorConflict},
		{name: "maps not found", err: apierrors.NewNotFound(schema.GroupResource{Resource: "sandboxes"}, "source"), code: managererrors.ErrorNotFound},
		{name: "maps unavailable", err: apierrors.NewServerTimeout(schema.GroupResource{Resource: "sandboxes"}, "get", 1), code: managererrors.ErrorUnavailable},
		{name: "maps unknown", err: errors.New("unexpected"), code: managererrors.ErrorInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := classifyForkSourceError(tt.err, "source-id", "checkpointing")
			if tt.err == nil {
				assert.NoError(t, err)
				return
			}
			assert.Equal(t, tt.code, managererrors.GetErrCode(err))
		})
	}
}

func TestForkSandboxValidation(t *testing.T) {
	manager := &SandboxManager{}
	valid := ForkSandboxOptions{
		SourceID:  "source",
		Namespace: "default",
		User:      testUser,
		Count:     1,
	}
	tests := []struct {
		name   string
		mutate func(*ForkSandboxOptions)
	}{
		{
			name: "requires source ID",
			mutate: func(opts *ForkSandboxOptions) {
				opts.SourceID = ""
			},
		},
		{
			name: "requires namespace",
			mutate: func(opts *ForkSandboxOptions) {
				opts.Namespace = ""
			},
		},
		{
			name: "requires user",
			mutate: func(opts *ForkSandboxOptions) {
				opts.User = ""
			},
		},
		{
			name: "requires positive count",
			mutate: func(opts *ForkSandboxOptions) {
				opts.Count = 0
			},
		},
		{
			name: "limits count",
			mutate: func(opts *ForkSandboxOptions) {
				opts.Count = maxForkSandboxes + 1
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := valid
			tt.mutate(&opts)

			results, err := manager.ForkSandbox(t.Context(), opts)

			assert.Nil(t, results)
			require.Error(t, err)
			assert.Equal(t, managererrors.ErrorBadRequest, managererrors.GetErrCode(err))
		})
	}
}

func TestForkSandboxRejectsSourcePreparationFailures(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*forkOrchestrationSandbox)
		code   managererrors.ErrorCode
	}{
		{
			name: "refresh failure",
			mutate: func(source *forkOrchestrationSandbox) {
				source.refreshErr = errors.New("refresh failed")
			},
			code: managererrors.ErrorInternal,
		},
		{
			name: "recycling source",
			mutate: func(source *forkOrchestrationSandbox) {
				source.GetAnnotations()[v1alpha1.AnnotationCleanup] = v1alpha1.True
			},
			code: managererrors.ErrorConflict,
		},
		{
			name: "dead source",
			mutate: func(source *forkOrchestrationSandbox) {
				source.state = v1alpha1.SandboxStateDead
				source.reason = "Terminating"
			},
			code: managererrors.ErrorNotFound,
		},
		{
			name: "paused source",
			mutate: func(source *forkOrchestrationSandbox) {
				source.state = v1alpha1.SandboxStatePaused
			},
			code: managererrors.ErrorConflict,
		},
		{
			name: "network policy failure",
			mutate: func(source *forkOrchestrationSandbox) {
				source.networkErr = errors.New("network policy failed")
			},
			code: managererrors.ErrorInternal,
		},
		{
			name: "checkpoint failure",
			mutate: func(source *forkOrchestrationSandbox) {
				source.checkpointErr = errors.New("checkpoint failed")
			},
			code: managererrors.ErrorInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, _ := setupTestManager(t)
			source := newForkOrchestrationSandbox("source", "source-uid")
			source.checkpointID = "fork-checkpoint"
			tt.mutate(source)
			manager.infra = &forkOrchestrationInfra{Infrastructure: manager.infra, source: source}

			results, err := manager.ForkSandbox(t.Context(), ForkSandboxOptions{
				SourceID:  "source-id",
				Namespace: "default",
				User:      testUser,
				Count:     1,
			})

			assert.Nil(t, results)
			require.Error(t, err)
			assert.Equal(t, tt.code, managererrors.GetErrCode(err))
		})
	}
}

func TestForkSandboxReturnsCanceledSlotsBeforeClone(t *testing.T) {
	manager, _ := setupTestManager(t)
	source := newForkOrchestrationSandbox("source", "source-uid")
	source.checkpointID = "fork-checkpoint"
	fakeInfra := &forkOrchestrationInfra{Infrastructure: manager.infra, source: source}
	manager.infra = fakeInfra
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	results, err := manager.ForkSandbox(ctx, ForkSandboxOptions{
		SourceID:  "source-id",
		Namespace: "default",
		User:      testUser,
		Count:     2,
	})

	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, result := range results {
		assert.ErrorIs(t, result.Err, context.Canceled)
	}
	assert.Empty(t, fakeInfra.cloneOps)
}

func TestForkSandboxLogsCompletedCheckpointCleanupFailure(t *testing.T) {
	manager, _ := setupTestManager(t)
	source := newForkOrchestrationSandbox("source", "source-uid")
	source.checkpointID = "fork-checkpoint"
	fakeInfra := &forkOrchestrationInfra{
		Infrastructure: manager.infra,
		source:         source,
		cleanupErr:     errors.New("checkpoint cleanup failed"),
	}
	manager.infra = fakeInfra

	results, err := manager.ForkSandbox(t.Context(), ForkSandboxOptions{
		SourceID:  "source-id",
		Namespace: "default",
		User:      testUser,
		Count:     1,
	})

	require.NoError(t, err)
	require.Len(t, results, 1)
	require.NoError(t, results[0].Err)
	require.Len(t, fakeInfra.cleanups, 1)
}

func TestForkSandboxHappyBatchCreatesOneCheckpointAndCleansIt(t *testing.T) {
	manager, _ := setupTestManager(t)
	source := newForkOrchestrationSandbox("source", "source-uid")
	source.checkpointID = "fork-checkpoint"
	source.networkPolicy = &infra.SandboxNetworkConfig{AllowOut: []string{"example.com"}, DenyOut: []string{"blocked.example.com"}}
	fakeInfra := &forkOrchestrationInfra{Infrastructure: manager.infra, source: source}
	manager.infra = fakeInfra

	results, err := manager.ForkSandbox(t.Context(), ForkSandboxOptions{
		SourceID:                  "source-id",
		Namespace:                 "default",
		User:                      testUser,
		Count:                     3,
		TimeoutSeconds:            30,
		AutoPause:                 true,
		PausedRetention:           time.Hour,
		PausedRetentionAnnotation: "1h",
	})

	require.NoError(t, err)
	require.Len(t, results, 3)
	for _, result := range results {
		assert.NoError(t, result.Err)
		assert.NotNil(t, result.Sandbox)
	}

	require.Len(t, source.checkpointOps, 1)
	checkpointOpts := source.checkpointOps[0]
	assert.True(t, *checkpointOpts.KeepRunning)
	assert.Equal(t, forkCheckpointTTL, *checkpointOpts.TTL)
	assert.True(t, checkpointOpts.Fork)

	require.Len(t, fakeInfra.cloneOps, 3)
	for _, cloneOpts := range fakeInfra.cloneOps {
		assert.Equal(t, "fork-checkpoint", cloneOpts.CheckPointID)
		assert.True(t, cloneOpts.AllowForkCheckpoint)
		assert.True(t, cloneOpts.RotateRuntimeAccessToken)
		require.NotNil(t, cloneOpts.NetworkPolicy)
		assert.Equal(t, source.networkPolicy.AllowOut, cloneOpts.NetworkPolicy.AllowOut)
		assert.Equal(t, source.networkPolicy.DenyOut, cloneOpts.NetworkPolicy.DenyOut)
	}
	require.Len(t, fakeInfra.children, 3)
	for _, child := range fakeInfra.children {
		assert.Equal(t, "1h", child.GetAnnotations()[v1alpha1.AnnotationReservePausedSandboxDuration])
		assert.Nil(t, child.GetAutoPausePolicy())
		require.Len(t, child.saveOps, 1)
		assert.True(t, child.saveOps[0].SetAutoPausePolicy)
		assert.NotNil(t, child.saveOps[0].AutoPausePolicy)
	}

	require.Equal(t, []forkCheckpointCleanup{{
		namespace:    "default",
		sandboxUID:   "source-uid",
		checkpointID: "fork-checkpoint",
	}}, fakeInfra.cleanups)
}

func TestForkSandboxPartialChildFailureRetainsCheckpoint(t *testing.T) {
	manager, _ := setupTestManager(t)
	source := newForkOrchestrationSandbox("source", "source-uid")
	source.checkpointID = "fork-checkpoint"
	fakeInfra := &forkOrchestrationInfra{
		Infrastructure: manager.infra,
		source:         source,
		cloneErrors:    []error{errors.New("clone failed")},
	}
	manager.infra = fakeInfra

	results, err := manager.ForkSandbox(t.Context(), ForkSandboxOptions{
		SourceID:  "source-id",
		Namespace: "default",
		User:      testUser,
		Count:     3,
	})

	require.NoError(t, err)
	require.Len(t, results, 3)
	failed := 0
	for _, result := range results {
		if result.Err != nil {
			failed++
			assert.Nil(t, result.Sandbox)
			continue
		}
		assert.NotNil(t, result.Sandbox)
	}
	assert.Equal(t, 1, failed)
	assert.Len(t, fakeInfra.cloneOps, 3)
	assert.Empty(t, fakeInfra.cleanups)
}

func TestForkSandboxReturnsNilChildAfterTimeoutPersistFailure(t *testing.T) {
	manager, _ := setupTestManager(t)
	source := newForkOrchestrationSandbox("source", "source-uid")
	source.checkpointID = "fork-checkpoint"
	manager.infra = &forkOrchestrationInfra{
		Infrastructure: manager.infra,
		source:         source,
		childSaveErr:   errors.New("save timeout failed"),
	}

	results, err := manager.ForkSandbox(t.Context(), ForkSandboxOptions{
		SourceID:  "source-id",
		Namespace: "default",
		User:      testUser,
		Count:     1,
	})

	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Nil(t, results[0].Sandbox)
	require.ErrorContains(t, results[0].Err, "save final fork timeout")
}

func TestSaveForkChildTimeoutCleansChildAfterPersistFailure(t *testing.T) {
	manager, _ := setupTestManager(t)
	child := newForkOrchestrationSandbox("child", "child-uid")
	child.saveErr = errors.New("save timeout failed")

	err := manager.saveForkChildTimeout(t.Context(), child, ForkSandboxOptions{User: testUser, TimeoutSeconds: 30}, nil)

	require.ErrorContains(t, err, "save final fork timeout")
	assert.Equal(t, 1, child.killed)
}

func TestSaveForkChildTimeoutReturnsAfterChildCleanupFailure(t *testing.T) {
	manager, _ := setupTestManager(t)
	child := newForkOrchestrationSandbox("child", "child-uid")
	child.saveErr = errors.New("save timeout failed")
	child.killErr = errors.New("kill failed")

	err := manager.saveForkChildTimeout(t.Context(), child, ForkSandboxOptions{User: testUser, TimeoutSeconds: 30}, nil)

	require.ErrorContains(t, err, "save final fork timeout")
	assert.Equal(t, 1, child.killed)
}

func TestForkSandboxCancellationMarksRemainingSlots(t *testing.T) {
	manager, _ := setupTestManager(t)
	source := newForkOrchestrationSandbox("source", "source-uid")
	source.checkpointID = "fork-checkpoint"
	cloneStarted := make(chan struct{}, forkCloneWorkers)
	cloneGate := make(chan struct{})
	fakeInfra := &forkOrchestrationInfra{
		Infrastructure: manager.infra,
		source:         source,
		cloneStarted:   cloneStarted,
		cloneGate:      cloneGate,
	}
	manager.infra = fakeInfra

	ctx, cancel := context.WithCancel(t.Context())
	var releaseGate sync.Once
	release := func() {
		releaseGate.Do(func() { close(cloneGate) })
	}
	t.Cleanup(func() {
		cancel()
		release()
	})
	type forkResponse struct {
		results []ForkSandboxResult
		err     error
	}
	response := make(chan forkResponse, 1)
	go func() {
		results, err := manager.ForkSandbox(ctx, ForkSandboxOptions{
			SourceID:  "source-id",
			Namespace: "default",
			User:      testUser,
			Count:     forkCloneWorkers + 2,
		})
		response <- forkResponse{results: results, err: err}
	}()

	for range forkCloneWorkers {
		select {
		case <-cloneStarted:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for fork clone workers")
		}
	}
	cancel()
	release()

	select {
	case got := <-response:
		require.NoError(t, got.err)
		require.Len(t, got.results, forkCloneWorkers+2)
		for index, result := range got.results {
			require.Error(t, result.Err, "result %d", index)
		}
		for index := forkCloneWorkers; index < len(got.results); index++ {
			assert.ErrorIs(t, got.results[index].Err, context.Canceled)
		}
		assert.Empty(t, fakeInfra.cleanups)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for canceled fork")
	}
}
