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
	"fmt"
	"sync"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
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

func TestNewWaitEntry(t *testing.T) {
	ctx := context.Background()
	checker := func(obj *agentsv1alpha1.Sandbox) (bool, error) { return true, nil }
	entry := NewWaitEntry[*agentsv1alpha1.Sandbox](ctx, WaitActionResume, checker)

	require.NotNil(t, entry)
	assert.Equal(t, WaitActionResume, entry.Action)
	assert.NotNil(t, entry.Done())
	assert.Equal(t, ctx, entry.Context())
}

func TestWaitEntry_Close(t *testing.T) {
	entry := NewWaitEntry[*agentsv1alpha1.Sandbox](
		context.Background(),
		WaitActionPause,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
	)

	// Done channel should not be closed before Close
	select {
	case <-entry.Done():
		t.Fatal("done channel should not be closed before Close()")
	default:
	}

	entry.Close()

	// Done channel should be closed after Close
	select {
	case <-entry.Done():
		// expected
	case <-time.After(100 * time.Millisecond):
		t.Fatal("done channel was not closed after Close()")
	}
}

func TestWaitEntry_CloseIdempotent(t *testing.T) {
	entry := NewWaitEntry[*agentsv1alpha1.Sandbox](
		context.Background(),
		WaitActionPause,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
	)

	// Multiple Close calls should not panic (closeOnce ensures this)
	assert.NotPanics(t, func() {
		entry.Close()
		entry.Close()
		entry.Close()
	})
}

func TestWaitEntry_Check(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Status:     agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
	}

	t.Run("check returns true", func(t *testing.T) {
		entry := NewWaitEntry[*agentsv1alpha1.Sandbox](
			context.Background(),
			WaitActionWaitReady,
			func(obj *agentsv1alpha1.Sandbox) (bool, error) {
				return obj.Status.Phase == agentsv1alpha1.SandboxRunning, nil
			},
		)
		ok, err := entry.Check(sbx)
		require.NoError(t, err)
		assert.True(t, ok)
	})

	t.Run("check returns false", func(t *testing.T) {
		entry := NewWaitEntry[*agentsv1alpha1.Sandbox](
			context.Background(),
			WaitActionWaitReady,
			func(obj *agentsv1alpha1.Sandbox) (bool, error) {
				return obj.Status.Phase == agentsv1alpha1.SandboxPending, nil
			},
		)
		ok, err := entry.Check(sbx)
		require.NoError(t, err)
		assert.False(t, ok)
	})

	t.Run("check returns error", func(t *testing.T) {
		entry := NewWaitEntry[*agentsv1alpha1.Sandbox](
			context.Background(),
			WaitActionWaitReady,
			func(obj *agentsv1alpha1.Sandbox) (bool, error) {
				return false, assert.AnError
			},
		)
		ok, err := entry.Check(sbx)
		require.Error(t, err)
		assert.False(t, ok)
		assert.Equal(t, assert.AnError, err)
	})
}

func TestWaitEntry_Context(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entry := NewWaitEntry[*agentsv1alpha1.Sandbox](
		ctx,
		WaitActionCheckpoint,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
	)

	assert.Equal(t, ctx, entry.Context())
}

// --- WaitForObjectSatisfied tests ---

func newTestSandbox(name, namespace string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
}

func identityUpdate(obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
	return obj, nil
}

func TestWaitForObjectSatisfied_AlreadySatisfied(t *testing.T) {
	sbx := newTestSandbox("sbx-satisfied", "default")
	var hooks sync.Map

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return true, nil },
		time.Second,
	)
	require.NoError(t, err)
}

func TestWaitForObjectSatisfied_CheckFuncErrorImmediately(t *testing.T) {
	sbx := newTestSandbox("sbx-check-err", "default")
	var hooks sync.Map

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, assert.AnError },
		time.Second,
	)
	require.Error(t, err)
	assert.Equal(t, assert.AnError, err)
}

func TestWaitForObjectSatisfied_ZeroTimeout(t *testing.T) {
	sbx := newTestSandbox("sbx-zero-timeout", "default")
	var hooks sync.Map

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		0,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not satisfied")
}

func TestWaitForObjectSatisfied_NegativeTimeout(t *testing.T) {
	sbx := newTestSandbox("sbx-neg-timeout", "default")
	var hooks sync.Map

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		-1*time.Second,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not satisfied")
}

func TestWaitForObjectSatisfied_Timeout(t *testing.T) {
	sbx := newTestSandbox("sbx-timeout", "default")
	var hooks sync.Map

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		50*time.Millisecond,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not satisfied")
}

func TestWaitForObjectSatisfied_ContextAlreadyCanceled(t *testing.T) {
	sbx := newTestSandbox("sbx-ctx-canceled", "default")
	var hooks sync.Map

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before calling

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		ctx, &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		time.Hour,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not satisfied")
}

