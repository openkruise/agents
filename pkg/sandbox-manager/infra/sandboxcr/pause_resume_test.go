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

package sandboxcr

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	cachepkg "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/runtime"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/openkruise/agents/pkg/utils/timeout"
	testutils "github.com/openkruise/agents/test/utils"
)

type apiReaderOverrideCache struct {
	cachepkg.Provider
	apiReader ctrl.Reader
	client    ctrl.Client
}

func (c *apiReaderOverrideCache) GetAPIReader() ctrl.Reader {
	if c.apiReader == nil {
		return c.Provider.GetAPIReader()
	}
	return c.apiReader
}

func (c *apiReaderOverrideCache) GetClient() ctrl.Client {
	if c.client == nil {
		return c.Provider.GetClient()
	}
	return c.client
}

// TestSandbox_ResumeConcurrent tests concurrent resume operations on the same sandbox
func TestSandbox_ResumeConcurrent(t *testing.T) {
	utils.InitLogOutput()
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPaused,
			PodInfo: v1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
		},
		Spec: v1alpha1.SandboxSpec{
			Paused: true,
		},
	}

	// Initialize the sandbox as paused (similar to "resume paused / paused sandbox" test case)
	sandbox.Status.Phase = v1alpha1.SandboxPaused
	sandbox.Status.Conditions = append(sandbox.Status.Conditions, metav1.Condition{
		Type:   string(v1alpha1.SandboxConditionPaused),
		Status: metav1.ConditionTrue,
	})
	state, reason := sandboxutils.GetSandboxState(sandbox)
	assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)

	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())
	CreateSandboxWithStatus(t, fc, sandbox)
	time.Sleep(10 * time.Millisecond)

	// Register sandbox key for wait simulation
	mockMgr := cache.GetMockManager()
	mockMgr.AddWaitReconcileKey(sandbox)

	// Channel to collect results from goroutines
	resultCh := make(chan error, 3)

	start := time.Now()
	// Start three goroutines calling Resume
	for i := 0; i < 3; i++ {
		s := AsSandbox(sandbox, cache)
		go func() {
			err := s.Resume(t.Context(), infra.ResumeOptions{})
			resultCh <- err
		}()
	}

	// After 0.5 seconds, update the sandbox phase to Running and Ready condition to True
	time.AfterFunc(500*time.Millisecond, func() {
		var sbxToUpdate v1alpha1.Sandbox
		if err := fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: sandbox.Name}, &sbxToUpdate); err != nil {
			return
		}
		modified := sbxToUpdate.DeepCopy()
		modified.Status.Phase = v1alpha1.SandboxRunning
		SetSandboxCondition(modified, string(v1alpha1.SandboxConditionReady), metav1.ConditionTrue, "Resume", "")
		_ = fc.Status().Patch(t.Context(), modified, ctrl.MergeFrom(&sbxToUpdate))
	})

	// Wait for all goroutines to complete
	results := make([]error, 3)
	for i := 0; i < 3; i++ {
		results[i] = <-resultCh
	}

	// Check that all goroutines returned nil (no error)
	for i, result := range results {
		if result != nil {
			t.Errorf("Goroutine %d returned error: %v", i, result)
		}
	}

	assert.True(t, time.Since(start) >= 500*time.Millisecond)

	// Verify that the sandbox is in Running state
	var updatedSbx v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updatedSbx))
	state, reason = sandboxutils.GetSandboxState(&updatedSbx)
	assert.Equal(t, v1alpha1.SandboxStateRunning, state, reason)
	assert.False(t, updatedSbx.Spec.Paused)
}

