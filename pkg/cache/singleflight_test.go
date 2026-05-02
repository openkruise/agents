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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/controllers"
)

// TestParseSingleflightAnnotation tests annotation parsing with various inputs.
func TestParseSingleflightAnnotation(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		key         string
		want        singleflightAnnotation
		wantOK      bool
	}{
		{
			name: "valid annotation",
			annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": "5:true:1000",
			},
			key:    "test-key",
			want:   singleflightAnnotation{Seq: 5, Done: true, LastUpdate: 1000},
			wantOK: true,
		},
		{
			name: "valid annotation done=false",
			annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": "0:false:0",
			},
			key:    "test-key",
			want:   singleflightAnnotation{Seq: 0, Done: false, LastUpdate: 0},
			wantOK: true,
		},
		{
			name:   "missing annotation",
			key:    "test-key",
			want:   singleflightAnnotation{},
			wantOK: false,
		},
		{
			name: "malformed value",
			annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": "abc",
			},
			key:    "test-key",
			want:   singleflightAnnotation{},
			wantOK: false,
		},
		{
			name: "wrong parts count",
			annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": "1:2",
			},
			key:    "test-key",
			want:   singleflightAnnotation{},
			wantOK: false,
		},
		{
			name: "non-numeric seq",
			annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": "abc:true:1000",
			},
			key:    "test-key",
			want:   singleflightAnnotation{},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}
			got, ok := parseSingleflightAnnotation(sbx, tt.key)
			assert.Equal(t, tt.wantOK, ok)
			if ok {
				assert.Equal(t, tt.want.Seq, got.Seq)
				assert.Equal(t, tt.want.Done, got.Done)
				assert.Equal(t, tt.want.LastUpdate, got.LastUpdate)
			}
		})
	}
}

// TestDistributedSingleFlightDo_FirstRunnerWins tests the basic Run path where
// a single caller becomes the Runner and completes successfully.
func TestDistributedSingleFlightDo_FirstRunnerWins(t *testing.T) {
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
		},
	}

	cache, fc, err := newSingleflightTestCache(t, sandbox)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())

	mockMgr := cache.GetMockManager()
	mockMgr.AddWaitReconcileKey(sandbox)

	result, err := DistributedSingleFlightDo(
		t.Context(),
		cache,
		sandbox,
		"test-key",
		func(_ *v1alpha1.Sandbox) error { return nil },
		func(sbx *v1alpha1.Sandbox) { sbx.Labels["sf-modifier"] = "true" },
		func(_ *v1alpha1.Sandbox) error { return nil },
		DefaultSingleflightPreemptionThreshold,
	)

	require.NoError(t, err)
	assert.Equal(t, "true", result.Labels["sf-modifier"])

	// Verify annotation is set on the API server object
	var updated v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updated))
	annValue, ok := updated.Annotations[SingleflightAnnotationPrefix+"test-key"]
	require.True(t, ok)
	assert.Contains(t, annValue, "1:true:")
}

// TestDistributedSingleFlightDo_PrecheckFails tests that precheck failure
// aborts the operation and does not set any annotation.
func TestDistributedSingleFlightDo_PrecheckFails(t *testing.T) {
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
		},
	}

	cache, fc, err := newSingleflightTestCache(t, sandbox)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())

	mockMgr := cache.GetMockManager()
	mockMgr.AddWaitReconcileKey(sandbox)

	_, err = DistributedSingleFlightDo(
		t.Context(),
		cache,
		sandbox,
		"test-key",
		func(_ *v1alpha1.Sandbox) error { return fmt.Errorf("precheck failed") },
		func(_ *v1alpha1.Sandbox) {},
		func(_ *v1alpha1.Sandbox) error { return nil },
		DefaultSingleflightPreemptionThreshold,
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "precheck failed")

	// Verify no annotation was set
	var updated v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updated))
	_, ok := updated.Annotations[SingleflightAnnotationPrefix+"test-key"]
	assert.False(t, ok)
}

