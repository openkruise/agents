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
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/cache/controllers"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	testutils "github.com/openkruise/agents/test/utils"
)

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
			expectError:   "pausing is only available for running state",
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
			expectError:   "sandbox is not in running phase",
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
			expectError:   "sandbox is not in running phase",
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
				Timeout: &infra.TimeoutOptions{
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
			if !opts.Timeout.ShutdownTime.IsZero() {
				// milliseconds will be removed by k8s
				assert.WithinDuration(t, opts.Timeout.ShutdownTime, s.Sandbox.Spec.ShutdownTime.Time, time.Second)
			}
			if !opts.Timeout.PauseTime.IsZero() {
				assert.WithinDuration(t, opts.Timeout.PauseTime, s.Sandbox.Spec.PauseTime.Time, time.Second)
			}
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
			expectError:   "sandbox is pausing",
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
			expectedState: "",
			expectError:   "resuming is only available for paused state",
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
			expectError:   "resuming is only available for paused state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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

			err = s.Resume(resumeCtx, infra.ResumeOptions{Timeout: &infra.TimeoutOptions{}})

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
			assert.Nil(t, updatedSbx.Spec.ShutdownTime)
			assert.Nil(t, updatedSbx.Spec.PauseTime)
		})
	}
}

func TestSandbox_ResumeBumpsTimeoutsDuringUnpause(t *testing.T) {
	utils.InitLogOutput()
	now := time.Now()
	expired := metav1.NewTime(now.Add(-time.Minute))
	finalPauseTime := now.Add(15 * time.Minute)
	finalShutdownTime := now.Add(20 * time.Minute)

	tests := []struct {
		name         string
		pauseTime    *metav1.Time
		shutdownTime *metav1.Time
		expectError  string
	}{
		{
			name:      "unpause succeeds and wait timeout saves final timeout",
			pauseTime: &expired,
		},
		{
			name:         "only shutdown expired is rejected",
			pauseTime:    nil,
			shutdownTime: &expired,
			expectError:  "ShutdownTimeReached",
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
					Paused:       true,
					PauseTime:    tt.pauseTime,
					ShutdownTime: tt.shutdownTime,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxPaused,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionPaused),
							Status: metav1.ConditionTrue,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
			}

			cache, fc, err := cachetest.NewTestCache(t)
			require.NoError(t, err)
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, sandbox)

			beforeResume := time.Now()
			s := AsSandbox(sandbox, cache)
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			err = s.Resume(ctx, infra.ResumeOptions{
				Timeout: &infra.TimeoutOptions{
					PauseTime:    finalPauseTime,
					ShutdownTime: finalShutdownTime,
				},
			})
			require.Error(t, err)

			var updatedSbx v1alpha1.Sandbox
			if tt.expectError != "" {
				assert.Contains(t, err.Error(), tt.expectError)
				require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: sandbox.Name}, &updatedSbx))
				assert.True(t, updatedSbx.Spec.Paused)
				if tt.pauseTime == nil {
					assert.Nil(t, updatedSbx.Spec.PauseTime)
				} else {
					require.NotNil(t, updatedSbx.Spec.PauseTime)
					assert.WithinDuration(t, tt.pauseTime.Time, updatedSbx.Spec.PauseTime.Time, time.Second)
				}
				if tt.shutdownTime == nil {
					assert.Nil(t, updatedSbx.Spec.ShutdownTime)
				} else {
					require.NotNil(t, updatedSbx.Spec.ShutdownTime)
					assert.WithinDuration(t, tt.shutdownTime.Time, updatedSbx.Spec.ShutdownTime.Time, time.Second)
				}
				return
			}

			require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: sandbox.Name}, &updatedSbx))
			assert.False(t, updatedSbx.Spec.Paused)
			maxResumeCost := time.Since(beforeResume) + time.Second
			require.NotNil(t, updatedSbx.Spec.PauseTime)
			assert.WithinDuration(t, finalPauseTime, updatedSbx.Spec.PauseTime.Time, maxResumeCost)
			require.NotNil(t, updatedSbx.Spec.ShutdownTime)
			assert.WithinDuration(t, finalShutdownTime, updatedSbx.Spec.ShutdownTime.Time, maxResumeCost)
		})
	}
}

func TestSandbox_ResumeSavesFinalTimeoutBeforeReInit(t *testing.T) {
	utils.InitLogOutput()
	server := testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{
		RunCommandResult: runtime.RunCommandResult{
			PID:    1,
			Exited: true,
		},
		RunCommandImmediately: true,
		InitErrCode:           500,
	})
	defer server.Close()

	initRuntimeOpts := config.InitRuntimeOptions{
		EnvVars:     map[string]string{"TEST_VAR": "test_value"},
		AccessToken: "test-token",
	}
	initRuntimeJSON, err := json.Marshal(initRuntimeOpts)
	require.NoError(t, err)

	expired := metav1.NewTime(time.Now().Add(-time.Minute))
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL:         server.URL,
				v1alpha1.AnnotationInitRuntimeRequest: string(initRuntimeJSON),
				v1alpha1.AnnotationOwner:              "test-user",
			},
		},
		Spec: v1alpha1.SandboxSpec{
			Paused:    true,
			PauseTime: &expired,
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPaused,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				},
			},
			PodInfo: v1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
		},
	}

	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	defer cache.Stop(t.Context())
	CreateSandboxWithStatus(t, fc, sandbox)

	finalTimeout := infra.TimeoutOptions{
		ShutdownTime: time.Now().Add(2 * time.Hour),
		PauseTime:    time.Now().Add(15 * time.Minute),
	}
	time.AfterFunc(20*time.Millisecond, func() {
		markSandboxRunningAndSignalResumeWait(fc, cache, sandbox)
	})

	s := AsSandbox(sandbox, cache)
	err = s.Resume(t.Context(), infra.ResumeOptions{Timeout: &finalTimeout})
	require.Error(t, err)

	updatedSbx := &v1alpha1.Sandbox{}
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "default", Name: sandbox.Name}, updatedSbx))
	require.NotNil(t, updatedSbx.Spec.ShutdownTime)
	require.NotNil(t, updatedSbx.Spec.PauseTime)
	assert.WithinDuration(t, finalTimeout.ShutdownTime, updatedSbx.Spec.ShutdownTime.Time, time.Second)
	assert.WithinDuration(t, finalTimeout.PauseTime, updatedSbx.Spec.PauseTime.Time, time.Second)
}

