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
	lockErr       error
	releaseErr    error
	// onLock mutates the Sandbox as the object returned by acquisition.
	onLock       func(*forkOrchestrationSandbox)
	onCheckpoint func()
	lockOps      []infra.CheckpointLockOptions
	released     []v1alpha1.CheckpointLock
	state        string
	reason       string
	saveErr      error
	saveOps      []infra.SaveTimeoutOptions
	killErr      error
	killed       int
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

func (s *forkOrchestrationSandbox) AcquireCheckpointLock(_ context.Context, opts infra.CheckpointLockOptions) (v1alpha1.CheckpointLock, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lockOps = append(s.lockOps, opts)
	if s.lockErr != nil {
		return v1alpha1.CheckpointLock{}, s.lockErr
	}
	if s.onLock != nil {
		s.onLock(s)
	}
	return v1alpha1.CheckpointLock{Holder: "holder", Operation: opts.Operation, ExpireTime: metav1.NewTime(time.Now().Add(opts.TTL))}, nil
}

func (s *forkOrchestrationSandbox) ReleaseCheckpointLock(_ context.Context, lock v1alpha1.CheckpointLock) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = append(s.released, lock)
	return s.releaseErr
}

func (s *forkOrchestrationSandbox) SelectNetworkPolicy(context.Context) (*infra.SandboxNetworkConfig, error) {
	return s.networkPolicy, s.networkErr
}

func (s *forkOrchestrationSandbox) CreateCheckpoint(_ context.Context, opts infra.CreateCheckpointOptions) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpointOps = append(s.checkpointOps, opts)
	if s.onCheckpoint != nil {
		s.onCheckpoint()
	}
	if s.checkpointErr != nil {
		return "", s.checkpointErr
	}
	return s.checkpointID, nil
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
	lookupErr    error
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
	return i.source, i.lookupErr
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
		{name: "maps lookup deadline", err: managererrors.WrapError(managererrors.ErrorInternal, context.DeadlineExceeded, "lookup timed out"), code: managererrors.ErrorUnavailable},
		{name: "maps lookup unavailability", err: managererrors.WrapError(managererrors.ErrorInternal, apierrors.NewServiceUnavailable("unavailable"), "lookup failed"), code: managererrors.ErrorUnavailable},
		{name: "preserves internal error", err: managererrors.NewError(managererrors.ErrorInternal, "internal failure"), code: managererrors.ErrorInternal},
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

func TestForkSandboxSourceLookupFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code managererrors.ErrorCode
	}{
		{name: "deadline", err: context.DeadlineExceeded, code: managererrors.ErrorUnavailable},
		{name: "backend unavailable", err: apierrors.NewServiceUnavailable("unavailable"), code: managererrors.ErrorUnavailable},
		{name: "absent", err: infra.ErrSandboxNotFound, code: managererrors.ErrorNotFound},
		{name: "unexpected", err: errors.New("unexpected"), code: managererrors.ErrorInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := &SandboxManager{infra: &forkOrchestrationInfra{lookupErr: tt.err}}
			results, err := manager.ForkSandbox(t.Context(), ForkSandboxOptions{
				SourceID: "source", Namespace: "default", User: testUser, Count: 1,
			})
			require.Error(t, err)
			assert.Nil(t, results)
			assert.Equal(t, tt.code, managererrors.GetErrCode(err))
			if tt.code != managererrors.ErrorNotFound {
				assert.ErrorIs(t, err, tt.err)
			}
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
		// locked reports whether the checkpoint lock is acquired and released.
		locked bool
	}{
		{
			name: "lock acquisition failure",
			mutate: func(source *forkOrchestrationSandbox) {
				source.lockErr = errors.New("lock failed")
			},
			code: managererrors.ErrorInternal,
		},
		{
			name: "concurrent checkpoint",
			mutate: func(source *forkOrchestrationSandbox) {
				source.lockErr = managererrors.NewError(managererrors.ErrorConflict, "snapshot checkpoint in progress")
			},
			code: managererrors.ErrorConflict,
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
			name: "source replaced before lock acquisition",
			mutate: func(source *forkOrchestrationSandbox) {
				source.onLock = func(locked *forkOrchestrationSandbox) {
					locked.SetUID("replacement-uid")
				}
			},
			code:   managererrors.ErrorNotFound,
			locked: true,
		},
		{
			name: "source paused before lock acquisition",
			mutate: func(source *forkOrchestrationSandbox) {
				source.onLock = func(locked *forkOrchestrationSandbox) {
					locked.state = v1alpha1.SandboxStatePaused
				}
			},
			code:   managererrors.ErrorConflict,
			locked: true,
		},
		{
			name: "network policy failure",
			mutate: func(source *forkOrchestrationSandbox) {
				source.networkErr = errors.New("network policy failed")
			},
			code:   managererrors.ErrorInternal,
			locked: true,
		},
		{
			name: "checkpoint failure",
			mutate: func(source *forkOrchestrationSandbox) {
				source.checkpointErr = errors.New("checkpoint failed")
			},
			code:   managererrors.ErrorInternal,
			locked: true,
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
			if tt.locked {
				assert.Len(t, source.lockOps, 1)
				assert.Len(t, source.released, 1)
			} else {
				assert.Empty(t, source.released)
			}
			if source.lockErr == nil && !tt.locked {
				assert.Empty(t, source.lockOps, "an unforkable informer view must not write the lock")
			}
		})
	}
}