// TestSandbox_Resume_ContextExpiredAfterWait tests the scenario where the wait operation
// succeeds right at the deadline boundary, causing the context to expire. The Resume function
// should create a fresh context for post-resume operations (ReInit, CSI mount, inplace refresh).
func TestSandbox_Resume_ContextExpiredAfterWait(t *testing.T) {
	utils.InitLogOutput()

	serverOpts := testutils.TestRuntimeServerOptions{
		RunCommandResult: runtime.RunCommandResult{
			PID:    1,
			Exited: true,
		},
		RunCommandImmediately: true,
		InitErrCode:           0,
	}
	server := testutils.NewTestRuntimeServer(serverOpts)
	defer server.Close()

	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL: server.URL,
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPaused,
			PodInfo: v1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
		},
		Spec: v1alpha1.SandboxSpec{
			Paused: true,
		},
	}

	// Initialize as paused state
	sandbox.Status.Conditions = append(sandbox.Status.Conditions, metav1.Condition{
		Type:   string(v1alpha1.SandboxConditionPaused),
		Status: metav1.ConditionTrue,
	})
	state, reason := sandboxutils.GetSandboxState(sandbox)
	assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)

	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())
	CreateSandboxWithStatus(t, fc, sandbox)
	time.Sleep(10 * time.Millisecond)

	s := AsSandbox(sandbox, cache)

	// Register sandbox key for wait simulation
	mockMgr := cache.GetMockManager()
	mockMgr.AddWaitReconcileKey(sandbox)

	// Use a very short timeout (200ms) and update sandbox at 190ms
	// This simulates the scenario where wait succeeds right at the deadline boundary
	modified := s.Sandbox.DeepCopy()
	mergeFrom := ctrl.MergeFrom(s.Sandbox)
	time.AfterFunc(190*time.Millisecond, func() {
		modified.Status.Phase = v1alpha1.SandboxRunning
		modified.Status.Conditions = []metav1.Condition{
			{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
		}
		_ = fc.Status().Patch(t.Context(), modified, mergeFrom)
	})

	// Context timeout is slightly longer than the update delay, but short enough
	// that context may expire during the wait's double-check phase
	resumeCtx, resumeCancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer resumeCancel()

	err = s.Resume(resumeCtx, infra.ResumeOptions{})

	// Should succeed because Resume creates a fresh context for post-resume operations
	require.NoError(t, err)

	// Verify sandbox is in Running state
	var updatedSbx v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updatedSbx))
	state, reason = sandboxutils.GetSandboxState(&updatedSbx)
	assert.Equal(t, v1alpha1.SandboxStateRunning, state, reason)
	assert.False(t, updatedSbx.Spec.Paused)
	assert.Nil(t, updatedSbx.Spec.ShutdownTime)
	assert.Nil(t, updatedSbx.Spec.PauseTime)
}

