package sandboxcr

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	testutils "github.com/openkruise/agents/test/utils"
)

func TestValidateAndInitCloneOptions(t *testing.T) {
	tests := []struct {
		name        string
		opts        infra.CloneSandboxOptions
		expectError string
		expectOpts  infra.CloneSandboxOptions
	}{
		{
			name: "valid options",
			opts: infra.CloneSandboxOptions{
				User:         "test-user",
				CheckPointID: "test-checkpoint",
			},
			expectOpts: infra.CloneSandboxOptions{
				User:             "test-user",
				CheckPointID:     "test-checkpoint",
				WaitReadyTimeout: 0, // will be set to default
			},
		},
		{
			name: "empty user",
			opts: infra.CloneSandboxOptions{
				CheckPointID: "test-checkpoint",
			},
			expectError: "user is required",
		},
		{
			name: "empty checkpoint id",
			opts: infra.CloneSandboxOptions{
				User: "test-user",
			},
			expectError: "checkpoint id is required",
		},
		{
			name: "custom wait ready timeout",
			opts: infra.CloneSandboxOptions{
				User:             "test-user",
				CheckPointID:     "test-checkpoint",
				WaitReadyTimeout: 60 * time.Second,
			},
			expectOpts: infra.CloneSandboxOptions{
				User:             "test-user",
				CheckPointID:     "test-checkpoint",
				WaitReadyTimeout: 60 * time.Second,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ValidateAndInitCloneOptions(tt.opts)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectOpts.User, result.User)
				assert.Equal(t, tt.expectOpts.CheckPointID, result.CheckPointID)
				if tt.opts.WaitReadyTimeout > 0 {
					assert.Equal(t, tt.expectOpts.WaitReadyTimeout, result.WaitReadyTimeout)
				}
			}
		})
	}
}

func TestValidateAndInitCheckpointOptions(t *testing.T) {
	tests := []struct {
		name       string
		opts       infra.CreateCheckpointOptions
		expectOpts infra.CreateCheckpointOptions
	}{
		{
			name:       "default timeout",
			opts:       infra.CreateCheckpointOptions{},
			expectOpts: infra.CreateCheckpointOptions{WaitSuccessTimeout: consts.DefaultWaitCheckpointTimeout},
		},
		{
			name: "custom timeout",
			opts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 60 * time.Second,
			},
			expectOpts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 60 * time.Second,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ValidateAndInitCheckpointOptions(tt.opts)
			assert.Equal(t, tt.expectOpts.WaitSuccessTimeout, result.WaitSuccessTimeout)
		})
	}
}

