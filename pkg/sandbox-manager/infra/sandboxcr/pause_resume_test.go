package sandboxcr

import (
	"context"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
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

			cache, client := NewTestCache()
			CreateSandboxWithStatus(t, client, sandbox)
			time.Sleep(10 * time.Millisecond)

			s := AsSandbox(sandbox, client, cache)
			var err error
			if tt.operatePause {
				err = s.Pause(context.Background())
				if err == nil {
					patch := ctrl.MergeFrom(s.Sandbox.DeepCopy())
					s.Status.Phase = v1alpha1.SandboxPaused
					data, err := patch.Data(s.Sandbox)
					assert.NoError(t, err)
					_, err = client.ApiV1alpha1().Sandboxes("default").Patch(
						context.Background(), s.Name, types.MergePatchType, data, metav1.PatchOptions{})
					assert.NoError(t, err)
				}
			} else {
				if !tt.expectError {
					time.AfterFunc(20*time.Millisecond, func() {
						patch := ctrl.MergeFrom(s.Sandbox.DeepCopy())
						s.Status.Phase = v1alpha1.SandboxRunning
						SetSandboxCondition(s.Sandbox, string(v1alpha1.SandboxConditionReady), metav1.ConditionTrue, "Resume", "")
						data, err := patch.Data(s.Sandbox)
						assert.NoError(t, err)
						_, err = client.ApiV1alpha1().Sandboxes("default").Patch(
							context.Background(), s.Name, types.MergePatchType, data, metav1.PatchOptions{})
						assert.NoError(t, err)
					})
				}
				err = s.Resume(context.Background())
			}
			if tt.expectError {
				assert.Error(t, err)
				return
			} else {
				assert.NoError(t, err)
			}

			updatedSbx, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			state, reason := sandboxutils.GetSandboxState(updatedSbx)
			assert.Equal(t, tt.expectedState, state, reason)
			assert.Equal(t, tt.operatePause, updatedSbx.Spec.Paused)
		})
	}
}
