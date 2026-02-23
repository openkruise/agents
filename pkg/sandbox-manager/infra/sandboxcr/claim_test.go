package sandboxcr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

func GetSbsOwnerReference() []metav1.OwnerReference {
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sandboxset",
			UID:  "12345",
		},
	}
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, v1alpha1.SandboxSetControllerKind)}
}

func CreateSandboxWithStatus(t *testing.T, client versioned.Interface, sbx *v1alpha1.Sandbox) {
	_, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).Create(t.Context(), sbx, metav1.CreateOptions{})
	require.NoError(t, err)
	_, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(t.Context(), sbx, metav1.UpdateOptions{})
	require.NoError(t, err)
}

func EnsureSandboxInCache(t *testing.T, cache *Cache, sbx *v1alpha1.Sandbox) {
	require.Eventually(t, func() bool {
		_, err := cache.GetSandbox(sandboxutils.GetSandboxID(sbx))
		return err == nil
	}, time.Second, 10*time.Millisecond, "get sandbox from cache timeout")
}

var metricsAnnotationKey = v1alpha1.InternalPrefix + "metrics"

func GetMetricsFromSandbox(t *testing.T, sbx infra.Sandbox) infra.ClaimMetrics {
	ms := sbx.GetAnnotations()[metricsAnnotationKey]
	metrics := infra.ClaimMetrics{}
	require.NoError(t, json.Unmarshal([]byte(ms), &metrics))
	return metrics
}

//goland:noinspection GoDeprecation
func TestInfra_ClaimSandbox(t *testing.T) {
	SetClaimTimeout(50 * time.Millisecond)
	server := NewTestRuntimeServer(RunCommandResult{
		PID:    1,
		Exited: true,
	}, true, nil)
	defer server.Close()
	existTemplate := "test-template"
	user := "test-user"

	// Test cases
	tests := []struct {
		name        string
		available   int
		options     infra.ClaimSandboxOptions
		preModifier func(sbx *v1alpha1.Sandbox, infra *Infra)
		postCheck   func(t *testing.T, sbx infra.Sandbox)
		expectError string
	}{
		{
			name:      "claim with available pods",
			available: 2,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
			},
		},
		{
			name:      "claim with no template",
			available: 1,
			options: infra.ClaimSandboxOptions{
				User: user,
			},
			expectError: "template is required",
		},
		{
			name:      "claim with no user",
			available: 1,
			options: infra.ClaimSandboxOptions{
				Template: existTemplate,
			},
			expectError: "user is required",
		},
		{
			name:      "claim with no available pods",
			available: 0,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
			},
			expectError: "no stock",
		},
		{
			name:      "claim with modifier",
			available: 2,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
				Modifier: func(sbx infra.Sandbox) {
					sbx.SetAnnotations(map[string]string{
						"test-annotation": "test-value",
					})
				},
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, "test-value", sbx.GetAnnotations()["test-annotation"])
			},
		},
		{
			name:      "all locked",
			available: 10,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
			},
			preModifier: func(sbx *v1alpha1.Sandbox, infra *Infra) {
				sbx.Annotations[v1alpha1.AnnotationLock] = "XX"
			},
			expectError: "no candidate",
		},
		{
			name:      "claim with inplace update",
			available: 1,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
				InplaceUpdate: &infra.InplaceUpdateOptions{
					Image: "new-image",
				},
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, "new-image", sbx.(*Sandbox).Spec.Template.Spec.Containers[0].Image)
				metrics := GetMetricsFromSandbox(t, sbx)
				assert.Greater(t, metrics.InplaceUpdate, time.Duration(0))
			},
		},
		{
			name:      "claim with csi mount",
			available: 1,
			options: infra.ClaimSandboxOptions{
				User:        user,
				Template:    existTemplate,
				InitRuntime: &infra.InitRuntimeOptions{},
				CSIMount: &infra.CSIMountOptions{
					Driver: "",
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox, infra *Infra) {
				sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = AccessToken
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				metrics := GetMetricsFromSandbox(t, sbx)
				assert.Greater(t, metrics.InitRuntime, time.Duration(0))
				assert.Greater(t, metrics.CSIMount, time.Duration(0))
			},
		},
		{
			name:      "claim with out-dated cache",
			available: 1,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
			},
			preModifier: func(sbx *v1alpha1.Sandbox, infra *Infra) {
				sbx.UID = types.UID(uuid.NewString())
				sbx = sbx.DeepCopy()
				sbx.ResourceVersion = "100"
				utils.ResourceVersionExpectationExpect(sbx)
			},
			expectError: "no candidate",
		},
		{
			name:      "candidate picked by another request",
			available: 10,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
			},
			preModifier: func(sbx *v1alpha1.Sandbox, infra *Infra) {
				if sbx.Name == "sbx-3" {
					return
				}
				infra.pickCache.Store(getPickKey(sbx), struct{}{})
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, "sbx-3", sbx.GetName())
			},
		},
		{
			name:      "all candidate are picked",
			available: 2,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
			},
			preModifier: func(sbx *v1alpha1.Sandbox, infra *Infra) {
				infra.pickCache.Store(getPickKey(sbx), struct{}{})
			},
			expectError: "all candidates are picked",
		},
	}

	for _, tt := range tests {
		utils.InitLogOutput()
		t.Run(tt.name, func(t *testing.T) {
			testInfra, client := NewTestInfra(t)
			for i := 0; i < tt.available; i++ {
				sbx := &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("sbx-%d", i),
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxTemplate: existTemplate,
						},
						Annotations:     map[string]string{},
						OwnerReferences: GetSbsOwnerReference(),
					},
					Spec: v1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: corev1.PodSpec{
									Containers: []corev1.Container{
										{
											Name:  "main",
											Image: "old-image",
										},
									},
								},
							},
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{
							{
								Type:   string(v1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
						PodInfo: v1alpha1.PodInfo{
							PodIP: "1.2.3.4",
						},
					},
				}
				if tt.preModifier != nil {
					tt.preModifier(sbx, testInfra)
				}
				state, reason := sandboxutils.GetSandboxState(sbx)
				require.Equal(t, v1alpha1.SandboxStateAvailable, state, "reason", reason)
				CreateSandboxWithStatus(t, client, sbx)
				require.Eventually(t, func() bool {
					_, err := testInfra.GetSandbox(t.Context(), sandboxutils.GetSandboxID(sbx))
					return err == nil
				}, 100*time.Millisecond, 5*time.Millisecond)
			}
			sbx, metrics, err := testInfra.ClaimSandbox(t.Context(), tt.options)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.True(t, strings.Contains(err.Error(), tt.expectError))
			} else {
				require.NoError(t, err)
				require.NotNil(t, sbx)
				annotations := sbx.GetAnnotations()
				assert.NotEmpty(t, annotations[v1alpha1.AnnotationLock])
				assert.Equal(t, tt.options.User, annotations[v1alpha1.AnnotationOwner])
				metricsStr, err := json.Marshal(metrics)
				require.NoError(t, err)
				annotations[metricsAnnotationKey] = string(metricsStr)
				sbx.SetAnnotations(annotations)
				if tt.postCheck != nil {
					tt.postCheck(t, sbx)
				}
			}
		})
	}
}