// TestDistributedSingleFlightDo_SecondCallerWaits tests the Wait path where
// a caller sees an active Runner and waits for it to finish.
func TestDistributedSingleFlightDo_SecondCallerWaits(t *testing.T) {
	now := time.Now()
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": fmt.Sprintf("1:false:%d", now.Unix()),
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
		},
	}

	cache, fc, err := newSingleflightTestCache(t, sandbox)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())

	mockMgr := cache.GetMockManager()
	mockMgr.AddWaitReconcileKey(sandbox)

	// Async goroutine: after 50ms, update annotation to done=true
	time.AfterFunc(50*time.Millisecond, func() {
		var sbx v1alpha1.Sandbox
		if err := fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &sbx); err != nil {
			return
		}
		modified := sbx.DeepCopy()
		setSingleflightAnnotation(modified, "test-key", 1, true)
		_ = fc.Update(t.Context(), modified)
	})

	start := time.Now()
	result, err := DistributedSingleFlightDo(
		t.Context(),
		cache,
		sandbox,
		"test-key",
		func(_ *v1alpha1.Sandbox) error { return nil },
		func(_ *v1alpha1.Sandbox) {},
		func(_ *v1alpha1.Sandbox) error { return nil },
		DefaultSingleflightPreemptionThreshold,
	)

	require.NoError(t, err)
	assert.GreaterOrEqual(t, time.Since(start), 40*time.Millisecond)

	// Verify the returned object has done=true annotation
	annValue := result.Annotations[SingleflightAnnotationPrefix+"test-key"]
	assert.Contains(t, annValue, "1:true:")

	// Verify no leftover wait entries
	cache.GetWaitHooks().Range(func(k, v any) bool {
		t.Errorf("unexpected leftover wait entry: %v", k)
		return false
	})
}

// TestDistributedSingleFlightDo_SeqConflictGoesToWait tests concurrent seq competition
// where one goroutine wins Run and the other gets a seq conflict and transitions to Wait.
func TestDistributedSingleFlightDo_SeqConflictGoesToWait(t *testing.T) {
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": fmt.Sprintf("5:true:%d", time.Now().Unix()),
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
		},
	}

	cache, fc, err := newSingleflightTestCache(t, sandbox)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())

	mockMgr := cache.GetMockManager()
	mockMgr.AddWaitReconcileKey(sandbox)

	// WaitGroup for both goroutines
	var wg sync.WaitGroup
	wg.Add(2)

	var err1, err2 error

	// First goroutine: tries to claim seq=6, becomes Runner, sleeps 50ms then returns
	go func() {
		defer wg.Done()
		_, err1 = DistributedSingleFlightDo(
			t.Context(),
			cache,
			sandbox,
			"test-key",
			func(_ *v1alpha1.Sandbox) error { return nil },
			func(sbx *v1alpha1.Sandbox) { sbx.Labels["sf-modifier"] = "true" },
			func(_ *v1alpha1.Sandbox) error {
				time.Sleep(50 * time.Millisecond)
				return nil
			},
			DefaultSingleflightPreemptionThreshold,
		)
	}()

	// Give first goroutine a head start
	time.Sleep(5 * time.Millisecond)

	// Second goroutine: tries to claim seq=6 but gets conflict, goes to Wait
	go func() {
		defer wg.Done()
		_, err2 = DistributedSingleFlightDo(
			t.Context(),
			cache,
			sandbox,
			"test-key",
			func(_ *v1alpha1.Sandbox) error { return nil },
			func(sbx *v1alpha1.Sandbox) { sbx.Labels["sf-modifier"] = "true" },
			func(_ *v1alpha1.Sandbox) error {
				time.Sleep(50 * time.Millisecond)
				return nil
			},
			DefaultSingleflightPreemptionThreshold,
		)
	}()

	// After 100ms, update annotation to "6:true:<now>" to unblock the waiting goroutine
	time.AfterFunc(100*time.Millisecond, func() {
		var sbx v1alpha1.Sandbox
		if err := fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &sbx); err != nil {
			return
		}
		modified := sbx.DeepCopy()
		setSingleflightAnnotation(modified, "test-key", 6, true)
		_ = fc.Update(t.Context(), modified)
	})

	wg.Wait()

	require.NoError(t, err1)
	require.NoError(t, err2)

	// Verify final annotation contains "6:true:"
	var updated v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updated))
	annValue := updated.Annotations[SingleflightAnnotationPrefix+"test-key"]
	assert.Contains(t, annValue, "6:true:")
}