//goland:noinspection GoDeprecation
func TestSandbox_Pause(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name                   string
		initSandbox            func(sbx *v1alpha1.Sandbox)
		expectedState          string
		expectError            string
		expectTimeoutUpdate    bool
		simulatePauseCompleted bool // use time.AfterFunc to simulate underlying pause completion
		useShortTimeout        bool // simulate underlying not completing by using short context timeout
	}{
		{
			name: "pause running sandbox - operation completed successfully",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = false
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateRunning, state, reason)
			},
			expectedState:          v1alpha1.SandboxStatePaused,
			expectError:            "",
			expectTimeoutUpdate:    true,
			simulatePauseCompleted: true,
		},
		{
			name: "pause running sandbox - underlying operation not completed",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = false
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateRunning, state, reason)
			},
			expectedState:   "",
			expectError:     "object is not satisfied during double check",
			useShortTimeout: true,
		},
		{
			name: "pause running / available sandbox",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = false
				sbx.OwnerReferences = GetSbsOwnerReference()
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateAvailable, state, reason)
			},
			expectedState: v1alpha1.SandboxStateAvailable,
			expectError:   "sandbox is not pausable, reason: SandboxStateNotAllowed",
		},
		{
			name: "pause already paused sandbox",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)
			},
			expectedState: v1alpha1.SandboxStatePaused,
			expectError:   "",
		},
		{
			name: "pause killing sandbox",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxTerminating
				sbx.Spec.Paused = false
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateDead, state, reason)
			},
			expectedState: v1alpha1.SandboxStateDead,
			expectError:   "sandbox is not pausable, reason: SandboxPhaseNotAllowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{
				RunCommandResult: runtime.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			})
			defer server.Close()

			now := time.Now()
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeURL: server.URL,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
				Spec: v1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: now.Add(2 * time.Hour)},
					PauseTime:    &metav1.Time{Time: now.Add(time.Hour)},
				},
			}

			tt.initSandbox(sandbox)

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, sandbox)
			time.Sleep(10 * time.Millisecond)

			s := AsSandbox(sandbox, cache)
			opts := infra.PauseOptions{
				Timeout: &timeout.Options{
					ShutdownTime: now.Add(time.Hour),
					PauseTime:    now.Add(time.Minute),
				},
			}

			if tt.simulatePauseCompleted {
				mockMgr := cache.GetMockManager()
				mockMgr.AddWaitReconcileKey(sandbox)
				modified := s.Sandbox.DeepCopy()
				mergeFrom := ctrl.MergeFrom(s.Sandbox)
				time.AfterFunc(20*time.Millisecond, func() {
					modified.Status.Phase = v1alpha1.SandboxPaused
					SetSandboxCondition(modified, string(v1alpha1.SandboxConditionPaused), metav1.ConditionTrue, "Pause", "Paused by user")
					require.NoError(t, fc.Status().Patch(t.Context(), modified, mergeFrom))
				})
			}

			var pauseCtx context.Context
			var pauseCancel context.CancelFunc
			if tt.useShortTimeout {
				pauseCtx, pauseCancel = context.WithTimeout(t.Context(), 150*time.Millisecond)
			} else {
				pauseCtx, pauseCancel = context.WithTimeout(t.Context(), 5*time.Second)
			}
			defer pauseCancel()

			err = s.Pause(pauseCtx, opts)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)

			state, reason := sandboxutils.GetSandboxState(s.Sandbox)
			assert.Equal(t, tt.expectedState, state, reason)
			assert.True(t, s.Sandbox.Spec.Paused)
			if tt.expectTimeoutUpdate && opts.Timeout != nil && !opts.Timeout.ShutdownTime.IsZero() {
				// milliseconds will be removed by k8s
				assert.WithinDuration(t, opts.Timeout.ShutdownTime, s.Sandbox.Spec.ShutdownTime.Time, time.Second)
			}
			if tt.expectTimeoutUpdate && opts.Timeout != nil && !opts.Timeout.PauseTime.IsZero() {
				assert.WithinDuration(t, opts.Timeout.PauseTime, s.Sandbox.Spec.PauseTime.Time, time.Second)
			}
		})
	}
}

