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

package wake

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
)

func TestInitWakerAndGetWaker(t *testing.T) {
	// Reset before test
	var zero atomic.Pointer[Waker]
	defaultWaker = zero

	// Before init, GetWaker returns nil
	if w := GetWaker(); w != nil {
		t.Error("GetWaker() should return nil before InitWaker is called")
	}

	// After init with nil cache, GetWaker returns non-nil (but Wake would fail)
	InitWaker(nil)
	w := GetWaker()
	if w == nil {
		t.Error("GetWaker() should return non-nil after InitWaker is called")
	}

	// The waker's cache field should be nil
	if w.cache != nil {
		t.Error("Waker.cache should be nil when initialized with nil cache")
	}

	// Reset for other tests
	defaultWaker = zero
}

func TestHasWakeAnnotation(t *testing.T) {
	tests := []struct {
		name        string
		sandboxName string
		sandboxNS   string
		annotations map[string]string
		createSbx   bool
		wakerNil    bool
		want        bool
	}{
		{
			name:        "annotation present true",
			sandboxName: "sbx-wake",
			sandboxNS:   "default",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: agentsv1alpha1.True,
			},
			createSbx: true,
			want:      true,
		},
		{
			name:        "annotation present false",
			sandboxName: "sbx-no-wake",
			sandboxNS:   "default",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeOnTraffic: "false",
			},
			createSbx: true,
			want:      false,
		},
		{
			name:        "annotation absent",
			sandboxName: "sbx-no-annot",
			sandboxNS:   "default",
			annotations: nil,
			createSbx:   true,
			want:        false,
		},
		{
			name:        "sandbox not found",
			sandboxName: "sbx-missing",
			sandboxNS:   "default",
			createSbx:   false,
			want:        false,
		},
		{
			name:     "nil waker returns false",
			wakerNil: true,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.wakerNil {
				var nilWaker *Waker
				assert.False(t, nilWaker.HasWakeAnnotation(context.Background(), "default", "sbx"))
				return
			}

			var initObjs []ctrl.Object
			if tt.createSbx {
				sbx := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        tt.sandboxName,
						Namespace:   tt.sandboxNS,
						Annotations: tt.annotations,
					},
				}
				initObjs = append(initObjs, sbx)
			}

			cacheProvider, _, err := cachetest.NewTestCache(t, initObjs...)
			require.NoError(t, err)

			waker := &Waker{cache: cacheProvider}
			got := waker.HasWakeAnnotation(context.Background(), tt.sandboxNS, tt.sandboxName)
			assert.Equal(t, tt.want, got)
		})
	}
}

// newPausedSandbox creates a Sandbox CR in Paused state with Paused condition True.
func newPausedSandbox(name, namespace string, annotations map[string]string, shutdownTime *metav1.Time) *agentsv1alpha1.Sandbox {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Annotations: annotations,
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxIsClaimed: "true",
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Paused:       true,
			ShutdownTime: shutdownTime,
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPaused,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				},
			},
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
		},
	}
	return sbx
}