func TestCloneSandbox(t *testing.T) {
	utils.InitLogOutput()

	checkpointID := "test-checkpoint-123"
	user := "test-user"

	// Define context key types for sandbox override
	type sbxOverrideKey struct{}
	type sbxOverride struct {
		Name        string
		RuntimeURL  string
		AccessToken string
	}

	// Decorator: DefaultCreateSandbox - set sandbox ready after creation
	origCreateSandbox := DefaultCreateSandbox
	DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, client *clients.ClientSet, cache infra.CacheProvider) (*v1alpha1.Sandbox, error) {
		if override, ok := ctx.Value(sbxOverrideKey{}).(sbxOverride); ok {
			if override.Name != "" {
				sbx.Name = override.Name
			}
			if override.RuntimeURL != "" {
				if sbx.Annotations == nil {
					sbx.Annotations = map[string]string{}
				}
				sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = override.RuntimeURL
			}
			if override.AccessToken != "" {
				if sbx.Annotations == nil {
					sbx.Annotations = map[string]string{}
				}
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = override.AccessToken
			}
		}
		created, err := origCreateSandbox(ctx, sbx, client, cache)
		if err != nil {
			return nil, err
		}
		// Update Sandbox status to Ready
		// checkSandboxReady checks: state == SandboxStateRunning && PodIP != ""
		// GetSandboxState requires: Phase == Running, not controlled by SandboxSet, Ready condition is true
		created.Status = v1alpha1.SandboxStatus{
			Phase:              v1alpha1.SandboxRunning,
			ObservedGeneration: created.Generation,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: v1alpha1.SandboxReadyReasonPodReady,
				},
			},
			PodInfo: v1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		}
		created, err = client.ApiV1alpha1().Sandboxes(created.Namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
		if err != nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond) // Wait for informer sync
		return created, nil
	}
	t.Cleanup(func() { DefaultCreateSandbox = origCreateSandbox })

	tests := []struct {
		name        string
		opts        infra.CloneSandboxOptions
		serverOpts  testutils.TestRuntimeServerOptions
		initRuntime *config.InitRuntimeOptions
		sbxOverride sbxOverride
		preProcess  func(t *testing.T, cache *Cache, client *clients.ClientSet)
		postCheck   func(t *testing.T, sbx infra.Sandbox, metrics infra.CloneMetrics)
		expectError string
	}{
		{
			name: "successful clone",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 30 * time.Second,
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			sbxOverride: sbxOverride{Name: "test-sandbox-clone-1"},
			postCheck: func(t *testing.T, sbx infra.Sandbox, metrics infra.CloneMetrics) {
				assert.NotNil(t, sbx)
				assert.Equal(t, user, sbx.GetAnnotations()[v1alpha1.AnnotationOwner])
				assert.Equal(t, checkpointID, sbx.GetLabels()[v1alpha1.LabelSandboxTemplate])
				assert.Equal(t, "true", sbx.GetLabels()[v1alpha1.LabelSandboxIsClaimed])
				assert.NotEmpty(t, sbx.GetAnnotations()[v1alpha1.AnnotationClaimTime])
				// Verify metrics are recorded
				assert.GreaterOrEqual(t, metrics.GetTemplate, time.Duration(0))
				assert.GreaterOrEqual(t, metrics.CreateSandbox, time.Duration(0))
				assert.GreaterOrEqual(t, metrics.WaitReady, time.Duration(0))
				assert.GreaterOrEqual(t, metrics.Total, time.Duration(0))
			},
		},
		{
			name: "clone with modifier",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 30 * time.Second,
				Modifier: func(sbx infra.Sandbox) {
					sbx.SetAnnotations(map[string]string{
						"custom-annotation": "custom-value",
					})
				},
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			sbxOverride: sbxOverride{Name: "test-sandbox-clone-2"},
			postCheck: func(t *testing.T, sbx infra.Sandbox, metrics infra.CloneMetrics) {
				assert.Equal(t, "custom-value", sbx.GetAnnotations()["custom-annotation"])
				assert.Equal(t, user, sbx.GetAnnotations()[v1alpha1.AnnotationOwner])
			},
		},
		{
			name: "re-init runtime success",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 30 * time.Second,
			},
			initRuntime: &config.InitRuntimeOptions{
				AccessToken: "test-access-token",
				EnvVars: map[string]string{
					"VAR1": "value1",
					"VAR2": "value2",
				},
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
				InitErrCode:           0,
			},
			sbxOverride: sbxOverride{Name: "test-sandbox-clone-3"},
			postCheck: func(t *testing.T, sbx infra.Sandbox, metrics infra.CloneMetrics) {
				assert.NotNil(t, sbx)
				assert.GreaterOrEqual(t, metrics.InitRuntime, time.Duration(0))
				// Check runtime init annotations
				assert.Equal(t, "test-access-token", sbx.GetAnnotations()[v1alpha1.AnnotationRuntimeAccessToken])
				assert.NotEmpty(t, sbx.GetAnnotations()[v1alpha1.AnnotationInitRuntimeRequest])
			},
		},
		{
			name: "re-init runtime 401 (ReInit success)",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 30 * time.Second,
			},
			initRuntime: &config.InitRuntimeOptions{
				AccessToken: "test-access-token",
				EnvVars: map[string]string{
					"VAR1": "value1",
				},
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
				InitErrCode:           401,
			},
			sbxOverride: sbxOverride{Name: "test-sandbox-clone-4"},
			postCheck: func(t *testing.T, sbx infra.Sandbox, metrics infra.CloneMetrics) {
				assert.NotNil(t, sbx)
				assert.GreaterOrEqual(t, metrics.InitRuntime, time.Duration(0))
				// Check runtime init annotations
				assert.Equal(t, "test-access-token", sbx.GetAnnotations()[v1alpha1.AnnotationRuntimeAccessToken])
				assert.NotEmpty(t, sbx.GetAnnotations()[v1alpha1.AnnotationInitRuntimeRequest])
			},
		},
		{
			name: "re-init runtime 500 error",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 30 * time.Second,
			},
			initRuntime: &config.InitRuntimeOptions{
				AccessToken: "test-access-token",
				EnvVars: map[string]string{
					"VAR1": "value1",
				},
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
				InitErrCode:           500,
			},
			sbxOverride: sbxOverride{Name: "test-sandbox-clone-5"},
			expectError: "failed to proxy request to sandbox",
		},
		{
			name: "checkpoint not found",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     "non-existent-checkpoint",
				WaitReadyTimeout: 30 * time.Second,
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			expectError: "not found",
		},
		{
			name: "checkpoint without template label",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     "checkpoint-no-template",
				WaitReadyTimeout: 30 * time.Second,
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			preProcess: func(t *testing.T, cache *Cache, client *clients.ClientSet) {
				// Create checkpoint without template label
				cp := &v1alpha1.Checkpoint{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "checkpoint-no-template",
						Namespace: "default",
						Labels:    map[string]string{},
					},
					Status: v1alpha1.CheckpointStatus{
						CheckpointId: "checkpoint-no-template",
					},
				}
				_, err := client.ApiV1alpha1().Checkpoints("default").Create(context.Background(), cp, metav1.CreateOptions{})
				require.NoError(t, err)
				// Wait for checkpoint to be cached
				require.Eventually(t, func() bool {
					_, err := cache.GetCheckpoint("checkpoint-no-template")
					return err == nil
				}, time.Second, 10*time.Millisecond)
			},
			expectError: "not found",
		},
		{
			name: "template not found",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     "checkpoint-no-sbt",
				WaitReadyTimeout: 30 * time.Second,
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:    1,
					Exited: true,
				},
				RunCommandImmediately: true,
			},
			preProcess: func(t *testing.T, cache *Cache, client *clients.ClientSet) {
				// Create checkpoint - CloneSandbox now looks for SandboxTemplate with same name as checkpoint
				cp := &v1alpha1.Checkpoint{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "checkpoint-no-sbt",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxTemplate: "checkpoint-no-sbt",
						},
					},
					Status: v1alpha1.CheckpointStatus{
						CheckpointId: "checkpoint-no-sbt",
					},
				}
				_, err := client.ApiV1alpha1().Checkpoints("default").Create(context.Background(), cp, metav1.CreateOptions{})
				require.NoError(t, err)
				// Wait for checkpoint to be cached
				require.Eventually(t, func() bool {
					_, err := cache.GetCheckpoint("checkpoint-no-sbt")
					return err == nil
				}, time.Second, 10*time.Millisecond)
			},
			expectError: "not found",
		},
		{
			name: "csi mount success",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 30 * time.Second,
				CSIMount: &config.CSIMountOptions{
					MountOptionList: []config.MountConfig{
						{
							Driver:     "test-driver",
							RequestRaw: "test-request",
						},
					},
				},
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:      1,
					ExitCode: 0,
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			sbxOverride: sbxOverride{Name: "test-sandbox-csi-mount-1", AccessToken: testutils.AccessToken},
			postCheck: func(t *testing.T, sbx infra.Sandbox, metrics infra.CloneMetrics) {
				assert.NotNil(t, sbx)
				assert.Greater(t, metrics.CSIMount, time.Duration(0), "CSIMount metric should be greater than 0")
				assert.GreaterOrEqual(t, metrics.Total, metrics.CSIMount, "Total should include CSIMount time")
			},
		},
		{
			name: "csi mount failure - non-zero exit code",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 30 * time.Second,
				CSIMount: &config.CSIMountOptions{
					MountOptionList: []config.MountConfig{
						{
							Driver:     "test-driver",
							RequestRaw: "test-request",
						},
					},
				},
			},
			serverOpts: testutils.TestRuntimeServerOptions{
				RunCommandResult: testutils.RunCommandResult{
					PID:      1,
					ExitCode: 1,
					Stderr:   []string{"mount error"},
					Exited:   true,
				},
				RunCommandImmediately: true,
			},
			sbxOverride: sbxOverride{Name: "test-sandbox-csi-mount-2", AccessToken: testutils.AccessToken},
			expectError: "failed to perform csi mount",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := testutils.NewTestRuntimeServer(tt.serverOpts)
			defer server.Close()

			cache, clientSet, err := NewTestCache(t)
			require.NoError(t, err)
			defer cache.Stop(t.Context())
			client := clientSet

			tt.opts.CloneTimeout = 500 * time.Millisecond

			// Create SandboxTemplate with same name as checkpoint
			// CloneSandbox now looks for template by checkpoint.Name, not by label
			sbt := &v1alpha1.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      checkpointID, // Same name as checkpoint
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxTemplateSpec{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "test-image",
								},
							},
						},
					},
				},
			}
			_, err = client.ApiV1alpha1().SandboxTemplates("default").Create(context.Background(), sbt, metav1.CreateOptions{})
			require.NoError(t, err)

			// Wait for SandboxTemplate to be cached
			require.Eventually(t, func() bool {
				_, err := cache.GetSandboxTemplate("default", checkpointID)
				return err == nil
			}, time.Minute, 10*time.Millisecond)

			// Create Checkpoint with same name as SandboxTemplate
			if tt.opts.CheckPointID != "non-existent-checkpoint" && tt.name != "checkpoint without template label" && tt.name != "template not found" {
				cp := &v1alpha1.Checkpoint{
					ObjectMeta: metav1.ObjectMeta{
						Name:      checkpointID,
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxTemplate: checkpointID,
						},
					},
					Status: v1alpha1.CheckpointStatus{
						CheckpointId: checkpointID,
					},
				}
				if tt.initRuntime != nil {
					initRuntimeAnnotation, err := json.Marshal(tt.initRuntime)
					require.NoError(t, err)
					cp.Annotations = map[string]string{
						v1alpha1.AnnotationInitRuntimeRequest: string(initRuntimeAnnotation),
					}
				}
				_, err = client.ApiV1alpha1().Checkpoints("default").Create(context.Background(), cp, metav1.CreateOptions{})
				require.NoError(t, err)
				// Wait for checkpoint to be cached
				require.Eventually(t, func() bool {
					_, err := cache.GetCheckpoint(checkpointID)
					return err == nil
				}, time.Second, 10*time.Millisecond)
			}

			// Run preProcess if defined
			if tt.preProcess != nil {
				tt.preProcess(t, cache, client)
				// Wait a bit for preProcess to create resources
				time.Sleep(50 * time.Millisecond)
			}

			// Build context with sbxOverride if needed
			ctx := t.Context()
			if tt.sbxOverride.Name != "" || tt.sbxOverride.RuntimeURL != "" {
				override := tt.sbxOverride
				if override.RuntimeURL == "" {
					override.RuntimeURL = server.URL
				}
				ctx = context.WithValue(ctx, sbxOverrideKey{}, override)
			}

			// Call CloneSandbox
			sbx, metrics, err := CloneSandbox(ctx, tt.opts, cache, client)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				require.NotNil(t, sbx)
				if tt.postCheck != nil {
					tt.postCheck(t, sbx, metrics)
				}
			}
		})
	}
}