func TestSandbox_PauseSkipsSideEffectsWhenLatestAlreadyPaused(t *testing.T) {
	tests := []struct {
		name string
		opts infra.PauseOptions
	}{
		{
			name: "does not update timeout when latest already paused",
			opts: infra.PauseOptions{
				Timeout: &timeout.Options{
					ShutdownTime: time.Now().Add(3 * time.Hour),
					PauseTime:    time.Now().Add(90 * time.Minute),
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{},
				},
				Spec: v1alpha1.SandboxSpec{
					Paused:       false,
					ShutdownTime: &metav1.Time{Time: now.Add(2 * time.Hour)},
					PauseTime:    &metav1.Time{Time: now.Add(time.Hour)},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
				},
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, sandbox)
			time.Sleep(10 * time.Millisecond)

			key := types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}
			var clientGets atomic.Int32
			cacheClient, ok := cache.GetClient().(ctrl.WithWatch)
			require.True(t, ok)
			client := interceptor.NewClient(cacheClient, interceptor.Funcs{
				Get: func(ctx context.Context, c ctrl.WithWatch, key ctrl.ObjectKey, obj ctrl.Object, opts ...ctrl.GetOption) error {
					if clientGets.Add(1) != 1 {
						return c.Get(ctx, key, obj, opts...)
					}
					var current v1alpha1.Sandbox
					if err := fc.Get(ctx, key, &current); err != nil {
						return err
					}
					modified := current.DeepCopy()
					modified.Spec.Paused = true
					if err := fc.Patch(ctx, modified, ctrl.MergeFrom(&current)); err != nil {
						return err
					}
					return c.Get(ctx, key, obj, opts...)
				},
			})
			s := AsSandbox(sandbox.DeepCopy(), &apiReaderOverrideCache{
				Provider: cache,
				client:   client,
			})
			mockMgr := cache.GetMockManager()
			mockMgr.AddWaitReconcileKey(sandbox)
			modified := sandbox.DeepCopy()
			mergeFrom := ctrl.MergeFrom(sandbox)
			patchErrCh := make(chan error, 1)
			time.AfterFunc(20*time.Millisecond, func() {
				modified.Status.Phase = v1alpha1.SandboxPaused
				modified.Status.Conditions = []metav1.Condition{
					{Type: string(v1alpha1.SandboxConditionPaused), Status: metav1.ConditionTrue, Reason: "Pause"},
				}
				patchErrCh <- fc.Status().Patch(t.Context(), modified, mergeFrom)
			})

			pauseCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, s.Pause(pauseCtx, tt.opts))
			select {
			case err := <-patchErrCh:
				require.NoError(t, err)
			default:
			}

			var updatedSbx v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), key, &updatedSbx))
			assert.True(t, updatedSbx.Spec.Paused)
			require.NotNil(t, updatedSbx.Spec.ShutdownTime)
			require.NotNil(t, updatedSbx.Spec.PauseTime)
			assert.WithinDuration(t, now.Add(2*time.Hour), updatedSbx.Spec.ShutdownTime.Time, time.Second)
			assert.WithinDuration(t, now.Add(time.Hour), updatedSbx.Spec.PauseTime.Time, time.Second)
		})
	}
}

func TestSandbox_PauseRefreshesBeforePreconditionChecks(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "local running wrapper treats latest paused sandbox as already paused",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latest := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{},
				},
				Spec: v1alpha1.SandboxSpec{
					Paused: true,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionPaused),
							Status: metav1.ConditionTrue,
						},
					},
				},
			}
			local := latest.DeepCopy()
			local.Spec.Paused = false
			local.Status.Phase = v1alpha1.SandboxRunning
			local.Status.Conditions = []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, latest)
			time.Sleep(10 * time.Millisecond)

			s := AsSandbox(local, cache)
			err = s.Pause(t.Context(), infra.PauseOptions{})
			require.NoError(t, err)

			var updatedSbx v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: latest.Namespace, Name: latest.Name}, &updatedSbx))
			assert.True(t, updatedSbx.Spec.Paused)
		})
	}
}