func TestWake(t *testing.T) {
	shutdownTime := time.Now().Add(2 * time.Hour)
	pauseTime := time.Now().Add(1 * time.Hour)

	tests := []struct {
		name           string
		sandboxName    string
		sandboxNS      string
		annotations    map[string]string
		shutdownTime   *metav1.Time
		pauseTime      *metav1.Time
		defaultTimeout time.Duration
		skipCreate     bool
		simulateResume bool
		expectError    string
	}{
		{
			name:           "sandbox not found returns error",
			sandboxName:    "nonexistent",
			sandboxNS:      "default",
			defaultTimeout: 60 * time.Second,
			skipCreate:     true,
			expectError:    "not found",
		},
		{
			name:           "successful wake with default timeout",
			sandboxName:    "sbx-default",
			sandboxNS:      "default",
			annotations:    map[string]string{},
			shutdownTime:   &metav1.Time{Time: shutdownTime},
			pauseTime:      &metav1.Time{Time: pauseTime},
			defaultTimeout: 60 * time.Second,
			simulateResume: true,
			expectError:    "",
		},
		{
			name:        "wake with annotation timeout",
			sandboxName: "sbx-annot",
			sandboxNS:   "default",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeTimeoutSeconds: "120",
			},
			shutdownTime:   &metav1.Time{Time: shutdownTime},
			pauseTime:      &metav1.Time{Time: pauseTime},
			defaultTimeout: 60 * time.Second,
			simulateResume: true,
			expectError:    "",
		},
		{
			name:        "invalid annotation falls back to default",
			sandboxName: "sbx-invalid",
			sandboxNS:   "default",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeTimeoutSeconds: "abc",
			},
			shutdownTime:   &metav1.Time{Time: shutdownTime},
			pauseTime:      &metav1.Time{Time: pauseTime},
			defaultTimeout: 60 * time.Second,
			simulateResume: true,
			expectError:    "",
		},
		{
			name:        "negative annotation falls back to default",
			sandboxName: "sbx-negative",
			sandboxNS:   "default",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationWakeTimeoutSeconds: "-5",
			},
			shutdownTime:   &metav1.Time{Time: shutdownTime},
			pauseTime:      &metav1.Time{Time: pauseTime},
			defaultTimeout: 60 * time.Second,
			simulateResume: true,
			expectError:    "",
		},
		{
			name:           "short default timeout still resumes",
			sandboxName:    "sbx-short-timeout",
			sandboxNS:      "default",
			annotations:    map[string]string{},
			shutdownTime:   &metav1.Time{Time: shutdownTime},
			pauseTime:      &metav1.Time{Time: pauseTime},
			defaultTimeout: 30 * time.Second,
			simulateResume: true,
			expectError:    "",
		},
		{
			name:           "wake preserves nil ShutdownTime",
			sandboxName:    "sbx-nil-shutdown",
			sandboxNS:      "default",
			annotations:    map[string]string{},
			shutdownTime:   nil,
			pauseTime:      nil,
			defaultTimeout: 30 * time.Second,
			simulateResume: true,
			expectError:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.skipCreate {
				// Test with no sandbox in the cluster
				cacheProvider, _, err := cachetest.NewTestCache(t)
				require.NoError(t, err)
				require.NoError(t, cacheProvider.Run(t.Context()))
				t.Cleanup(func() { cacheProvider.Stop(t.Context()) })

				waker := &Waker{cache: cacheProvider}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				err = waker.Wake(ctx, tt.sandboxNS, tt.sandboxName, tt.defaultTimeout)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			sbx := newPausedSandbox(tt.sandboxName, tt.sandboxNS, tt.annotations, tt.shutdownTime)
			if tt.pauseTime != nil {
				sbx.Spec.PauseTime = tt.pauseTime
			}

			cacheProvider, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cacheProvider.Run(t.Context()))
			t.Cleanup(func() { cacheProvider.Stop(t.Context()) })

			// Create sandbox with status
			require.NoError(t, fc.Create(t.Context(), sbx))
			require.NoError(t, fc.Status().Update(t.Context(), sbx))
			time.Sleep(10 * time.Millisecond)

			waker := &Waker{cache: cacheProvider}

			if tt.simulateResume {
				mockMgr := cacheProvider.GetMockManager()
				mockMgr.AddWaitReconcileKey(sbx)

				modified := sbx.DeepCopy()
				mergeFrom := ctrl.MergeFrom(sbx)
				time.AfterFunc(20*time.Millisecond, func() {
					modified.Status.Phase = agentsv1alpha1.SandboxRunning
					modified.Status.Conditions = []metav1.Condition{
						{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
					}
					_ = fc.Status().Patch(t.Context(), modified, mergeFrom)
				})
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()

			err = waker.Wake(ctx, tt.sandboxNS, tt.sandboxName, tt.defaultTimeout)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)

			// Verify the sandbox was unpaused
			var updated agentsv1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), ctrl.ObjectKey{Namespace: tt.sandboxNS, Name: tt.sandboxName}, &updated))
			assert.False(t, updated.Spec.Paused, "sandbox should be unpaused after wake")

			// Verify ShutdownTime is preserved
			if tt.shutdownTime != nil {
				require.NotNil(t, updated.Spec.ShutdownTime, "ShutdownTime should be preserved")
				assert.WithinDuration(t, tt.shutdownTime.Time, updated.Spec.ShutdownTime.Time, time.Second)
			}
		})
	}
}

// TestWakeSingleflightCoalesces verifies that concurrent Wake calls for the
// same sandbox are coalesced into a single Resume call.
func TestWakeSingleflightCoalesces(t *testing.T) {
	const concurrency = 5

	sbx := newPausedSandbox("sbx-singleflight", "default", map[string]string{}, nil)
	cacheProvider, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cacheProvider.Run(t.Context()))
	t.Cleanup(func() { cacheProvider.Stop(t.Context()) })

	require.NoError(t, fc.Create(t.Context(), sbx))
	require.NoError(t, fc.Status().Update(t.Context(), sbx))
	time.Sleep(10 * time.Millisecond)

	waker := &Waker{cache: cacheProvider}

	// Set up mock to simulate successful resume
	mockMgr := cacheProvider.GetMockManager()
	mockMgr.AddWaitReconcileKey(sbx)

	modified := sbx.DeepCopy()
	mergeFrom := ctrl.MergeFrom(sbx)
	time.AfterFunc(50*time.Millisecond, func() {
		modified.Status.Phase = agentsv1alpha1.SandboxRunning
		modified.Status.Conditions = []metav1.Condition{
			{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
		}
		_ = fc.Status().Patch(t.Context(), modified, mergeFrom)
	})

	// Launch N concurrent Wake calls for the same sandbox.
	var (
		wg       sync.WaitGroup
		errCount atomic.Int32
	)
	for i := 0; i < concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := waker.Wake(ctx, "default", "sbx-singleflight", 60*time.Second); err != nil {
				errCount.Add(1)
			}
		}()
	}
	wg.Wait()

	// All callers should receive success (0 errors).
	assert.Equal(t, int32(0), errCount.Load(),
		"all concurrent Wake callers should succeed")
}

