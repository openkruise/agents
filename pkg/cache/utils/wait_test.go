/*
Copyright 2025.

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

package utils

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// --- WaitHookKey tests ---

func TestWaitHookKey(t *testing.T) {
	tests := []struct {
		name        string
		obj         *agentsv1alpha1.Sandbox
		wantContain []string
	}{
		{
			name: "sandbox with namespace and name",
			obj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "my-sandbox",
					Namespace: "default",
				},
			},
			wantContain: []string{"my-sandbox", "default"},
		},
		{
			name: "sandbox with empty namespace",
			obj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "cluster-sandbox",
					Namespace: "",
				},
			},
			wantContain: []string{"cluster-sandbox"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := WaitHookKey[*agentsv1alpha1.Sandbox](tt.obj)
			assert.NotEmpty(t, key)
			for _, s := range tt.wantContain {
				assert.Contains(t, key, s)
			}
		})
	}
}

func TestWaitHookKey_DifferentTypes(t *testing.T) {
	// Keys for the same namespace/name but different types must differ
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "obj", Namespace: "ns"},
	}
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "obj", Namespace: "ns"},
	}
	keySbx := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	keyCp := WaitHookKey[*agentsv1alpha1.Checkpoint](cp)
	assert.NotEqual(t, keySbx, keyCp, "keys for different types must differ even when namespace/name are identical")
}

func TestWaitHookKey_UniquePerNamespaceName(t *testing.T) {
	sbx1 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-a", Namespace: "ns-1"},
	}
	sbx2 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-b", Namespace: "ns-1"},
	}
	sbx3 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "sbx-a", Namespace: "ns-2"},
	}
	k1 := WaitHookKey[*agentsv1alpha1.Sandbox](sbx1)
	k2 := WaitHookKey[*agentsv1alpha1.Sandbox](sbx2)
	k3 := WaitHookKey[*agentsv1alpha1.Sandbox](sbx3)
	assert.NotEqual(t, k1, k2, "different names must produce different keys")
	assert.NotEqual(t, k1, k3, "different namespaces must produce different keys")
	assert.NotEqual(t, k2, k3)
}

func TestWaitHookKey_FormatConsistency(t *testing.T) {
	// The same object should always produce the same key
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "stable", Namespace: "ns"},
	}
	k1 := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	k2 := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	assert.Equal(t, k1, k2)
	// Key format: %T/<ns>/<name>
	expected := fmt.Sprintf("%T/%s/%s", sbx, sbx.GetNamespace(), sbx.GetName())
	assert.Equal(t, expected, k1)
}

// --- WaitHookKeyFromRequest tests ---

func TestWaitHookKeyFromRequest(t *testing.T) {
	tests := []struct {
		name string
		req  ctrl.Request
	}{
		{
			name: "request with namespace and name",
			req: ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-sandbox"},
			},
		},
		{
			name: "request with empty namespace (cluster-scoped)",
			req: ctrl.Request{
				NamespacedName: types.NamespacedName{Namespace: "", Name: "cluster-res"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key := WaitHookKeyFromRequest[*agentsv1alpha1.Sandbox](tt.req)
			assert.NotEmpty(t, key)
			assert.Contains(t, key, tt.req.Name)
			if tt.req.Namespace != "" {
				assert.Contains(t, key, tt.req.Namespace)
			}
		})
	}
}

func TestWaitHookKeyFromRequest_ConsistentWithWaitHookKey(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "my-sandbox", Namespace: "default"},
	}
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "default", Name: "my-sandbox"},
	}

	keyFromObj := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	keyFromReq := WaitHookKeyFromRequest[*agentsv1alpha1.Sandbox](req)

	assert.Equal(t, keyFromObj, keyFromReq,
		"WaitHookKey and WaitHookKeyFromRequest must produce identical keys for same type/namespace/name")
}

func TestWaitHookKeyFromRequest_DifferentTypes(t *testing.T) {
	req := ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "obj"},
	}
	keySbx := WaitHookKeyFromRequest[*agentsv1alpha1.Sandbox](req)
	keyCp := WaitHookKeyFromRequest[*agentsv1alpha1.Checkpoint](req)
	assert.NotEqual(t, keySbx, keyCp, "keys for different types must differ")
}

// --- WaitEntry tests ---

func TestAcquireReleaseEntry_RefCountedLifecycle(t *testing.T) {
	sbx := newTestSandbox("sbx-refcount", "default")
	var hooks sync.Map
	key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)

	entry1, err := AcquireEntry[*agentsv1alpha1.Sandbox](
		&hooks, key, WaitActionResume,
	)
	require.NoError(t, err)
	require.NotNil(t, entry1)

	entry2, err := AcquireEntry[*agentsv1alpha1.Sandbox](
		&hooks, key, WaitActionResume,
	)
	require.NoError(t, err)
	assert.Same(t, entry1, entry2)

	ReleaseEntry[*agentsv1alpha1.Sandbox](&hooks, key, entry1)
	stored, exists := hooks.Load(key)
	require.True(t, exists)
	assert.Same(t, entry1, stored)

	ReleaseEntry[*agentsv1alpha1.Sandbox](&hooks, key, entry2)
	_, exists = hooks.Load(key)
	assert.False(t, exists)
}

func TestAcquireEntry_ActionConflictDoesNotAcquire(t *testing.T) {
	sbx := newTestSandbox("sbx-action-conflict", "default")
	var hooks sync.Map
	key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)

	entry, err := AcquireEntry[*agentsv1alpha1.Sandbox](
		&hooks, key, WaitActionResume,
	)
	require.NoError(t, err)

	conflict, err := AcquireEntry[*agentsv1alpha1.Sandbox](
		&hooks, key, WaitActionPause,
	)
	require.Error(t, err)
	assert.Nil(t, conflict)
	assert.Contains(t, err.Error(), "already exists")

	ReleaseEntry[*agentsv1alpha1.Sandbox](&hooks, key, entry)
	_, exists := hooks.Load(key)
	assert.False(t, exists, "conflicting acquire must not increment refs")
}

// --- lazy WaitTask path tests ---

func newTestSandbox(name, namespace string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func identityUpdate(_ context.Context, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	return obj, nil
}

// sandboxRunning is the checker shared by the read-outcome cases.
func sandboxRunning(obj *agentsv1alpha1.Sandbox) (bool, error) {
	return obj.Status.Phase == agentsv1alpha1.SandboxRunning, nil
}

// TestWaitTask_ReadOutcomes covers every source that can complete a lazy wait:
// the pre-check, a read, a read requested by RequestRead, and a polled read,
// including the read and checker errors that must end the wait.
func TestWaitTask_ReadOutcomes(t *testing.T) {
	tests := []struct {
		name  string
		check func(*agentsv1alpha1.Sandbox) (bool, error) // defaults to sandboxRunning
		// update returns the result of the n-th read (1-based). It defaults to
		// returning the object unchanged, which keeps the wait pending.
		update func(n int, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error)
		// poke requests a re-read once the waiter has parked after its first read.
		poke    bool
		wantErr error
	}{
		{
			name: "pre-check error ends the wait before reading",
			check: func(*agentsv1alpha1.Sandbox) (bool, error) {
				return false, assert.AnError
			},
			wantErr: assert.AnError,
		},
		{
			name: "first read satisfies",
			update: func(_ int, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
				obj = obj.DeepCopy()
				obj.Status.Phase = agentsv1alpha1.SandboxRunning
				return obj, nil
			},
		},
		{
			name: "requested re-read satisfies",
			update: func(n int, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
				if n == 1 {
					return obj, nil
				}
				obj = obj.DeepCopy()
				obj.Status.Phase = agentsv1alpha1.SandboxRunning
				return obj, nil
			},
			poke: true,
		},
		{
			name: "polled re-read satisfies",
			update: func(n int, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
				if n == 1 {
					return obj, nil
				}
				obj = obj.DeepCopy()
				obj.Status.Phase = agentsv1alpha1.SandboxRunning
				return obj, nil
			},
		},
		{
			name: "read error ends the wait",
			update: func(n int, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
				if n == 1 {
					return obj, nil
				}
				return nil, assert.AnError
			},
			poke:    true,
			wantErr: assert.AnError,
		},
		{
			name: "checker error ends the wait",
			check: func(obj *agentsv1alpha1.Sandbox) (bool, error) {
				if obj.Status.Phase == agentsv1alpha1.SandboxRunning {
					return false, assert.AnError
				}
				return false, nil
			},
			update: func(n int, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
				if n == 1 {
					return obj, nil
				}
				obj = obj.DeepCopy()
				obj.Status.Phase = agentsv1alpha1.SandboxRunning
				return obj, nil
			},
			poke:    true,
			wantErr: assert.AnError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var hooks sync.Map
				target := newTestSandbox("sbx-read", "default")
				target.Status.Phase = agentsv1alpha1.SandboxPending
				check := tt.check
				if check == nil {
					check = sandboxRunning
				}
				reads := 0
				task := NewWaitTask[*agentsv1alpha1.Sandbox](
					t.Context(), &hooks, WaitActionResume, target,
					func(_ context.Context, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
						reads++
						if tt.update == nil {
							return obj, nil
						}
						return tt.update(reads, obj)
					},
					check,
				)
				done := make(chan error, 1)
				go func() { done <- task.Wait(time.Minute) }()
				if tt.poke {
					synctest.Wait() // the waiter parked in select after its first read
					value, ok := hooks.Load(WaitHookKey(target))
					require.True(t, ok)
					value.(*WaitEntry[*agentsv1alpha1.Sandbox]).RequestRead()
				}
				err := <-done
				if tt.wantErr != nil {
					require.ErrorIs(t, err, tt.wantErr)
				} else {
					require.NoError(t, err)
				}
				_, exists := hooks.Load(WaitHookKey(target))
				assert.False(t, exists, "the wait hook must be released when the wait returns")
			})
		})
	}
}

// TestWaitTask_TimeoutClassification separates an internal wait-budget expiry
// from the caller's own deadline, because callers retry on the former and stop
// on the latter.
func TestWaitTask_TimeoutClassification(t *testing.T) {
	const budget = time.Second
	tests := []struct {
		name       string
		timeout    time.Duration
		ctx        func(t *testing.T) context.Context
		canceled   bool
		deadline   bool
		notSat     bool
		wantBudget time.Duration
	}{
		{
			name:       "internal wait budget expires",
			timeout:    budget,
			ctx:        func(t *testing.T) context.Context { t.Helper(); return t.Context() },
			notSat:     true,
			wantBudget: budget,
		},
		{
			name:    "caller deadline expires first",
			timeout: 5 * time.Minute,
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithTimeout(t.Context(), budget)
				t.Cleanup(cancel)
				return ctx
			},
			deadline:   true,
			notSat:     true,
			wantBudget: budget,
		},
		{
			name:    "caller cancels",
			timeout: 5 * time.Minute,
			ctx: func(t *testing.T) context.Context {
				t.Helper()
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			canceled: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var hooks sync.Map
				task := NewWaitTask[*agentsv1alpha1.Sandbox](
					tt.ctx(t), &hooks, WaitActionResume, newTestSandbox("sbx-timeout", "default"),
					identityUpdate,
					func(*agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
				)
				err := task.Wait(tt.timeout)
				require.Error(t, err)
				assert.Equal(t, tt.canceled, errors.Is(err, context.Canceled), "context.Canceled match")
				assert.Equal(t, tt.deadline, errors.Is(err, context.DeadlineExceeded), "context.DeadlineExceeded match")
				assert.Equal(t, tt.notSat, errors.Is(err, ErrWaitNotSatisfied), "ErrWaitNotSatisfied match")
				if tt.wantBudget != 0 {
					var waitErr *WaitNotSatisfiedError
					require.ErrorAs(t, err, &waitErr)
					assert.Equal(t, tt.wantBudget, waitErr.Timeout, "reported wait budget")
				}
			})
		})
	}
}

func TestWaitForObjectSatisfied_ActionConflict(t *testing.T) {
	sbx := newTestSandbox("sbx-conflict", "default")
	var hooks sync.Map
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// Register a long-running wait with action "Resume"
	go func() {
		defer close(done)
		_ = NewWaitTask[*agentsv1alpha1.Sandbox](
			ctx, &hooks, WaitActionResume, sbx,
			identityUpdate,
			func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		).Wait(time.Hour)
	}()

	// Wait until the hook is registered using Eventually instead of fixed Sleep
	require.Eventually(t, func() bool {
		key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
		_, exists := hooks.Load(key)
		return exists
	}, 2*time.Second, 10*time.Millisecond, "hook should be registered before conflict check")

	// Try with a different action on the same object - should conflict
	err := NewWaitTask[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, WaitActionPause, sbx,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
	).Wait(time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrWaitTaskConflict)
	var conflictErr *WaitTaskConflictError
	require.True(t, errors.As(err, &conflictErr))
	assert.Equal(t, WaitActionResume, conflictErr.ExistingAction)
	assert.Equal(t, WaitActionPause, conflictErr.NewAction)

	cancel()

	// Ensure goroutine completes before test returns
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not complete within timeout")
	}
}

func TestWaitForObjectSatisfied_ReuseExistingHook(t *testing.T) {
	sbx := newTestSandbox("sbx-reuse", "default")
	sbx.Status.Phase = agentsv1alpha1.SandboxPending
	var hooks sync.Map

	updatedSbx := sbx.DeepCopy()
	updatedSbx.Status.Phase = agentsv1alpha1.SandboxRunning
	var updateMu sync.Mutex
	updateCalls := 0

	satisfiedFunc := func(obj *agentsv1alpha1.Sandbox) (bool, error) {
		return obj.Status.Phase == agentsv1alpha1.SandboxRunning, nil
	}
	updateFunc := func(_ context.Context, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
		updateMu.Lock()
		defer updateMu.Unlock()
		updateCalls++
		if updateCalls <= 2 {
			return sbx, nil
		}
		return updatedSbx, nil
	}

	// Start two concurrent waits with the same action on the same object.
	// Both should reuse the same hook and succeed once the hook is signaled.
	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- NewWaitTask[*agentsv1alpha1.Sandbox](
				context.Background(), &hooks, WaitActionResume, sbx,
				updateFunc, satisfiedFunc,
			).Wait(2 * time.Second)
		}()
	}

	// Wait for both goroutines to register, then signal
	require.Eventually(t, func() bool {
		updateMu.Lock()
		defer updateMu.Unlock()
		return updateCalls >= 2
	}, 2*time.Second, 10*time.Millisecond, "both waiters should complete post-acquire checks before signal")
	key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	if val, ok := hooks.Load(key); ok {
		val.(*WaitEntry[*agentsv1alpha1.Sandbox]).RequestRead()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		assert.NoError(t, err)
	}
}

func TestWaitForObjectSatisfied_FirstCreatorExitsSecondStillWaiting(t *testing.T) {
	// Verifies that when the first waiter (entry creator) exits due to context cancellation,
	// the second waiter continues to wait and eventually succeeds.
	sbx := newTestSandbox("sbx-creator-exits", "default")
	sbx.Status.Phase = agentsv1alpha1.SandboxPending
	var hooks sync.Map
	key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)

	ctx1, cancel1 := context.WithCancel(context.Background())

	updatedSbx := sbx.DeepCopy()
	updatedSbx.Status.Phase = agentsv1alpha1.SandboxRunning

	// satisfied tracks whether the simulated object has become ready.
	var satisfiedMu sync.Mutex
	satisfied := false

	sharedUpdateFunc := func(_ context.Context, obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
		satisfiedMu.Lock()
		defer satisfiedMu.Unlock()
		if satisfied {
			return updatedSbx, nil
		}
		return sbx, nil
	}
	sharedCheckFunc := func(obj *agentsv1alpha1.Sandbox) (bool, error) {
		return obj.Status.Phase == agentsv1alpha1.SandboxRunning, nil
	}

	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)

	// First waiter (creator) - will be canceled early
	go func() {
		firstDone <- NewWaitTask[*agentsv1alpha1.Sandbox](
			ctx1, &hooks, WaitActionResume, sbx,
			sharedUpdateFunc, sharedCheckFunc,
		).Wait(time.Hour)
	}()

	// Wait for first waiter to register the hook
	require.Eventually(t, func() bool {
		_, exists := hooks.Load(key)
		return exists
	}, 2*time.Second, 10*time.Millisecond)

	// Second waiter joins the same entry
	go func() {
		secondDone <- NewWaitTask[*agentsv1alpha1.Sandbox](
			context.Background(), &hooks, WaitActionResume, sbx,
			sharedUpdateFunc, sharedCheckFunc,
		).Wait(time.Hour)
	}()

	// Wait for the second waiter to have acquired (refs == 2)
	require.Eventually(t, func() bool {
		val, ok := hooks.Load(key)
		if !ok {
			return false
		}
		entry := val.(*WaitEntry[*agentsv1alpha1.Sandbox])
		entry.mu.Lock()
		defer entry.mu.Unlock()
		return entry.refs >= 2
	}, 2*time.Second, 10*time.Millisecond)

	// Cancel the first waiter (creator exits)
	cancel1()
	select {
	case err := <-firstDone:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(5 * time.Second):
		t.Fatal("first waiter did not exit after context cancellation")
	}

	// Hook should still exist because second waiter holds a ref
	_, exists := hooks.Load(key)
	assert.True(t, exists, "hook must remain registered while second waiter is still active")

	// Mark the object as satisfied, then signal the entry
	satisfiedMu.Lock()
	satisfied = true
	satisfiedMu.Unlock()

	if val, ok := hooks.Load(key); ok {
		val.(*WaitEntry[*agentsv1alpha1.Sandbox]).RequestRead()
	}

	// Second waiter should succeed
	select {
	case err := <-secondDone:
		assert.NoError(t, err, "second waiter must succeed after first creator exits and condition is met")
	case <-time.After(5 * time.Second):
		t.Fatal("second waiter did not exit after signal")
	}

	// Hook should be fully cleaned up
	_, exists = hooks.Load(key)
	assert.False(t, exists, "hook should be cleaned up after all waiters exit")
}

// --- WaitAction constants test ---

func TestWaitActionConstants(t *testing.T) {
	assert.Equal(t, WaitAction("Resume"), WaitActionResume)
	assert.Equal(t, WaitAction("Pause"), WaitActionPause)
	assert.Equal(t, WaitAction("WaitReady"), WaitActionWaitReady)
	assert.Equal(t, WaitAction("Checkpoint"), WaitActionCheckpoint)
}

func TestWaitTask_ObservedSnapshotBelongsToEachWaiter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var hooks sync.Map
		target := newTestSandbox("shared", "default")
		target.UID = "new"
		target.ResourceVersion = "10"
		start := func(obj *agentsv1alpha1.Sandbox, label string) <-chan error {
			check := func(snapshot *agentsv1alpha1.Sandbox) (bool, error) { return snapshot.Labels[label] == "yes", nil }
			task, err := NewAcquiredWaitTask(t.Context(), &hooks, WaitActionResume, obj, identityUpdate, check)
			require.NoError(t, err)
			done := make(chan error, 1)
			go func() { done <- task.Wait(time.Minute) }()
			synctest.Wait()
			return done
		}
		old := target.DeepCopy()
		old.UID = "old"
		old.ResourceVersion = "1"
		first := start(old, "done")
		second := start(target, "done")
		value, ok := hooks.Load(WaitHookKey(target))
		require.True(t, ok)
		entry := value.(*WaitEntry[*agentsv1alpha1.Sandbox])
		snapshot := old.DeepCopy()
		snapshot.Labels = map[string]string{"done": "yes"}
		entry.Observe(snapshot)
		require.NoError(t, <-first)
		select {
		case err := <-second:
			t.Fatalf("stale observation completed second waiter: %v", err)
		default:
		}
		late := start(target, "ready")
		latest := target.DeepCopy()
		latest.Labels = snapshot.Labels
		entry.Observe(latest)
		require.NoError(t, <-second)
		synctest.Wait()
		select {
		case err := <-late:
			t.Fatalf("another waiter's predicate completed the late waiter: %v", err)
		default:
		}
		ready := latest.DeepCopy()
		ready.Labels["ready"] = "yes"
		entry.Observe(ready)
		require.NoError(t, <-late)
		_, ok = hooks.Load(WaitHookKey(target))
		require.False(t, ok)
	})
}