func TestSandbox_PauseConflictsWithActiveResumeWaitHook(t *testing.T) {
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.SandboxSpec{
			Paused: true,
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPaused,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())
	CreateSandboxWithStatus(t, fc, sandbox)
	time.Sleep(10 * time.Millisecond)

	waitCtx, waitCancel := context.WithCancel(t.Context())
	defer waitCancel()
	waitDone := make(chan error, 1)
	go func() {
		task, taskErr := cache.NewSandboxResumeTask(waitCtx, sandbox)
		if taskErr != nil {
			waitDone <- taskErr
			return
		}
		defer task.Release()
		waitDone <- task.Wait(time.Hour)
	}()

	key := cacheutils.WaitHookKey[*v1alpha1.Sandbox](sandbox)
	require.Eventually(t, func() bool {
		_, exists := cache.GetWaitHooks().Load(key)
		return exists
	}, 2*time.Second, 10*time.Millisecond)

	s := AsSandbox(sandbox, cache)
	err = s.Pause(t.Context(), infra.PauseOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another action(Resume)'s wait task already exists")

	if val, ok := cache.GetWaitHooks().Load(key); ok {
		val.(*cacheutils.WaitEntry[*v1alpha1.Sandbox]).Close()
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("resume wait hook did not release")
	}
}

func TestSandbox_PauseConflictsWithActiveResumeWaitHookBeforeMutation(t *testing.T) {
	tests := []struct {
		name              string
		initialSpecPaused bool
		expectError       string
	}{
		{
			name:              "active resume hook rejects pause without mutating spec paused",
			initialSpecPaused: false,
			expectError:       "another action(Resume)'s wait task already exists",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{},
				},
				Spec: v1alpha1.SandboxSpec{
					Paused: tt.initialSpecPaused,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
				},
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, sandbox)
			time.Sleep(10 * time.Millisecond)

			resumeTask, taskErr := cache.NewSandboxResumeTask(t.Context(), sandbox)
			require.NoError(t, taskErr)
			defer resumeTask.Release()

			s := AsSandbox(sandbox, cache)
			err = s.Pause(t.Context(), infra.PauseOptions{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)

			var updatedSbx v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}, &updatedSbx))
			assert.Equal(t, tt.initialSpecPaused, updatedSbx.Spec.Paused)
		})
	}
}

//goland:noinspection GoDeprecation
func TestSandbox_Resume(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name                    string
		initSandbox             func(sbx *v1alpha1.Sandbox)
		expectedState           string
		expectError             string
		initErrCode             int
		withInitRuntime         bool
		simulateResumeCompleted bool // use time.AfterFunc to simulate underlying resume completion
		useShortTimeout         bool // simulate underlying not completing by using short context timeout
	}{
		{
			name: "resume paused sandbox - operation completed successfully",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)
			},
			expectedState:           v1alpha1.SandboxStateRunning,
			expectError:             "",
			simulateResumeCompleted: true,
		},
		{
			name: "resume paused sandbox - underlying operation not completed",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)
			},
			expectedState:   "",
			expectError:     "object is not satisfied during double check",
			useShortTimeout: true,
		},
		{
			name: "resume paused sandbox with init runtime success",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)
			},
			expectedState:           v1alpha1.SandboxStateRunning,
			expectError:             "",
			initErrCode:             0,
			withInitRuntime:         true,
			simulateResumeCompleted: true,
		},
		{
			name: "resume paused sandbox with init runtime 401 (ReInit success)",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)
			},
			expectedState:           v1alpha1.SandboxStateRunning,
			expectError:             "",
			initErrCode:             401,
			withInitRuntime:         true,
			simulateResumeCompleted: true,
		},
		{
			name: "resume paused sandbox with init runtime 500 error",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				})
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)
			},
			expectedState:           "",
			expectError:             "failed to perform ReInit after resume",
			initErrCode:             500,
			withInitRuntime:         true,
			simulateResumeCompleted: true,
		},
		{
			name: "resume pausing sandbox",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxPaused
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionFalse,
				})
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStatePaused, state, reason)
			},
			expectedState: "",
			expectError:   "sandbox is not resumable, reason: SandboxIsPausing",
		},
		{
			name: "resume already running sandbox",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				})
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateRunning, state, reason)
			},
			expectedState: v1alpha1.SandboxStateRunning,
			expectError:   "",
		},
		{
			name: "resume killing sandbox",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxTerminating
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateDead, state, reason)
			},
			expectedState: "",
			expectError:   "sandbox is not resumable, reason: SandboxPhaseNotAllowed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shutdownTime := time.Now().Add(2 * time.Hour)
			pauseTime := time.Now().Add(1 * time.Hour)
			serverOpts := testutils.TestRuntimeServerOptions{
				RunCommandResult: runtime.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
				InitErrCode:           tt.initErrCode,
			}
			server := testutils.NewTestRuntimeServer(serverOpts)
			defer server.Close()

			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeURL: server.URL,
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
				Spec: v1alpha1.SandboxSpec{
					ShutdownTime: &metav1.Time{Time: shutdownTime},
					PauseTime:    &metav1.Time{Time: pauseTime},
				},
			}

			if tt.withInitRuntime {
				initRuntimeOpts := config.InitRuntimeOptions{
					EnvVars: map[string]string{
						"TEST_VAR": "test_value",
					},
					AccessToken: "test-token",
				}
				initRuntimeJSON, err := json.Marshal(initRuntimeOpts)
				require.NoError(t, err)
				sandbox.Annotations[v1alpha1.AnnotationInitRuntimeRequest] = string(initRuntimeJSON)
			}

			tt.initSandbox(sandbox)

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, sandbox)
			time.Sleep(10 * time.Millisecond)

			s := AsSandbox(sandbox, cache)

			if tt.simulateResumeCompleted {
				// Register sandbox key for wait simulation
				mockMgr := cache.GetMockManager()
				mockMgr.AddWaitReconcileKey(sandbox)
				modified := s.Sandbox.DeepCopy()
				mergeFrom := ctrl.MergeFrom(s.Sandbox)
				time.AfterFunc(20*time.Millisecond, func() {
					modified.Status.Phase = v1alpha1.SandboxRunning
					modified.Status.Conditions = []metav1.Condition{
						{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
					}
					_ = fc.Status().Patch(t.Context(), modified, mergeFrom)
				})
			}

			var resumeCtx context.Context
			var resumeCancel context.CancelFunc
			if tt.useShortTimeout {
				resumeCtx, resumeCancel = context.WithTimeout(t.Context(), 150*time.Millisecond)
			} else {
				resumeCtx, resumeCancel = context.WithTimeout(t.Context(), 5*time.Second)
			}
			defer resumeCancel()

			err = s.Resume(resumeCtx, infra.ResumeOptions{})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)
			var updatedSbx v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updatedSbx))

			state, reason := sandboxutils.GetSandboxState(&updatedSbx)
			assert.Equal(t, tt.expectedState, state, reason)
			assert.False(t, updatedSbx.Spec.Paused)
			require.NotNil(t, updatedSbx.Spec.ShutdownTime)
			require.NotNil(t, updatedSbx.Spec.PauseTime)
			assert.WithinDuration(t, shutdownTime, updatedSbx.Spec.ShutdownTime.Time, time.Second)
			assert.WithinDuration(t, pauseTime, updatedSbx.Spec.PauseTime.Time, time.Second)
		})
	}
}