func TestWaitForObjectSatisfied_ContextTimeout(t *testing.T) {
	sbx := newTestSandbox("sbx-ctx-timeout", "default")
	var hooks sync.Map

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		ctx, &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		time.Hour,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not satisfied")
}

func TestWaitForObjectSatisfied_ActionConflict(t *testing.T) {
	sbx := newTestSandbox("sbx-conflict", "default")
	var hooks sync.Map
	done := make(chan struct{})

	// Register a long-running wait with action "Resume"
	go func() {
		defer close(done)
		_ = WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
			context.Background(), &hooks, sbx, WaitActionResume,
			identityUpdate,
			func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
			time.Hour,
		)
	}()

	// Wait until the hook is registered using Eventually instead of fixed Sleep
	require.Eventually(t, func() bool {
		key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
		_, exists := hooks.Load(key)
		return exists
	}, 2*time.Second, 10*time.Millisecond, "hook should be registered before conflict check")

	// Try with a different action on the same object - should conflict
	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionPause,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		time.Second,
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	// Close the entry so the first goroutine can exit (its internal timeout is 1 hour)
	key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	if val, ok := hooks.Load(key); ok {
		val.(*WaitEntry[*agentsv1alpha1.Sandbox]).Close()
	}

	// Ensure goroutine completes before test returns
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("goroutine did not complete within timeout")
	}
}

func TestWaitForObjectSatisfied_SatisfiedAfterSignal(t *testing.T) {
	sbx := newTestSandbox("sbx-signal", "default")
	sbx.Status.Phase = agentsv1alpha1.SandboxPending

	var hooks sync.Map

	// Simulate controller signaling "done" after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
		if val, ok := hooks.Load(key); ok {
			val.(*WaitEntry[*agentsv1alpha1.Sandbox]).Close()
		}
	}()

	// updateFunc returns a "satisfied" sandbox
	updatedSbx := sbx.DeepCopy()
	updatedSbx.Status.Phase = agentsv1alpha1.SandboxRunning

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		func(obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
			return updatedSbx, nil
		},
		func(obj *agentsv1alpha1.Sandbox) (bool, error) {
			return obj.Status.Phase == agentsv1alpha1.SandboxRunning, nil
		},
		2*time.Second,
	)
	require.NoError(t, err)
}

func TestWaitForObjectSatisfied_DoubleCheckUpdateError(t *testing.T) {
	sbx := newTestSandbox("sbx-update-err", "default")
	var hooks sync.Map

	// Signal the hook closed after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
		if val, ok := hooks.Load(key); ok {
			val.(*WaitEntry[*agentsv1alpha1.Sandbox]).Close()
		}
	}()

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		func(obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
			return nil, assert.AnError // update fails
		},
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		2*time.Second,
	)
	require.Error(t, err)
	assert.Equal(t, assert.AnError, err)
}

func TestWaitForObjectSatisfied_DoubleCheckSatisfiedError(t *testing.T) {
	sbx := newTestSandbox("sbx-check-fail", "default")
	var hooks sync.Map

	// Signal the hook closed after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
		if val, ok := hooks.Load(key); ok {
			val.(*WaitEntry[*agentsv1alpha1.Sandbox]).Close()
		}
	}()

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) {
			return false, assert.AnError // check fails
		},
		2*time.Second,
	)
	require.Error(t, err)
	assert.Equal(t, assert.AnError, err)
}

func TestWaitForObjectSatisfied_ReuseExistingHook(t *testing.T) {
	sbx := newTestSandbox("sbx-reuse", "default")
	var hooks sync.Map

	updatedSbx := sbx.DeepCopy()
	updatedSbx.Status.Phase = agentsv1alpha1.SandboxRunning

	satisfiedFunc := func(obj *agentsv1alpha1.Sandbox) (bool, error) {
		return obj.Status.Phase == agentsv1alpha1.SandboxRunning, nil
	}
	updateFunc := func(obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
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
			errCh <- WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
				context.Background(), &hooks, sbx, WaitActionResume,
				updateFunc, satisfiedFunc, 2*time.Second,
			)
		}()
	}

	// Wait for both goroutines to register, then signal
	time.Sleep(80 * time.Millisecond)
	key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	if val, ok := hooks.Load(key); ok {
		val.(*WaitEntry[*agentsv1alpha1.Sandbox]).Close()
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		assert.NoError(t, err)
	}
}

func TestWaitForObjectSatisfied_HookCleanedUpAfterReturn(t *testing.T) {
	sbx := newTestSandbox("sbx-cleanup", "default")
	var hooks sync.Map

	err := WaitForObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), &hooks, sbx, WaitActionResume,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return true, nil },
		time.Second,
	)
	require.NoError(t, err)

	// Hook should be cleaned up
	key := WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	_, exists := hooks.Load(key)
	assert.False(t, exists, "wait hook should be deleted after WaitForObjectSatisfied returns")
}