func TestCloneSandbox_WithRateLimiter(t *testing.T) {
	utils.InitLogOutput()

	// Create a rate limiter with 0 burst to ensure it's always exhausted
	limiter := rate.NewLimiter(rate.Limit(1), 0)

	cache, clientSet, err := NewTestCache(t)
	require.NoError(t, err)
	defer cache.Stop(t.Context())
	client := clientSet

	template := "test-template"
	checkpointID := "test-checkpoint"
	user := "test-user"

	// Create SandboxSet
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      template,
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxSetSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "main",
								Image: "test-image",
							},
						},
					},
				},
			},
		},
	}
	_, err = client.ApiV1alpha1().SandboxSets("default").Create(context.Background(), sbs, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for SandboxSet to be cached
	require.Eventually(t, func() bool {
		_, err := cache.GetSandboxSet(template)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	// Create Checkpoint
	cp := &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointID,
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxTemplate: template,
			},
		},
		Status: v1alpha1.CheckpointStatus{
			CheckpointId: checkpointID,
		},
	}
	_, err = client.ApiV1alpha1().Checkpoints("default").Create(context.Background(), cp, metav1.CreateOptions{})
	require.NoError(t, err)
	// Wait for checkpoint to be cached
	require.Eventually(t, func() bool {
		_, err := cache.GetCheckpoint(checkpointID)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	opts := infra.CloneSandboxOptions{
		User:             user,
		CheckPointID:     checkpointID,
		WaitReadyTimeout: 30 * time.Second,
		CreateLimiter:    limiter,
	}

	// Call CloneSandbox - should fail due to rate limit
	sbx, metrics, err := CloneSandbox(t.Context(), opts, cache, client)

	assert.Nil(t, sbx, "sandbox should be nil when rate limited")
	assert.Error(t, err, "should return error when rate limited")
	// The error message from rate limiter is "rate: Wait(n=1) exceeds limiter's burst 0"
	assert.Contains(t, err.Error(), "rate:", "error should indicate rate limit")
	assert.Equal(t, time.Duration(0), metrics.Total, "metrics should be zero when rate limited")
}

func TestCloneSandbox_ContextCanceled(t *testing.T) {
	utils.InitLogOutput()

	cache, clientSet, err := NewTestCache(t)
	require.NoError(t, err)
	defer cache.Stop(t.Context())
	client := clientSet

	template := "test-template"
	checkpointID := "test-checkpoint"
	user := "test-user"

	// Create SandboxSet
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      template,
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxSetSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name:  "main",
								Image: "test-image",
							},
						},
					},
				},
			},
		},
	}
	_, err = client.ApiV1alpha1().SandboxSets("default").Create(context.Background(), sbs, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for SandboxSet to be cached
	require.Eventually(t, func() bool {
		_, err := cache.GetSandboxSet(template)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	// Create Checkpoint
	cp := &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointID,
			Namespace: "default",
			Labels: map[string]string{
				v1alpha1.LabelSandboxTemplate: template,
			},
		},
		Status: v1alpha1.CheckpointStatus{
			CheckpointId: checkpointID,
		},
	}
	_, err = client.ApiV1alpha1().Checkpoints("default").Create(context.Background(), cp, metav1.CreateOptions{})
	require.NoError(t, err)
	// Wait for checkpoint to be cached
	require.Eventually(t, func() bool {
		_, err := cache.GetCheckpoint(checkpointID)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	// Create canceled context
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	opts := infra.CloneSandboxOptions{
		User:             user,
		CheckPointID:     checkpointID,
		WaitReadyTimeout: 30 * time.Second,
	}

	// Call CloneSandbox with canceled context
	sbx, _, err := CloneSandbox(ctx, opts, cache, client)

	assert.Nil(t, sbx, "sandbox should be nil when context is canceled")
	assert.Error(t, err, "should return error when context is canceled")
	// When context is canceled during waitForSandboxReady, the error is "context canceled"
	// When context is canceled before, it could be different errors
	isContextError := err == context.Canceled || err == context.DeadlineExceeded ||
		(err != nil && assert.Contains(t, err.Error(), "context canceled"))
	assert.True(t, isContextError, "error should indicate context canceled, got: %v", err)
}

func newTestSandbox(name string) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   "default",
			Annotations: map[string]string{},
		},
		Spec: v1alpha1.SandboxSpec{
			EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{Name: "main", Image: "test-image"},
						},
					},
				},
			},
		},
	}
}