func TestSandbox_ResumeConflictsWithActivePauseWaitHook(t *testing.T) {
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.SandboxSpec{
			Paused: false,
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())
	CreateSandboxWithStatus(t, fc, sandbox)
	time.Sleep(10 * time.Millisecond)

	waitCtx, waitCancel := context.WithCancel(t.Context())
	defer waitCancel()
	waitDone := make(chan error, 1)
	go func() {
		task, taskErr := cache.NewSandboxPauseTask(waitCtx, sandbox)
		if taskErr != nil {
			waitDone <- taskErr
			return
		}
		defer task.Release()
		waitDone <- task.Wait(time.Hour)
	}()

	key := cacheutils.WaitHookKey[*v1alpha1.Sandbox](sandbox)
	require.Eventually(t, func() bool {
		_, exists := cache.GetWaitHooks().Load(key)
		return exists
	}, 2*time.Second, 10*time.Millisecond)

	s := AsSandbox(sandbox, cache)
	err = s.Resume(t.Context(), infra.ResumeOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another action(Pause)'s wait task already exists")

	if val, ok := cache.GetWaitHooks().Load(key); ok {
		val.(*cacheutils.WaitEntry[*v1alpha1.Sandbox]).Close()
	}
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("pause wait hook did not release")
	}
}