// TestDistributedSingleFlightDo_Preemption tests that a stale Runner
// (with done=false and lastUpdate older than threshold) can be preempted.
func TestDistributedSingleFlightDo_Preemption(t *testing.T) {
	staleTime := time.Now().Add(-10 * time.Second).Unix()
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": fmt.Sprintf("1:false:%d", staleTime),
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
		},
	}

	cache, fc, err := newSingleflightTestCache(t, sandbox)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())

	mockMgr := cache.GetMockManager()
	mockMgr.AddWaitReconcileKey(sandbox)

	result, err := DistributedSingleFlightDo(
		t.Context(),
		cache,
		sandbox,
		"test-key",
		func(_ *v1alpha1.Sandbox) error { return nil },
		func(sbx *v1alpha1.Sandbox) { sbx.Labels["sf-modifier"] = "true" },
		func(_ *v1alpha1.Sandbox) error { return nil },
		1*time.Second, // preemption threshold = 1 second, lastUpdate is 10 seconds old
	)

	require.NoError(t, err)
	assert.Equal(t, "true", result.Labels["sf-modifier"])

	// Verify annotation starts with "2:true:" (preempted seq=1, claimed seq=2)
	var updated v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updated))
	annValue := updated.Annotations[SingleflightAnnotationPrefix+"test-key"]
	assert.Contains(t, annValue, "2:true:")
}

// TestReleaseSingleflightLock_SeqObsolete tests that release with a mismatched
// seq does not update the annotation.
func TestReleaseSingleflightLock_SeqObsolete(t *testing.T) {
	now := time.Now().Unix()
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": fmt.Sprintf("10:false:%d", now),
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
		},
	}

	cache, fc, err := newSingleflightTestCache(t, sandbox)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())

	// Try to release with seq=5 (doesn't match current seq=10)
	releaseSingleflightLock(cache, sandbox, "test-key", 5)

	// Verify annotation is unchanged
	var updated v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updated))
	annValue := updated.Annotations[SingleflightAnnotationPrefix+"test-key"]
	assert.Contains(t, annValue, "10:false:")
}

// TestReleaseSingleflightLock_Success tests that release with a matching
// seq successfully sets done=true.
func TestReleaseSingleflightLock_Success(t *testing.T) {
	now := time.Now().Unix()
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{
				SingleflightAnnotationPrefix + "test-key": fmt.Sprintf("5:false:%d", now),
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
		},
	}

	cache, fc, err := newSingleflightTestCache(t, sandbox)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())

	// Release with matching seq=5
	releaseSingleflightLock(cache, sandbox, "test-key", 5)

	// Verify annotation now shows done=true
	var updated v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updated))
	annValue := updated.Annotations[SingleflightAnnotationPrefix+"test-key"]
	assert.Contains(t, annValue, "5:true:")
}

func newSingleflightTestCache(t *testing.T, initObjs ...ctrlclient.Object) (*Cache, ctrlclient.Client, error) {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, idx := range GetIndexFuncs() {
		builder = builder.WithIndex(idx.Obj, idx.FieldName, idx.Extract)
	}

	builder = builder.WithStatusSubresource(
		&v1alpha1.Sandbox{},
		&v1alpha1.SandboxSet{},
		&v1alpha1.Checkpoint{},
		&v1alpha1.SandboxClaim{},
		&v1alpha1.SandboxTemplate{},
	)
	builder = builder.WithInterceptorFuncs(resourceVersionInterceptorFuncs())

	if len(initObjs) > 0 {
		builder = builder.WithObjects(initObjs...)
	}

	fakeClient := builder.Build()
	mgrBuilder, err := controllers.NewMockManagerBuilder(t)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create mock manager builder: %w", err)
	}

	mgr := mgrBuilder.
		WithScheme(scheme).
		WithClient(fakeClient).
		WithWaitSimulation().
		Build()

	c, err := NewCache(mgr)
	if err != nil {
		return nil, nil, err
	}

	mgr.SetWaitHooks(c.GetWaitHooks())
	return c, fakeClient, nil
}

func resourceVersionInterceptorFuncs() interceptor.Funcs {
	return interceptor.Funcs{
		Update: func(ctx context.Context, client ctrlclient.WithWatch, obj ctrlclient.Object, opts ...ctrlclient.UpdateOption) error {
			latest := obj.DeepCopyObject().(ctrlclient.Object)
			if err := client.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, latest); err == nil {
				obj.SetResourceVersion(latest.GetResourceVersion())
			}
			return client.Update(ctx, obj, opts...)
		},
	}
}
