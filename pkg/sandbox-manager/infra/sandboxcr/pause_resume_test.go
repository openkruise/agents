package sandboxcr

import (
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime/pkg/client"
)

//goland:noinspection GoDeprecation
func TestSandbox_SetPause(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name          string
		initSandbox   func(sbx *v1alpha1.Sandbox)
		operatePause  bool
		expectPaused  bool
		expectedState string
		expectError   bool
	}{
		{
			name: "pause running / running sandbox",
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
			operatePause:  true,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStatePaused,
			expectError:   false,
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
			operatePause:  true,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStateAvailable,
			expectError:   true,
		},
		{
			name: "resume paused / paused sandbox",
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
			operatePause:  false,
			expectPaused:  false,
			expectedState: v1alpha1.SandboxStateRunning,
			expectError:   false,
		},
		{
			name: "resume paused / pausing sandbox",
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
			operatePause:  false,
			expectPaused:  false,
			expectedState: v1alpha1.SandboxStateRunning,
			expectError:   true,
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
			operatePause:  true,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStatePaused,
			expectError:   true,
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
			operatePause:  false,
			expectPaused:  false,
			expectedState: v1alpha1.SandboxStateRunning,
			expectError:   true,
		},
		{
			name: "resume killing sandbox",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxTerminating
				sbx.Spec.Paused = true
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateDead, state, reason)
			},
			operatePause:  false,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStateDead,
			expectError:   true,
		},
		{
			name: "pause killing sandbox",
			initSandbox: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Phase = v1alpha1.SandboxTerminating
				sbx.Spec.Paused = false
				state, reason := sandboxutils.GetSandboxState(sbx)
				assert.Equal(t, v1alpha1.SandboxStateDead, state, reason)
			},
			operatePause:  true,
			expectPaused:  true,
			expectedState: v1alpha1.SandboxStateDead,
			expectError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Now()
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-sandbox",
					Namespace:   "default",
					Labels:      map[string]string{},
					Annotations: map[string]string{},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					PodInfo: v1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
			}
			tt.initSandbox(sandbox)

			cache, client := NewTestCache(t)
			CreateSandboxWithStatus(t, client, sandbox)
			time.Sleep(10 * time.Millisecond)

			s := AsSandboxForTest(sandbox, client, cache)
			opts := infra.PauseOptions{
				Timeout: &infra.TimeoutOptions{
					ShutdownTime: now.Add(time.Hour),
					PauseTime:    now.Add(time.Minute),
				},
			}
			var err error
			if tt.operatePause {
				err = s.Pause(t.Context(), opts)
				if err == nil {
					patch := ctrl.MergeFrom(s.Sandbox.DeepCopy())
					s.Status.Phase = v1alpha1.SandboxPaused
					data, err := patch.Data(s.Sandbox)
					assert.NoError(t, err)
					_, err = client.ApiV1alpha1().Sandboxes("default").Patch(
						t.Context(), s.Name, types.MergePatchType, data, metav1.PatchOptions{})
					assert.NoError(t, err)
				}
			} else {
				if !tt.expectError {
					time.AfterFunc(20*time.Millisecond, func() {
						patch := ctrl.MergeFrom(s.Sandbox)
						updated := s.Sandbox.DeepCopy()
						updated.Status.Phase = v1alpha1.SandboxRunning
						SetSandboxCondition(updated, string(v1alpha1.SandboxConditionReady), metav1.ConditionTrue, "Resume", "")
						data, err := patch.Data(updated)
						assert.NoError(t, err)
						_, err = client.ApiV1alpha1().Sandboxes("default").Patch(
							t.Context(), s.Name, types.MergePatchType, data, metav1.PatchOptions{})
						assert.NoError(t, err)
					})
				}
				err = s.Resume(t.Context())
			}
			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)
			updatedSbx, err := client.ApiV1alpha1().Sandboxes("default").Get(t.Context(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			state, reason := sandboxutils.GetSandboxState(updatedSbx)
			assert.Equal(t, tt.expectedState, state, reason)
			assert.Equal(t, tt.operatePause, updatedSbx.Spec.Paused)

			if tt.operatePause {
				if !opts.Timeout.ShutdownTime.IsZero() {
					// milliseconds will be removed by k8s
					assert.WithinDuration(t, opts.Timeout.ShutdownTime, updatedSbx.Spec.ShutdownTime.Time, time.Second)
				}
				if !opts.Timeout.PauseTime.IsZero() {
					assert.WithinDuration(t, opts.Timeout.PauseTime, updatedSbx.Spec.PauseTime.Time, time.Second)
				}
			} else {
				assert.Nil(t, updatedSbx.Spec.ShutdownTime)
				assert.Nil(t, updatedSbx.Spec.PauseTime)
			}
		})
	}
}

// TestSandbox_ResumeConcurrent tests concurrent resume operations on the same sandbox
func TestSandbox_ResumeConcurrent(t *testing.T) {
	utils.InitLogOutput()
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-sandbox",
			Namespace:   "default",
			Labels:      map[string]string{},
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

	cache, client := NewTestCache(t)
	CreateSandboxWithStatus(t, client, sandbox)
	time.Sleep(10 * time.Millisecond)

	// Channel to collect results from goroutines
	resultCh := make(chan error, 3)

	start := time.Now()
	// Start three goroutines calling Resume
	for i := 0; i < 3; i++ {
		s := AsSandboxForTest(sandbox, client, cache)
		go func() {
			err := s.Resume(t.Context())
			resultCh <- err
		}()
	}

	// After 0.5 seconds, update the sandbox phase to Running and Ready condition to True
	time.AfterFunc(500*time.Millisecond, func() {
		patch := ctrl.MergeFrom(sandbox)
		updated := sandbox.DeepCopy()
		updated.Status.Phase = v1alpha1.SandboxRunning
		SetSandboxCondition(updated, string(v1alpha1.SandboxConditionReady), metav1.ConditionTrue, "Resume", "")
		data, err := patch.Data(updated)
		assert.NoError(t, err)
		_, err = client.ApiV1alpha1().Sandboxes("default").Patch(
			t.Context(), updated.Name, types.MergePatchType, data, metav1.PatchOptions{})
		assert.NoError(t, err)
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
	updatedSbx, err := client.ApiV1alpha1().Sandboxes("default").Get(t.Context(), "test-sandbox", metav1.GetOptions{})
	assert.NoError(t, err)
	state, reason = sandboxutils.GetSandboxState(updatedSbx)
	assert.Equal(t, v1alpha1.SandboxStateRunning, state, reason)
	assert.False(t, updatedSbx.Spec.Paused)
}