func TestSandbox_ResumeConflictsWithActivePauseWaitHookBeforeMutation(t *testing.T) {
	tests := []struct {
		name              string
		initialSpecPaused bool
		expectError       string
	}{
		{
			name:              "active pause hook rejects resume without mutating spec paused",
			initialSpecPaused: true,
			expectError:       "another action(Pause)'s wait task already exists",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{},
				},
				Spec: v1alpha1.SandboxSpec{
					Paused: tt.initialSpecPaused,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionPaused),
							Status: metav1.ConditionTrue,
						},
					},
				},
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, sandbox)
			time.Sleep(10 * time.Millisecond)

			pauseTask, taskErr := cache.NewSandboxPauseTask(t.Context(), sandbox)
			require.NoError(t, taskErr)
			defer pauseTask.Release()

			s := AsSandbox(sandbox, cache)
			err = s.Resume(t.Context(), infra.ResumeOptions{})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)

			var updatedSbx v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}, &updatedSbx))
			assert.Equal(t, tt.initialSpecPaused, updatedSbx.Spec.Paused)
		})
	}
}

func TestSandbox_ResumeRefreshesBeforePreconditionChecksAndUpdatesLatest(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "local running wrapper resumes latest paused sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latest := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{},
				},
				Spec: v1alpha1.SandboxSpec{
					Paused: true,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionPaused),
							Status: metav1.ConditionTrue,
						},
					},
				},
			}
			local := latest.DeepCopy()
			local.Spec.Paused = false
			local.Status.Phase = v1alpha1.SandboxRunning
			local.Status.Conditions = []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, latest)
			time.Sleep(10 * time.Millisecond)

			s := AsSandbox(local, cache)
			mockMgr := cache.GetMockManager()
			mockMgr.AddWaitReconcileKey(latest)
			patchErrCh := make(chan error, 1)
			time.AfterFunc(20*time.Millisecond, func() {
				var current v1alpha1.Sandbox
				if err := fc.Get(t.Context(), types.NamespacedName{Namespace: latest.Namespace, Name: latest.Name}, &current); err != nil {
					patchErrCh <- err
					return
				}
				modified := current.DeepCopy()
				modified.Status.Phase = v1alpha1.SandboxRunning
				modified.Status.Conditions = []metav1.Condition{
					{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
				}
				patchErrCh <- fc.Status().Patch(t.Context(), modified, ctrl.MergeFrom(&current))
			})

			resumeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, s.Resume(resumeCtx, infra.ResumeOptions{}))
			select {
			case err := <-patchErrCh:
				require.NoError(t, err)
			default:
			}

			var updatedSbx v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: latest.Namespace, Name: latest.Name}, &updatedSbx))
			assert.False(t, updatedSbx.Spec.Paused)
			state, reason := sandboxutils.GetSandboxState(&updatedSbx)
			assert.Equal(t, v1alpha1.SandboxStateRunning, state, reason)
		})
	}
}

func TestSandbox_ResumeAllowsAlreadyResumedLatestForStalePausedWrapper(t *testing.T) {
	tests := []struct {
		name string
	}{
		{
			name: "local paused wrapper treats latest running sandbox as already resumed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			latest := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{},
				},
				Spec: v1alpha1.SandboxSpec{
					Paused: false,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					},
				},
			}
			local := latest.DeepCopy()
			local.Spec.Paused = true
			local.Status.Phase = v1alpha1.SandboxPaused
			local.Status.Conditions = []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				},
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, latest)
			time.Sleep(10 * time.Millisecond)

			s := AsSandbox(local, cache)
			resumeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, s.Resume(resumeCtx, infra.ResumeOptions{}))

			var updatedSbx v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: latest.Namespace, Name: latest.Name}, &updatedSbx))
			assert.False(t, updatedSbx.Spec.Paused)
			state, reason := sandboxutils.GetSandboxState(&updatedSbx)
			assert.Equal(t, v1alpha1.SandboxStateRunning, state, reason)
		})
	}
}