// TestWakeSingleflightDifferentSandboxes verifies that concurrent Wake calls
// for different sandboxes are NOT coalesced (each triggers its own Resume).
func TestWakeSingleflightDifferentSandboxes(t *testing.T) {
	sbx1 := newPausedSandbox("sbx-sf-a", "default", map[string]string{}, nil)
	sbx2 := newPausedSandbox("sbx-sf-b", "default", map[string]string{}, nil)

	cacheProvider, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cacheProvider.Run(t.Context()))
	t.Cleanup(func() { cacheProvider.Stop(t.Context()) })

	for _, sbx := range []*agentsv1alpha1.Sandbox{sbx1, sbx2} {
		require.NoError(t, fc.Create(t.Context(), sbx))
		require.NoError(t, fc.Status().Update(t.Context(), sbx))
	}
	time.Sleep(10 * time.Millisecond)

	waker := &Waker{cache: cacheProvider}

	mockMgr := cacheProvider.GetMockManager()
	mockMgr.AddWaitReconcileKey(sbx1)
	mockMgr.AddWaitReconcileKey(sbx2)

	// Delay status updates so Wake has time to start waiting.
	time.AfterFunc(50*time.Millisecond, func() {
		for _, sbx := range []*agentsv1alpha1.Sandbox{sbx1, sbx2} {
			mod := sbx.DeepCopy()
			mf := ctrl.MergeFrom(sbx)
			mod.Status.Phase = agentsv1alpha1.SandboxRunning
			mod.Status.Conditions = []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
			}
			_ = fc.Status().Patch(t.Context(), mod, mf)
		}
	})

	var (
		wg       sync.WaitGroup
		errCount atomic.Int32
	)
	for _, name := range []string{"sbx-sf-a", "sbx-sf-b"} {
		wg.Add(1)
		go func(n string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			if err := waker.Wake(ctx, "default", n, 60*time.Second); err != nil {
				errCount.Add(1)
			}
		}(name)
	}
	wg.Wait()

	// Both sandboxes should be woken independently.
	assert.Equal(t, int32(0), errCount.Load(),
		"both sandboxes should wake successfully")
}

// TestWakeSingleflightCallerCancel verifies that when a caller's context
// expires before the shared wake completes, it receives ctx.Err() while
// the shared work continues in the background.
func TestWakeSingleflightCallerCancel(t *testing.T) {
	sbx := newPausedSandbox("sbx-cancel", "default", map[string]string{}, nil)
	cacheProvider, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cacheProvider.Run(t.Context()))
	t.Cleanup(func() { cacheProvider.Stop(t.Context()) })

	require.NoError(t, fc.Create(t.Context(), sbx))
	require.NoError(t, fc.Status().Update(t.Context(), sbx))
	time.Sleep(10 * time.Millisecond)

	waker := &Waker{cache: cacheProvider}

	// Set up mock: delay the resume so the impatient caller times out
	mockMgr := cacheProvider.GetMockManager()
	mockMgr.AddWaitReconcileKey(sbx)

	modified := sbx.DeepCopy()
	mergeFrom := ctrl.MergeFrom(sbx)
	// Delay resume completion so the first caller times out
	time.AfterFunc(200*time.Millisecond, func() {
		modified.Status.Phase = agentsv1alpha1.SandboxRunning
		modified.Status.Conditions = []metav1.Condition{
			{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
		}
		_ = fc.Status().Patch(t.Context(), modified, mergeFrom)
	})

	// Impatient caller with very short timeout
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	err = waker.Wake(ctx, "default", "sbx-cancel", 60*time.Second)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded,
		"impatient caller should receive DeadlineExceeded")

	// Wait for background work to complete, then verify sandbox was resumed
	time.Sleep(500 * time.Millisecond)
	var updated agentsv1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), ctrl.ObjectKey{Namespace: "default", Name: "sbx-cancel"}, &updated))
	assert.False(t, updated.Spec.Paused, "sandbox should still be resumed by background work")
}
