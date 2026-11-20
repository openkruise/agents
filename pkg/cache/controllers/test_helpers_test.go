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

package controllers

import (
	"context"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
)

func newFakeClientWithScheme(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// newMockManagerBuilderForTest wraps NewMockManagerBuilder for tests, failing the
// test immediately if construction fails.
func newMockManagerBuilderForTest(t *testing.T) *MockManagerBuilder {
	t.Helper()
	b, err := NewMockManagerBuilder(t)
	require.NoError(t, err)
	return b
}

// startTestWait registers a real waiter before returning. The first read
// supplies the unfinished input; subsequent reads use the test client so a
// reconciler cache miss exercises the waiter's confirmation path.
func startTestWait[T client.Object](t *testing.T, hooks *sync.Map, obj T, reader client.Reader, action cacheutils.WaitAction, check cacheutils.CheckFunc[T]) <-chan error {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	started := make(chan struct{})
	done := make(chan error, 1)
	stopped := make(chan struct{})
	update := func(ctx context.Context, target T) (T, error) {
		select {
		case <-started:
			fresh := target.DeepCopyObject().(T)
			err := reader.Get(ctx, client.ObjectKeyFromObject(target), fresh)
			return fresh, err
		default:
			close(started)
			return target, nil
		}
	}
	task, err := cacheutils.NewAcquiredWaitTask(ctx, hooks, action, obj, update, func(snapshot T) (bool, error) {
		if client.Object(snapshot) == client.Object(obj) {
			return false, nil
		}
		return check(snapshot)
	})
	require.NoError(t, err)
	go func() {
		defer close(stopped)
		done <- task.Wait(time.Minute)
	}()
	<-started
	t.Cleanup(func() { cancel(); <-stopped })
	return done
}

func TestMockManager_WaitSimulation_Basic(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
	}
	fakeClient := newFakeClientWithScheme(t, sbx)

	waitHooks := &sync.Map{}

	reconcileCount := atomic.Int32{}
	entry := startTestWait(t, waitHooks, sbx, fakeClient,
		cacheutils.WaitActionWaitReady,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) {
			return reconcileCount.Add(1) >= 2, nil
		},
	)

	mgr := newMockManagerBuilderForTest(t).
		WithClient(fakeClient).
		WithWaitSimulation(sbx).
		Build()
	mgr.SetWaitHooks(waitHooks)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	select {
	case err := <-entry:
		require.NoError(t, err)
		assert.GreaterOrEqual(t, reconcileCount.Load(), int32(2))
	case <-time.After(2 * time.Second):
		t.Fatal("timeout")
	}
}

func TestMockManager_WaitSimulation_MultiType(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
	cp := &agentsv1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "default"}}
	fakeClient := newFakeClientWithScheme(t, sbx, cp)

	waitHooks := &sync.Map{}

	sbxDone := make(chan struct{})
	cpDone := make(chan struct{})

	startTestWait(t, waitHooks, sbx, fakeClient, cacheutils.WaitActionWaitReady, func(obj *agentsv1alpha1.Sandbox) (bool, error) {
		close(sbxDone)
		return true, nil
	})
	startTestWait(t, waitHooks, cp, fakeClient, cacheutils.WaitActionCheckpoint, func(obj *agentsv1alpha1.Checkpoint) (bool, error) {
		close(cpDone)
		return true, nil
	})

	mgr := newMockManagerBuilderForTest(t).
		WithClient(fakeClient).
		WithWaitSimulation(sbx, cp).
		Build()
	mgr.SetWaitHooks(waitHooks)

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	select {
	case <-sbxDone:
	case <-time.After(2 * time.Second):
		t.Fatal("sandbox timeout")
	}
	select {
	case <-cpDone:
	case <-time.After(2 * time.Second):
		t.Fatal("checkpoint timeout")
	}
}

func TestMockManager_WaitSimulation_ContextCancel(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
	fakeClient := newFakeClientWithScheme(t, sbx)

	waitHooks := &sync.Map{}
	reconcileCount := atomic.Int32{}
	startTestWait(t, waitHooks, sbx, fakeClient, cacheutils.WaitActionWaitReady, func(obj *agentsv1alpha1.Sandbox) (bool, error) {
		reconcileCount.Add(1)
		return false, nil
	})

	mgr := newMockManagerBuilderForTest(t).
		WithClient(fakeClient).
		WithWaitSimulation(sbx).
		Build()
	mgr.SetWaitHooks(waitHooks)

	ctx, cancel := context.WithCancel(t.Context())
	require.NoError(t, mgr.Start(ctx))

	time.Sleep(600 * time.Millisecond)
	countBefore := reconcileCount.Load()
	cancel()

	time.Sleep(600 * time.Millisecond)
	assert.GreaterOrEqual(t, countBefore, int32(1))
	assert.LessOrEqual(t, reconcileCount.Load(), countBefore+1)
}