func TestSandbox_ResumeSkipsPostResumeOperationsWhenLatestAlreadyUnpaused(t *testing.T) {
	tests := []struct {
		name string
		opts infra.ResumeOptions
	}{
		{
			name: "skips post-resume operations when latest spec is already unpaused",
			opts: infra.ResumeOptions{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var initCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/init" {
					http.NotFound(w, r)
					return
				}
				initCalls.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			initRuntimeOpts := config.InitRuntimeOptions{
				EnvVars: map[string]string{
					"TEST_VAR": "test_value",
				},
				AccessToken: "test-token",
			}
			initRuntimeJSON, err := json.Marshal(initRuntimeOpts)
			require.NoError(t, err)

			shutdownTime := time.Now().Add(2 * time.Hour)
			pauseTime := time.Now().Add(time.Hour)
			latest := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeURL:         server.URL,
						v1alpha1.AnnotationInitRuntimeRequest: string(initRuntimeJSON),
					},
				},
				Spec: v1alpha1.SandboxSpec{
					Paused:       false,
					ShutdownTime: &metav1.Time{Time: shutdownTime},
					PauseTime:    &metav1.Time{Time: pauseTime},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionPaused),
							Status: metav1.ConditionTrue,
						},
					},
				},
			}
			local := latest.DeepCopy()
			local.Spec.Paused = true

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			require.NoError(t, cache.Run(t.Context()))
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, latest)
			time.Sleep(10 * time.Millisecond)

			s := AsSandbox(local, cache)
			mockMgr := cache.GetMockManager()
			mockMgr.AddWaitReconcileKey(latest)
			patchErrCh := make(chan error, 1)
			time.AfterFunc(20*time.Millisecond, func() {
				var current v1alpha1.Sandbox
				if err := fc.Get(t.Context(), types.NamespacedName{Namespace: latest.Namespace, Name: latest.Name}, &current); err != nil {
					patchErrCh <- err
					return
				}
				modified := current.DeepCopy()
				modified.Status.Phase = v1alpha1.SandboxRunning
				modified.Status.Conditions = []metav1.Condition{
					{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue, Reason: "Resume"},
				}
				patchErrCh <- fc.Status().Patch(t.Context(), modified, ctrl.MergeFrom(&current))
			})

			resumeCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, s.Resume(resumeCtx, tt.opts))
			select {
			case err := <-patchErrCh:
				require.NoError(t, err)
			default:
			}

			var updatedSbx v1alpha1.Sandbox
			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: latest.Namespace, Name: latest.Name}, &updatedSbx))
			assert.False(t, updatedSbx.Spec.Paused)
			assert.Equal(t, int32(0), initCalls.Load())
		})
	}
}

func TestSandbox_ResumePreservesTimeoutOnError(t *testing.T) {
	shutdownTime := time.Now().Add(2 * time.Hour)
	pauseTime := time.Now().Add(1 * time.Hour)
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
		},
		Spec: v1alpha1.SandboxSpec{
			Paused:       true,
			ShutdownTime: &metav1.Time{Time: shutdownTime},
			PauseTime:    &metav1.Time{Time: pauseTime},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPaused,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	require.NoError(t, cache.Run(t.Context()))
	defer cache.Stop(t.Context())
	CreateSandboxWithStatus(t, fc, sandbox)
	time.Sleep(10 * time.Millisecond)

	s := AsSandbox(sandbox, cache)
	resumeCtx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()

	err = s.Resume(resumeCtx, infra.ResumeOptions{})
	require.Error(t, err)

	var updatedSbx v1alpha1.Sandbox
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: "test-sandbox"}, &updatedSbx))
	assert.False(t, updatedSbx.Spec.Paused)
	require.NotNil(t, updatedSbx.Spec.ShutdownTime)
	require.NotNil(t, updatedSbx.Spec.PauseTime)
	assert.WithinDuration(t, shutdownTime, updatedSbx.Spec.ShutdownTime.Time, time.Second)
	assert.WithinDuration(t, pauseTime, updatedSbx.Spec.PauseTime.Time, time.Second)
}