func TestCreateCheckPoint(t *testing.T) {
	utils.InitLogOutput()

	// Define context key types
	type cpStatusKey struct{}
	type tmplOverrideKey struct{}
	type tmplOverride struct {
		Name string
		UID  types.UID
	}
	// Error injection types
	type injectErrKey struct{}
	type injectErrTarget string
	const (
		injectErrTemplate   injectErrTarget = "template"
		injectErrCheckpoint injectErrTarget = "checkpoint"
	)

	// Decorator 1: DefaultCreateSandboxTemplate
	origCreateSandboxTemplate := DefaultCreateSandboxTemplate
	DefaultCreateSandboxTemplate = func(ctx context.Context, client clients.SandboxClient, tmpl *v1alpha1.SandboxTemplate) (*v1alpha1.SandboxTemplate, error) {
		// Check for error injection
		if target, ok := ctx.Value(injectErrKey{}).(injectErrTarget); ok && target == injectErrTemplate {
			return nil, fmt.Errorf("injected error: template creation failed")
		}
		if override, ok := ctx.Value(tmplOverrideKey{}).(tmplOverride); ok {
			if override.Name != "" {
				tmpl.Name = override.Name
			}
			if override.UID != "" {
				tmpl.UID = override.UID
			}
		}
		return origCreateSandboxTemplate(ctx, client, tmpl)
	}
	t.Cleanup(func() { DefaultCreateSandboxTemplate = origCreateSandboxTemplate })

	// Decorator 2: DefaultCreateCheckpoint
	origCreateCheckpoint := DefaultCreateCheckpoint
	DefaultCreateCheckpoint = func(ctx context.Context, client clients.SandboxClient, cp *v1alpha1.Checkpoint) (*v1alpha1.Checkpoint, error) {
		// Check for error injection
		if target, ok := ctx.Value(injectErrKey{}).(injectErrTarget); ok && target == injectErrCheckpoint {
			return nil, fmt.Errorf("injected error: checkpoint creation failed")
		}
		if status, ok := ctx.Value(cpStatusKey{}).(v1alpha1.CheckpointStatus); ok {
			cp.Status = status
		}
		created, err := origCreateCheckpoint(ctx, client, cp)
		if err != nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond) // Wait for informer sync
		return created, nil
	}
	t.Cleanup(func() { DefaultCreateCheckpoint = origCreateCheckpoint })

	// table-driven tests
	tests := []struct {
		name         string
		sandbox      *v1alpha1.Sandbox
		cpStatus     v1alpha1.CheckpointStatus
		tmplOverride tmplOverride
		opts         infra.CreateCheckpointOptions
		injectErr    injectErrTarget
		expectError  string
		postCheck    func(t *testing.T, id string, clientSet *clients.ClientSet)
	}{
		{
			name:    "successful checkpoint creation",
			sandbox: newTestSandbox("test-sandbox-1"),
			cpStatus: v1alpha1.CheckpointStatus{
				Phase:        v1alpha1.CheckpointSucceeded,
				CheckpointId: "cp-id-123",
			},
			tmplOverride: tmplOverride{Name: "tmpl-1", UID: "uid-1"},
			opts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 5 * time.Second,
			},
			postCheck: func(t *testing.T, id string, clientSet *clients.ClientSet) {
				assert.Equal(t, "cp-id-123", id)
				cp, err := clientSet.ApiV1alpha1().Checkpoints("default").Get(context.Background(), "tmpl-1", metav1.GetOptions{})
				require.NoError(t, err)
				assert.Equal(t, "tmpl-1", cp.Name)
				assert.Equal(t, "test-sandbox-1", *cp.Spec.PodName)
				require.Len(t, cp.OwnerReferences, 1)
				assert.Equal(t, "SandboxTemplate", cp.OwnerReferences[0].Kind)
				assert.Equal(t, "tmpl-1", cp.OwnerReferences[0].Name)
				assert.Equal(t, types.UID("uid-1"), cp.OwnerReferences[0].UID)
				// Verify PersistentContents: sandbox has no PersistentContents, so both template and checkpoint should be empty
				tmpl, err := clientSet.ApiV1alpha1().SandboxTemplates("default").Get(context.Background(), "tmpl-1", metav1.GetOptions{})
				require.NoError(t, err)
				assert.Empty(t, tmpl.Spec.PersistentContents, "template PersistentContents should be empty when sandbox has no PersistentContents")
				assert.Empty(t, cp.Spec.PersistentContents, "checkpoint PersistentContents should be empty when sandbox has no PersistentContents")
			},
		},
		{
			name:    "checkpoint with all options",
			sandbox: newTestSandbox("test-sandbox-2"),
			cpStatus: v1alpha1.CheckpointStatus{
				Phase:        v1alpha1.CheckpointSucceeded,
				CheckpointId: "cp-id-opts",
			},
			tmplOverride: tmplOverride{Name: "tmpl-2", UID: "uid-2"},
			opts: infra.CreateCheckpointOptions{
				KeepRunning:        ptr.To(true),
				TTL:                ptr.To("30m"),
				PersistentContents: []string{"memory", "filesystem"},
				WaitSuccessTimeout: 5 * time.Second,
			},
			postCheck: func(t *testing.T, id string, clientSet *clients.ClientSet) {
				assert.Equal(t, "cp-id-opts", id)
				cp, err := clientSet.ApiV1alpha1().Checkpoints("default").Get(context.Background(), "tmpl-2", metav1.GetOptions{})
				require.NoError(t, err)
				assert.Equal(t, "tmpl-2", cp.Name)
				assert.Equal(t, "test-sandbox-2", *cp.Spec.PodName)
				require.Len(t, cp.OwnerReferences, 1)
				assert.Equal(t, "SandboxTemplate", cp.OwnerReferences[0].Kind)
				assert.Equal(t, "tmpl-2", cp.OwnerReferences[0].Name)
				assert.Equal(t, types.UID("uid-2"), cp.OwnerReferences[0].UID)
				// Verify options
				require.NotNil(t, cp.Spec.KeepRunning)
				assert.True(t, *cp.Spec.KeepRunning)
				require.NotNil(t, cp.Spec.TtlAfterFinished)
				assert.Equal(t, "30m", *cp.Spec.TtlAfterFinished)
				// Verify PersistentContents: opts.PersistentContents should override template's PersistentContents
				tmpl, err := clientSet.ApiV1alpha1().SandboxTemplates("default").Get(context.Background(), "tmpl-2", metav1.GetOptions{})
				require.NoError(t, err)
				assert.Empty(t, tmpl.Spec.PersistentContents, "template PersistentContents should be empty when sandbox has no PersistentContents")
				assert.Equal(t, []string{"memory", "filesystem"}, cp.Spec.PersistentContents, "checkpoint PersistentContents should use opts.PersistentContents")
			},
		},
		{
			name: "checkpoint with init runtime annotation",
			sandbox: func() *v1alpha1.Sandbox {
				sbx := newTestSandbox("test-sandbox-3")
				sbx.Annotations[v1alpha1.AnnotationInitRuntimeRequest] = `{"accessToken":"test-token","envVars":{"VAR1":"value1"}}`
				return sbx
			}(),
			cpStatus: v1alpha1.CheckpointStatus{
				Phase:        v1alpha1.CheckpointSucceeded,
				CheckpointId: "cp-id-rt",
			},
			tmplOverride: tmplOverride{Name: "tmpl-3", UID: "uid-3"},
			opts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 5 * time.Second,
			},
			postCheck: func(t *testing.T, id string, clientSet *clients.ClientSet) {
				assert.Equal(t, "cp-id-rt", id)
				cp, err := clientSet.ApiV1alpha1().Checkpoints("default").Get(context.Background(), "tmpl-3", metav1.GetOptions{})
				require.NoError(t, err)
				assert.Equal(t, "tmpl-3", cp.Name)
				assert.Equal(t, "test-sandbox-3", *cp.Spec.PodName)
				require.Len(t, cp.OwnerReferences, 1)
				assert.Equal(t, "SandboxTemplate", cp.OwnerReferences[0].Kind)
				assert.Equal(t, "tmpl-3", cp.OwnerReferences[0].Name)
				assert.Equal(t, types.UID("uid-3"), cp.OwnerReferences[0].UID)
				// Verify init runtime annotation
				assert.Equal(t, `{"accessToken":"test-token","envVars":{"VAR1":"value1"}}`, cp.Annotations[v1alpha1.AnnotationInitRuntimeRequest])
			},
		},
		{
			name:    "checkpoint failed",
			sandbox: newTestSandbox("test-sandbox-4"),
			cpStatus: v1alpha1.CheckpointStatus{
				Phase:   v1alpha1.CheckpointFailed,
				Message: "disk full",
			},
			tmplOverride: tmplOverride{Name: "tmpl-4", UID: "uid-4"},
			opts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 5 * time.Second,
			},
			expectError: "failed",
		},
		{
			name:    "checkpoint succeeded with empty id",
			sandbox: newTestSandbox("test-sandbox-5"),
			cpStatus: v1alpha1.CheckpointStatus{
				Phase:        v1alpha1.CheckpointSucceeded,
				CheckpointId: "",
			},
			tmplOverride: tmplOverride{Name: "tmpl-5", UID: "uid-5"},
			opts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 5 * time.Second,
			},
			expectError: "has no checkpoint id",
		},
		{
			name:    "checkpoint terminating",
			sandbox: newTestSandbox("test-sandbox-6"),
			cpStatus: v1alpha1.CheckpointStatus{
				Phase:   v1alpha1.CheckpointTerminating,
				Message: "terminating",
			},
			tmplOverride: tmplOverride{Name: "tmpl-6", UID: "uid-6"},
			opts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 5 * time.Second,
			},
			expectError: "failed",
		},
		{
			name:    "sandbox template creation failed",
			sandbox: newTestSandbox("test-sbx-tmpl-fail"),
			opts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 5 * time.Second,
			},
			injectErr:   injectErrTemplate,
			expectError: "failed to create sandbox template",
		},
		{
			name:    "checkpoint creation failed",
			sandbox: newTestSandbox("test-sbx-cp-fail"),
			opts: infra.CreateCheckpointOptions{
				WaitSuccessTimeout: 5 * time.Second,
			},
			tmplOverride: tmplOverride{Name: "tmpl-fail", UID: "uid-fail"},
			injectErr:    injectErrCheckpoint,
			expectError:  "failed to create checkpoint",
		},
		{
			name: "checkpoint with sandbox PersistentContents - opts overrides",
			sandbox: func() *v1alpha1.Sandbox {
				sbx := newTestSandbox("test-sandbox-pc")
				sbx.Spec.PersistentContents = []string{"memory"}
				return sbx
			}(),
			cpStatus: v1alpha1.CheckpointStatus{
				Phase:        v1alpha1.CheckpointSucceeded,
				CheckpointId: "cp-id-pc",
			},
			tmplOverride: tmplOverride{Name: "tmpl-pc", UID: "uid-pc"},
			opts: infra.CreateCheckpointOptions{
				PersistentContents: []string{"memory", "filesystem"},
				WaitSuccessTimeout: 5 * time.Second,
			},
			postCheck: func(t *testing.T, id string, clientSet *clients.ClientSet) {
				assert.Equal(t, "cp-id-pc", id)
				cp, err := clientSet.ApiV1alpha1().Checkpoints("default").Get(context.Background(), "tmpl-pc", metav1.GetOptions{})
				require.NoError(t, err)
				tmpl, err := clientSet.ApiV1alpha1().SandboxTemplates("default").Get(context.Background(), "tmpl-pc", metav1.GetOptions{})
				require.NoError(t, err)
				// Verify PersistentContents logic:
				// 1. Template should inherit from sandbox.Spec.PersistentContents
				assert.Equal(t, []string{"memory"}, tmpl.Spec.PersistentContents, "template should inherit sandbox's PersistentContents")
				// 2. Checkpoint should use opts.PersistentContents (override template's)
				assert.Equal(t, []string{"memory", "filesystem"}, cp.Spec.PersistentContents, "checkpoint should use opts.PersistentContents override")
			},
		},
		{
			name: "checkpoint with sandbox PersistentContents - inherit from template",
			sandbox: func() *v1alpha1.Sandbox {
				sbx := newTestSandbox("test-sandbox-inherit")
				sbx.Spec.PersistentContents = []string{"filesystem"}
				return sbx
			}(),
			cpStatus: v1alpha1.CheckpointStatus{
				Phase:        v1alpha1.CheckpointSucceeded,
				CheckpointId: "cp-id-inherit",
			},
			tmplOverride: tmplOverride{Name: "tmpl-inherit", UID: "uid-inherit"},
			opts: infra.CreateCheckpointOptions{
				// No PersistentContents in opts, should inherit from template
				WaitSuccessTimeout: 5 * time.Second,
			},
			postCheck: func(t *testing.T, id string, clientSet *clients.ClientSet) {
				assert.Equal(t, "cp-id-inherit", id)
				cp, err := clientSet.ApiV1alpha1().Checkpoints("default").Get(context.Background(), "tmpl-inherit", metav1.GetOptions{})
				require.NoError(t, err)
				tmpl, err := clientSet.ApiV1alpha1().SandboxTemplates("default").Get(context.Background(), "tmpl-inherit", metav1.GetOptions{})
				require.NoError(t, err)
				// Verify PersistentContents logic:
				// 1. Template should inherit from sandbox.Spec.PersistentContents
				assert.Equal(t, []string{"filesystem"}, tmpl.Spec.PersistentContents, "template should inherit sandbox's PersistentContents")
				// 2. Checkpoint should inherit from template (opts.PersistentContents is empty)
				assert.Equal(t, []string{"filesystem"}, cp.Spec.PersistentContents, "checkpoint should inherit template's PersistentContents when opts is empty")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cache, clientSet, err := NewTestCache(t)
			require.NoError(t, err)
			defer cache.Stop(t.Context())

			ctx := t.Context()
			ctx = context.WithValue(ctx, cpStatusKey{}, tt.cpStatus)
			ctx = context.WithValue(ctx, tmplOverrideKey{}, tt.tmplOverride)
			if tt.injectErr != "" {
				ctx = context.WithValue(ctx, injectErrKey{}, tt.injectErr)
			}

			id, err := CreateCheckpoint(ctx, tt.sandbox, clientSet.SandboxClient, cache, tt.opts)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				if tt.postCheck != nil {
					tt.postCheck(t, id, clientSet)
				}
			}
		})
	}
}