//goland:noinspection GoDeprecation
func TestClaimSandboxFailed(t *testing.T) {
	SetClaimTimeout(100 * time.Millisecond)
	server := NewTestRuntimeServer(RunCommandResult{
		PID:      1,
		ExitCode: 1, // returns an error
		Exited:   true,
	}, true, nil)
	defer server.Close()
	existTemplate := "test-template"

	// Test cases
	tests := []struct {
		name        string
		options     infra.ClaimSandboxOptions
		preModifier func(sbx *v1alpha1.Sandbox)
		expectError string
		getContext  func() context.Context
	}{
		{
			name: "inplace update failed, reserved",
			options: infra.ClaimSandboxOptions{
				User:                 "test-user",
				Template:             existTemplate,
				ReserveFailedSandbox: true,
				InplaceUpdate: &infra.InplaceUpdateOptions{
					Image: "new-image",
				},
			},
			expectError: "sandbox inplace update failed",
		},
		{
			name: "inplace update failed, not reserved",
			options: infra.ClaimSandboxOptions{
				User:                 "test-user",
				Template:             existTemplate,
				ReserveFailedSandbox: false,
				InplaceUpdate: &infra.InplaceUpdateOptions{
					Image: "new-image",
				},
			},
			expectError: "sandbox inplace update failed",
		},
		{
			name: "csi mount failed, reserved",
			options: infra.ClaimSandboxOptions{
				User:                 "test-user",
				Template:             existTemplate,
				ReserveFailedSandbox: true,
				InitRuntime:          &infra.InitRuntimeOptions{},
				CSIMount: &infra.CSIMountOptions{
					Driver: "",
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox) {
				sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = AccessToken
			},
			expectError: "command failed",
		},
		{
			name: "csi mount failed, not reserved",
			options: infra.ClaimSandboxOptions{
				User:                 "test-user",
				Template:             existTemplate,
				ReserveFailedSandbox: false,
				InitRuntime:          &infra.InitRuntimeOptions{},
				CSIMount: &infra.CSIMountOptions{
					Driver: "",
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox) {
				sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = AccessToken
			},
			expectError: "command failed",
		},
		{
			name: "context canceled",
			options: infra.ClaimSandboxOptions{
				User:     "test-user",
				Template: existTemplate,
				// hack: the sandbox is not locked in this case, set true to pass the assertion
				ReserveFailedSandbox: true,
			},
			getContext: func() context.Context {
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				return ctx
			},
			expectError: "context canceled",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, client := NewTestInfra(t)
			name := "test-sbx"
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxTemplate: existTemplate,
					},
					Annotations:     map[string]string{},
					OwnerReferences: GetSbsOwnerReference(),
				},
				Spec: v1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{
										Name:  "main",
										Image: "old-image",
									},
								},
							},
						},
					},
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
							// make inplace update fail
							Reason: v1alpha1.SandboxReadyReasonStartContainerFailed,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				},
			}
			if tt.preModifier != nil {
				tt.preModifier(sbx)
			}
			state, reason := sandboxutils.GetSandboxState(sbx)
			require.Equal(t, v1alpha1.SandboxStateAvailable, state, "reason", reason)
			CreateSandboxWithStatus(t, client, sbx)
			require.Eventually(t, func() bool {
				_, err := testInfra.GetSandbox(t.Context(), sandboxutils.GetSandboxID(sbx))
				return err == nil
			}, 100*time.Millisecond, 5*time.Millisecond)
			var ctx context.Context
			if tt.getContext == nil {
				ctx = t.Context()
			} else {
				ctx = tt.getContext()
			}
			_, _, err := TryClaimSandbox(ctx, tt.options, &testInfra.pickCache, testInfra.Cache, client)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			_, err = client.ApiV1alpha1().Sandboxes(sbx.Namespace).Get(t.Context(), name, metav1.GetOptions{})
			if tt.options.ReserveFailedSandbox {
				assert.NoError(t, err)
			} else {
				assert.True(t, apierrors.IsNotFound(err))
			}
		})
	}
}