func TestWaitForObjectSatisfied_WithCheckpointType(t *testing.T) {
	// Verify the generic works with a different type (Checkpoint)
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "default"},
		Status:     agentsv1alpha1.CheckpointStatus{Phase: agentsv1alpha1.CheckpointSucceeded},
	}
	var hooks sync.Map

	err := WaitForObjectSatisfied[*agentsv1alpha1.Checkpoint](
		context.Background(), &hooks, cp, WaitActionCheckpoint,
		func(obj *agentsv1alpha1.Checkpoint) (*agentsv1alpha1.Checkpoint, error) { return obj, nil },
		func(obj *agentsv1alpha1.Checkpoint) (bool, error) {
			return obj.Status.Phase == agentsv1alpha1.CheckpointSucceeded, nil
		},
		time.Second,
	)
	require.NoError(t, err)
}

func TestWaitForObjectSatisfied_WithCoreV1Pod(t *testing.T) {
	// Verify the generic works with a standard k8s type (Pod)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "my-pod", Namespace: "default"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	var hooks sync.Map

	err := WaitForObjectSatisfied[*corev1.Pod](
		context.Background(), &hooks, pod, WaitActionWaitReady,
		func(obj *corev1.Pod) (*corev1.Pod, error) { return obj, nil },
		func(obj *corev1.Pod) (bool, error) {
			return obj.Status.Phase == corev1.PodRunning, nil
		},
		time.Second,
	)
	require.NoError(t, err)
}

// --- DoubleCheckObjectSatisfied tests ---

func TestDoubleCheckObjectSatisfied_UpdateError(t *testing.T) {
	sbx := newTestSandbox("sbx-dbl-update-err", "default")

	err := DoubleCheckObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), sbx,
		func(obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
			return nil, assert.AnError
		},
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return true, nil },
	)
	require.Error(t, err)
	assert.Equal(t, assert.AnError, err)
}

func TestDoubleCheckObjectSatisfied_SatisfiedCheckError(t *testing.T) {
	sbx := newTestSandbox("sbx-dbl-check-err", "default")

	err := DoubleCheckObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), sbx,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) {
			return false, assert.AnError
		},
	)
	require.Error(t, err)
	assert.Equal(t, assert.AnError, err)
}

func TestDoubleCheckObjectSatisfied_NotSatisfied(t *testing.T) {
	sbx := newTestSandbox("sbx-dbl-not-sat", "default")

	err := DoubleCheckObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), sbx,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not satisfied")
}

func TestDoubleCheckObjectSatisfied_Satisfied(t *testing.T) {
	sbx := newTestSandbox("sbx-dbl-satisfied", "default")

	err := DoubleCheckObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), sbx,
		identityUpdate,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) { return true, nil },
	)
	require.NoError(t, err)
}

func TestDoubleCheckObjectSatisfied_UpdatedObjectUsedForCheck(t *testing.T) {
	// Ensure that the updated object returned by updateFunc is the one passed to satisfiedFunc
	original := newTestSandbox("sbx-dbl-update-obj", "default")
	original.Status.Phase = agentsv1alpha1.SandboxPending

	updated := original.DeepCopy()
	updated.Status.Phase = agentsv1alpha1.SandboxRunning

	err := DoubleCheckObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), original,
		func(obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
			return updated, nil // return updated (running) object
		},
		func(obj *agentsv1alpha1.Sandbox) (bool, error) {
			return obj.Status.Phase == agentsv1alpha1.SandboxRunning, nil
		},
	)
	require.NoError(t, err)
}

func TestDoubleCheckObjectSatisfied_OriginalObjectNotSatisfied_UpdatedSatisfied(t *testing.T) {
	// Original is pending, updated is running -> should succeed
	original := newTestSandbox("sbx-dbl-orig-pending", "default")
	original.Status.Phase = agentsv1alpha1.SandboxPending

	updated := original.DeepCopy()
	updated.Status.Phase = agentsv1alpha1.SandboxRunning

	err := DoubleCheckObjectSatisfied[*agentsv1alpha1.Sandbox](
		context.Background(), original,
		func(obj *agentsv1alpha1.Sandbox) (*agentsv1alpha1.Sandbox, error) {
			return updated, nil
		},
		func(obj *agentsv1alpha1.Sandbox) (bool, error) {
			return obj.Status.Phase == agentsv1alpha1.SandboxRunning, nil
		},
	)
	require.NoError(t, err)
}

// --- WaitAction constants test ---

func TestWaitActionConstants(t *testing.T) {
	assert.Equal(t, WaitAction("Resume"), WaitActionResume)
	assert.Equal(t, WaitAction("Pause"), WaitActionPause)
	assert.Equal(t, WaitAction("WaitReady"), WaitActionWaitReady)
	assert.Equal(t, WaitAction("Checkpoint"), WaitActionCheckpoint)
}