func TestSandbox_ResumeDeferredSaveTimeoutFailure(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name        string
		reInitFails bool
		expectError string
	}{
		{
			name:        "main flow succeeds returns save timeout error",
			expectError: "failed to save final resume timeout",
		},
		{
			name:        "main flow fails returns main error when save timeout also fails",
			reInitFails: true,
			expectError: "failed to perform ReInit after resume",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var annotations map[string]string
			var server *httptest.Server
			if tt.reInitFails {
				server = testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{
					RunCommandResult: runtime.RunCommandResult{
						PID:    1,
						Exited: true,
					},
					RunCommandImmediately: true,
					InitErrCode:           500,
				})
				defer server.Close()

				initRuntimeOpts := config.InitRuntimeOptions{
					EnvVars:     map[string]string{"TEST_VAR": "test_value"},
					AccessToken: "test-token",
				}
				initRuntimeJSON, err := json.Marshal(initRuntimeOpts)
				require.NoError(t, err)
				annotations = map[string]string{
					v1alpha1.AnnotationRuntimeURL:         server.URL,
					v1alpha1.AnnotationInitRuntimeRequest: string(initRuntimeJSON),
					v1alpha1.AnnotationOwner:              "test-user",
				}
			}

			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxIsClaimed: "true",
					},
					Annotations: annotations,
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
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
			}

			var sandboxUpdates int
			cache, fc := newResumeTestCacheWithUpdateInterceptor(t, func(ctx context.Context, c ctrl.WithWatch, obj ctrl.Object, opts ...ctrl.UpdateOption) error {
				if _, ok := obj.(*v1alpha1.Sandbox); ok {
					sandboxUpdates++
					if sandboxUpdates == 2 {
						return fmt.Errorf("injected save timeout failure")
					}
				}
				latest := obj.DeepCopyObject().(ctrl.Object)
				if err := c.Get(ctx, types.NamespacedName{Name: obj.GetName(), Namespace: obj.GetNamespace()}, latest); err == nil {
					obj.SetResourceVersion(latest.GetResourceVersion())
				}
				return c.Update(ctx, obj, opts...)
			})
			defer cache.Stop(t.Context())
			CreateSandboxWithStatus(t, fc, sandbox)

			time.AfterFunc(20*time.Millisecond, func() {
				markSandboxRunningAndSignalResumeWait(fc, cache, sandbox)
			})

			finalTimeout := infra.TimeoutOptions{
				ShutdownTime: time.Now().Add(2 * time.Hour),
				PauseTime:    time.Now().Add(15 * time.Minute),
			}
			s := AsSandbox(sandbox, cache)
			err := s.Resume(t.Context(), infra.ResumeOptions{Timeout: &finalTimeout})
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Equal(t, 2, sandboxUpdates)
			if tt.reInitFails {
				assert.NotContains(t, err.Error(), "failed to save final resume timeout")
			}
		})
	}
}

func markSandboxRunningAndSignalResumeWait(fc ctrl.Client, cache *infracache.Cache, sandbox *v1alpha1.Sandbox) {
	updated := &v1alpha1.Sandbox{}
	if getErr := fc.Get(context.Background(), types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name}, updated); getErr != nil {
		return
	}
	updated.Status.Phase = v1alpha1.SandboxRunning
	updated.Status.Conditions = []metav1.Condition{
		{
			Type:   string(v1alpha1.SandboxConditionReady),
			Status: metav1.ConditionTrue,
		},
	}
	if updateErr := fc.Status().Update(context.Background(), updated); updateErr != nil {
		return
	}
	if value, ok := cache.GetWaitHooks().Load(cacheutils.WaitHookKey[*v1alpha1.Sandbox](sandbox)); ok {
		value.(*cacheutils.WaitEntry[*v1alpha1.Sandbox]).Close()
	}
}

func newResumeTestCacheWithUpdateInterceptor(
	t *testing.T,
	update func(context.Context, ctrl.WithWatch, ctrl.Object, ...ctrl.UpdateOption) error,
) (*infracache.Cache, ctrl.Client) {
	t.Helper()
	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, idx := range infracache.GetIndexFuncs() {
		builder = builder.WithIndex(idx.Obj, idx.FieldName, idx.Extract)
	}
	builder = builder.WithStatusSubresource(
		&v1alpha1.Sandbox{},
		&v1alpha1.SandboxSet{},
		&v1alpha1.Checkpoint{},
		&v1alpha1.SandboxClaim{},
		&v1alpha1.SandboxTemplate{},
	)
	builder = builder.WithInterceptorFuncs(interceptor.Funcs{Update: update})
	fakeClient := builder.Build()

	mgrBuilder, err := controllers.NewMockManagerBuilder(t)
	require.NoError(t, err)
	mgr := mgrBuilder.
		WithScheme(scheme).
		WithClient(fakeClient).
		WithWaitSimulation().
		Build()
	testCache, err := infracache.NewCache(mgr)
	require.NoError(t, err)
	mgr.SetWaitHooks(testCache.GetWaitHooks())

	return testCache, fakeClient
}