func TestCheckSandboxInplaceUpdate(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name               string
		generation         int64
		observedGeneration int64
		condStatus         metav1.ConditionStatus
		condReason         string
		condMessage        string
		expectResult       bool
		expectError        error
	}{
		{
			name:               "success",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionTrue,
			condReason:         v1alpha1.SandboxReadyReasonPodReady,
			expectResult:       true,
		},
		{
			name:               "not satisfied: out-dated cache",
			generation:         2,
			observedGeneration: 1,
			condStatus:         metav1.ConditionTrue,
			condReason:         v1alpha1.SandboxReadyReasonPodReady,
			expectResult:       false,
		},
		{
			name:               "not satisfied: inplace updating",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionFalse,
			condReason:         v1alpha1.SandboxReadyReasonInplaceUpdating,
			expectResult:       false,
		},
		{
			name:               "not satisfied: start container failed, deleted",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionFalse,
			condReason:         v1alpha1.SandboxReadyReasonStartContainerFailed,
			condMessage:        "by test",
			expectResult:       false,
			expectError:        retriableError{Message: "sandbox inplace update failed: by test"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, client := NewTestInfra(t)
			template := "test-template"
			sbs := &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      template,
					Namespace: "default",
				},
			}
			_, err := client.ApiV1alpha1().SandboxSets("default").Create(context.Background(), sbs, metav1.CreateOptions{})
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				return testInfra.HasTemplate(template)
			}, 100*time.Millisecond, 5*time.Millisecond)
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx-1",
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxTemplate: template,
					},
					Annotations: map[string]string{},
					Generation:  tt.generation,
				},
				Status: v1alpha1.SandboxStatus{
					Phase: v1alpha1.SandboxRunning,
					Conditions: []metav1.Condition{
						{
							Type:    string(v1alpha1.SandboxConditionReady),
							Status:  tt.condStatus,
							Reason:  tt.condReason,
							Message: tt.condMessage,
						},
					},
					PodInfo: v1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
					ObservedGeneration: tt.observedGeneration,
				},
			}
			CreateSandboxWithStatus(t, client, sbx)
			time.Sleep(10 * time.Millisecond)

			gotSbx, err := testInfra.Cache.GetSandbox(sandboxutils.GetSandboxID(sbx))
			assert.NoError(t, err)
			if err != nil {
				return
			}
			result, err := checkSandboxInplaceUpdate(t.Context(), gotSbx)
			assert.Equal(t, tt.expectResult, result)
			if tt.expectError != nil {
				assert.Error(t, err)
				assert.True(t, errors.Is(err, tt.expectError))
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