func TestForkSandboxCancellationBeforeClone(t *testing.T) {
	tests := []struct {
		name string
		// cancelAt is when the request context is canceled.
		cancelAt    string
		requestErr  bool
		locks       int
		checkpoints int
	}{
		{name: "canceled request takes no lock", cancelAt: "request", requestErr: true},
		{name: "cancellation while locking creates no checkpoint", cancelAt: "lock", requestErr: true, locks: 1},
		{name: "cancellation during checkpoint marks every slot", cancelAt: "checkpoint", locks: 1, checkpoints: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, _ := setupTestManager(t)
			source := newForkOrchestrationSandbox("source", "source-uid")
			source.checkpointID = "fork-checkpoint"
			fakeInfra := &forkOrchestrationInfra{Infrastructure: manager.infra, source: source}
			manager.infra = fakeInfra
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch tt.cancelAt {
			case "request":
				cancel()
			case "lock":
				source.onLock = func(*forkOrchestrationSandbox) { cancel() }
			case "checkpoint":
				source.onCheckpoint = cancel
			}

			results, err := manager.ForkSandbox(ctx, ForkSandboxOptions{
				SourceID:  "source-id",
				Namespace: "default",
				User:      testUser,
				Count:     2,
			})

			if tt.requestErr {
				require.ErrorIs(t, err, context.Canceled)
				assert.Nil(t, results)
			} else {
				require.NoError(t, err)
				require.Len(t, results, 2)
				for _, result := range results {
					assert.ErrorIs(t, result.Err, context.Canceled)
				}
			}
			assert.Len(t, source.lockOps, tt.locks)
			assert.Len(t, source.released, tt.locks)
			assert.Len(t, source.checkpointOps, tt.checkpoints)
			assert.Empty(t, fakeInfra.cloneOps)
		})
	}
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
	assert.Equal(t, []infra.CheckpointLockOptions{{Operation: CheckpointOperationFork, TTL: 2 * time.Minute}}, source.lockOps)
	assert.Len(t, source.released, 1)

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

func TestForkSandboxSecurityRules(t *testing.T) {
	const blockRules = `[{"name":"block","match":[{"domains":["blocked.example.com"]}],"actions":{"block":{"statusCode":403}}}]`
	const headerRules = `[{"name":"headers","match":[{"domains":["api.example.com"]}],"actions":{"headerManipulation":{"set":[{"name":"x-trace","value":"trace"}]}}}]`
	tests := []struct {
		name    string
		initial string
		locked  string
	}{
		{name: "no rules"},
		{name: "domain block without TrafficPolicy", initial: blockRules, locked: blockRules},
		{name: "header injection without TrafficPolicy", initial: headerRules, locked: headerRules},
		{name: "rules from locked source", initial: headerRules, locked: blockRules},
		{name: "rules removed before acquisition", initial: blockRules},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			source := newForkOrchestrationSandbox("source", "source-uid")
			source.checkpointID = "checkpoint"
			source.Annotations[v1alpha1.AnnotationSecurityRules] = tt.initial
			source.onLock = func(locked *forkOrchestrationSandbox) {
				locked.Annotations[v1alpha1.AnnotationSecurityRules] = tt.locked
			}
			manager, _ := setupTestManager(t)
			fakeInfra := &forkOrchestrationInfra{Infrastructure: manager.infra, source: source}
			manager.infra = fakeInfra
			results, err := manager.ForkSandbox(t.Context(), ForkSandboxOptions{
				SourceID: "source", Namespace: "default", User: testUser, Count: 2,
			})
			require.NoError(t, err)
			require.Len(t, results, 2)
			for _, result := range results {
				require.NoError(t, result.Err)
				assert.Equal(t, tt.locked, result.Sandbox.GetAnnotations()[v1alpha1.AnnotationSecurityRules])
			}
		})
	}
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