func TestMockManager_WaitSimulation_CheckerError(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx-error", Namespace: "default"}}
	fakeClient := newFakeClientWithScheme(t, sbx)

	waitHooks := &sync.Map{}

	// A checker error ends the wait instead of leaving it pending.
	entry := startTestWait(t, waitHooks, sbx, fakeClient,
		cacheutils.WaitActionWaitReady,
		func(obj *agentsv1alpha1.Sandbox) (bool, error) {
			return false, assert.AnError
		},
	)

	mgr := newMockManagerBuilderForTest(t).
		WithClient(fakeClient).
		WithWaitSimulation(sbx).
		Build()
	mgr.SetWaitHooks(waitHooks)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	select {
	case err := <-entry:
		require.ErrorIs(t, err, assert.AnError)
	case <-time.After(1 * time.Second):
		t.Fatal("error entry timeout")
	}
}

func TestMockManager_WaitSimulation_Concurrent(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
	fakeClient := newFakeClientWithScheme(t, sbx)

	waitHooks := &sync.Map{}
	startTestWait(t, waitHooks, sbx, fakeClient, cacheutils.WaitActionWaitReady, func(obj *agentsv1alpha1.Sandbox) (bool, error) { return false, nil })

	mgr := newMockManagerBuilderForTest(t).
		WithClient(fakeClient).
		WithWaitSimulation(sbx).
		Build()
	mgr.SetWaitHooks(waitHooks)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	require.NoError(t, mgr.Start(ctx))

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mgr.AddWaitReconcileKey(&agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "concurrent", Namespace: "default"},
			})
		}()
	}
	wg.Wait()
	<-ctx.Done()
}

func TestMockManager_AddWaitReconcileKey(t *testing.T) {
	mgr := newMockManagerBuilderForTest(t).Build()

	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"}}
	cp := &agentsv1alpha1.Checkpoint{ObjectMeta: metav1.ObjectMeta{Name: "cp", Namespace: "ns"}}

	mgr.AddWaitReconcileKey(sbx)
	mgr.AddWaitReconcileKey(cp)
	mgr.AddWaitReconcileKey(sbx) // Duplicate add

	mgr.waitMu.RLock()
	defer mgr.waitMu.RUnlock()

	assert.Len(t, mgr.waitReconcileKeys[reflect.TypeOf(&agentsv1alpha1.Sandbox{})], 2)
	assert.Len(t, mgr.waitReconcileKeys[reflect.TypeOf(&agentsv1alpha1.Checkpoint{})], 1)
}

func TestMockManager_InitWaitReconcilers(t *testing.T) {
	mgr := newMockManagerBuilderForTest(t).Build()
	mgr.SetWaitHooks(&sync.Map{})
	mgr.waitSimEnabled = true
	mgr.initWaitReconcilers()

	assert.NotNil(t, mgr.waitReconcilers)
	assert.Contains(t, mgr.waitReconcilers, reflect.TypeOf(&agentsv1alpha1.Sandbox{}))
	assert.Contains(t, mgr.waitReconcilers, reflect.TypeOf(&agentsv1alpha1.Checkpoint{}))
}

func TestMockManager_Start_WithoutWaitSimulation(t *testing.T) {
	mgr := newMockManagerBuilderForTest(t).Build()

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	assert.NoError(t, mgr.Start(ctx))
}

func TestMockManager_Start_WithWaitSimulationButNilWaitHooks(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
	fakeClient := newFakeClientWithScheme(t, sbx)

	mgr := newMockManagerBuilderForTest(t).
		WithClient(fakeClient).
		WithWaitSimulation(sbx).
		Build()
	// Do not set waitHooks

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	assert.NoError(t, mgr.Start(ctx))
	<-ctx.Done()
}

func TestMockManager_WaitSimulation_UnknownType(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "default"}}
	fakeClient := newFakeClientWithScheme(t, sbx)

	mgr := newMockManagerBuilderForTest(t).
		WithClient(fakeClient).
		WithWaitSimulation(sbx).
		Build()
	mgr.SetWaitHooks(&sync.Map{})

	// Add unknown type
	mgr.waitMu.Lock()
	mgr.waitReconcileKeys[reflect.TypeOf(&agentsv1alpha1.SandboxSet{})] = []ctrl.Request{
		{NamespacedName: types.NamespacedName{Name: "unknown"}},
	}
	mgr.waitMu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	assert.NoError(t, mgr.Start(ctx))
	<-ctx.Done()
}

func TestMockManagerBuilder_WithWaitSimulation(t *testing.T) {
	sbx1 := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "s1", Namespace: "default"}}
	sbx2 := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Name: "s2", Namespace: "default"}}

	mgr := newMockManagerBuilderForTest(t).
		WithWaitSimulation(sbx1, sbx2).
		Build()

	assert.True(t, mgr.waitSimEnabled)
	mgr.waitMu.RLock()
	assert.Len(t, mgr.waitReconcileKeys[reflect.TypeOf(&agentsv1alpha1.Sandbox{})], 2)
	mgr.waitMu.RUnlock()
}

func TestMockManager_SetWaitHooks(t *testing.T) {
	mgr := newMockManagerBuilderForTest(t).Build()
	hooks := &sync.Map{}
	mgr.SetWaitHooks(hooks)
	assert.Equal(t, hooks, mgr.waitHooks)
}
