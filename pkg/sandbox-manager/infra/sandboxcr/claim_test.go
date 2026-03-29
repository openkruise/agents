package sandboxcr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"

	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	testutils "github.com/openkruise/agents/test/utils"

	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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
		_, err := cache.GetClaimedSandbox(sandboxutils.GetSandboxID(sbx))
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
	utils.InitLogOutput()

	origCreateSandbox := DefaultCreateSandbox
	DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, client *clients.ClientSet, cache infra.CacheProvider) (*v1alpha1.Sandbox, error) {
		if sbx.Name == "" && sbx.GenerateName != "" {
			sbx.Name = sbx.GenerateName + rand.String(5)
		}
		created, err := origCreateSandbox(ctx, sbx, client, cache)
		if err != nil {
			return nil, err
		}
		created.Status = v1alpha1.SandboxStatus{
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
		}
		created, err = client.ApiV1alpha1().Sandboxes(created.Namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
		if err != nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
		return created, nil
	}
	t.Cleanup(func() { DefaultCreateSandbox = origCreateSandbox })

	opts := testutils.TestRuntimeServerOptions{
		RunCommandResult: testutils.RunCommandResult{
			PID:    1,
			Exited: true,
		},
		RunCommandImmediately: true,
	}
	server := testutils.NewTestRuntimeServer(opts)
	defer server.Close()
	existTemplate := "test-template"
	user := "test-user"

	tmpl := v1alpha1.EmbeddedSandboxTemplate{
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
	}

	// Test cases
	tests := []struct {
		name         string
		available    int
		infraOptions config.SandboxManagerOptions
		options      infra.ClaimSandboxOptions
		preProcess   func(t *testing.T, infra *Infra)
		claimCtx     func(parent context.Context) context.Context
		preModifier  func(sbx *v1alpha1.Sandbox, infra *Infra)
		postCheck    func(t *testing.T, sbx infra.Sandbox)
		expectError  string
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
				InplaceUpdate: &config.InplaceUpdateOptions{
					Image: "new-image",
				},
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, "new-image", sbx.(*Sandbox).Spec.Template.Spec.Containers[0].Image)
				metrics := GetMetricsFromSandbox(t, sbx)
				assert.Greater(t, metrics.WaitReady, time.Duration(0))
			},
		},
		{
			name:      "claim with cpu resize",
			available: 1,
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
				InplaceUpdate: &config.InplaceUpdateOptions{
					Resources: &config.InplaceUpdateResourcesOptions{
						ScaleFactor:      2,
						ReturnOnFeasible: true,
					},
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox, infra *Infra) {
				reqCPU := resource.MustParse("500m")
				reqMem := resource.MustParse("512Mi")
				sbx.Spec.Template.Spec.Containers[0].Resources = corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    reqCPU,
						corev1.ResourceMemory: reqMem,
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    reqCPU,
						corev1.ResourceMemory: reqMem,
					},
				}
				// Set InplaceUpdate condition so checkSandboxInplaceFeasible returns true
				sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
					Type:   string(v1alpha1.SandboxConditionInplaceUpdate),
					Status: metav1.ConditionFalse,
					Reason: v1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
				})
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      sbx.Name,
						Namespace: sbx.Namespace,
					},
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{
							{
								Name: "main",
								Resources: corev1.ResourceRequirements{
									Requests: corev1.ResourceList{
										corev1.ResourceCPU:    reqCPU,
										corev1.ResourceMemory: reqMem,
									},
									Limits: corev1.ResourceList{
										corev1.ResourceCPU:    reqCPU,
										corev1.ResourceMemory: reqMem,
									},
								},
							},
						},
					},
					Status: corev1.PodStatus{
						Phase:    corev1.PodRunning,
						QOSClass: corev1.PodQOSGuaranteed,
						Conditions: []corev1.PodCondition{
							{
								Type:   corev1.PodResizeInProgress,
								Status: corev1.ConditionTrue,
							},
						},
					},
				}
				createdPod, err := infra.Client.K8sClient.CoreV1().Pods(sbx.Namespace).Create(t.Context(), pod, metav1.CreateOptions{})
				require.NoError(t, err)
				createdPod.Status = pod.Status
				_, err = infra.Client.K8sClient.CoreV1().Pods(sbx.Namespace).UpdateStatus(t.Context(), createdPod, metav1.UpdateOptions{})
				require.NoError(t, err)
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
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
				InitRuntime: &config.InitRuntimeOptions{},
				CSIMount: &config.CSIMountOptions{
					MountOptionList: []config.MountConfig{
						{
							Driver: "",
						},
					},
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox, infra *Infra) {
				sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = testutils.AccessToken
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
		{
			name:      "create on no stock",
			available: 0,
			options: infra.ClaimSandboxOptions{
				User:            user,
				Template:        existTemplate,
				CreateOnNoStock: true,
			},
			preProcess: func(t *testing.T, infra *Infra) {
				sbs := v1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      existTemplate,
						Namespace: "default",
					},
					Spec: v1alpha1.SandboxSetSpec{
						EmbeddedSandboxTemplate: tmpl,
					},
				}
				_, err := infra.Client.ApiV1alpha1().SandboxSets("default").Create(t.Context(), &sbs, metav1.CreateOptions{})
				require.NoError(t, err)
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, tmpl.Template.Spec.Containers[0].Name, sbx.(*Sandbox).Spec.Template.Spec.Containers[0].Name)
			},
		},
		{
			name:      "create on no stock with no sandboxset",
			available: 0,
			options: infra.ClaimSandboxOptions{
				User:            user,
				Template:        existTemplate,
				CreateOnNoStock: true,
			},
			expectError: "cannot create new sandbox: sandboxset test-template not found in cache",
		},
		{
			name:      "create on no stock with inplace update",
			available: 0,
			options: infra.ClaimSandboxOptions{
				User:            user,
				Template:        existTemplate,
				CreateOnNoStock: true,
				InplaceUpdate: &config.InplaceUpdateOptions{
					Image: "new-image",
				},
			},
			preProcess: func(t *testing.T, infra *Infra) {
				sbs := v1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      existTemplate,
						Namespace: "default",
					},
					Spec: v1alpha1.SandboxSetSpec{
						EmbeddedSandboxTemplate: tmpl,
					},
				}
				_, err := infra.Client.ApiV1alpha1().SandboxSets("default").Create(t.Context(), &sbs, metav1.CreateOptions{})
				require.NoError(t, err)
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, "new-image", sbx.(*Sandbox).Spec.Template.Spec.Containers[0].Image)
			},
		},
		{
			name: "failed to get worker: timeout",
			infraOptions: config.SandboxManagerOptions{
				MaxClaimWorkers: 1,
			},
			preProcess: func(t *testing.T, infra *Infra) {
				infra.claimLockChannel <- struct{}{}
			},
			options: infra.ClaimSandboxOptions{
				User:     user,
				Template: existTemplate,
			},
			expectError: "context canceled before getting a free claim worker: context deadline exceeded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.options.ClaimTimeout = 50 * time.Millisecond
			testInfra, client := NewTestInfra(t, tt.infraOptions)
			now := metav1.Now()
			for i := 0; i < tt.available; i++ {
				sbx := &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      fmt.Sprintf("sbx-%d", i),
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxTemplate:        existTemplate,
							agentsv1alpha1.LabelSandboxIsClaimed: "false",
						},
						CreationTimestamp: now,
						Annotations:       map[string]string{},
						OwnerReferences:   GetSbsOwnerReference(),
					},
					Spec: v1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: tmpl,
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
				CreateSandboxWithStatus(t, client.SandboxClient, sbx)
				require.Eventually(t, func() bool {
					_, ok, err := testInfra.Cache.sandboxInformer.GetStore().GetByKey(fmt.Sprintf("%s/%s", sbx.Namespace, sbx.Name))
					return err == nil && ok
				}, 100*time.Millisecond, 5*time.Millisecond)
			}

			if tt.preProcess != nil {
				tt.preProcess(t, testInfra)
			}
			claimCtx := t.Context()
			if tt.claimCtx != nil {
				claimCtx = tt.claimCtx(t.Context())
			}
			sbx, metrics, err := testInfra.ClaimSandbox(claimCtx, tt.options)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
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
				_, ok := testInfra.pickCache.Load(getPickKey(sbx.(*Sandbox).Sandbox))
				assert.False(t, ok)
			}
		})
	}
}

//goland:noinspection GoDeprecation
func TestClaimSandboxFailed(t *testing.T) {
	opts := testutils.TestRuntimeServerOptions{
		RunCommandResult: testutils.RunCommandResult{
			PID:      1,
			ExitCode: 1, // returns an error
			Exited:   true,
		},
		RunCommandImmediately: true,
	}
	server := testutils.NewTestRuntimeServer(opts)
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
				InplaceUpdate: &config.InplaceUpdateOptions{
					Image: "new-image",
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type: string(v1alpha1.SandboxConditionReady),
						// hack: both make sandbox available and inplace update failed
						Status: metav1.ConditionTrue,
						Reason: v1alpha1.SandboxReadyReasonStartContainerFailed,
					},
				}
			},
			expectError: "sandbox inplace update failed",
		},
		{
			name: "inplace update failed, not reserved",
			options: infra.ClaimSandboxOptions{
				User:                 "test-user",
				Template:             existTemplate,
				ReserveFailedSandbox: false,
				InplaceUpdate: &config.InplaceUpdateOptions{
					Image: "new-image",
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type: string(v1alpha1.SandboxConditionReady),
						// hack: both make sandbox available and inplace update failed
						Status: metav1.ConditionTrue,
						Reason: v1alpha1.SandboxReadyReasonStartContainerFailed,
					},
				}
			},
			expectError: "sandbox inplace update failed",
		},
		{
			name: "csi mount failed, reserved",
			options: infra.ClaimSandboxOptions{
				User:                 "test-user",
				Template:             existTemplate,
				ReserveFailedSandbox: true,
				InitRuntime:          &config.InitRuntimeOptions{},
				CSIMount: &config.CSIMountOptions{
					MountOptionList: []config.MountConfig{
						{
							Driver: "",
						},
					},
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox) {
				sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = testutils.AccessToken
			},
			expectError: "command failed",
		},
		{
			name: "csi mount failed, not reserved",
			options: infra.ClaimSandboxOptions{
				User:                 "test-user",
				Template:             existTemplate,
				ReserveFailedSandbox: false,
				InitRuntime:          &config.InitRuntimeOptions{},
				CSIMount: &config.CSIMountOptions{
					MountOptionList: []config.MountConfig{
						{
							Driver: "",
						},
					},
				},
			},
			preModifier: func(sbx *v1alpha1.Sandbox) {
				sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = testutils.AccessToken
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
		{
			name: "no ip",
			options: infra.ClaimSandboxOptions{
				User:     "test-user",
				Template: existTemplate,
				// hack: the sandbox is not locked in this case, set true to pass the assertion
				ReserveFailedSandbox: true,
			},
			preModifier: func(sbx *v1alpha1.Sandbox) {
				sbx.Status.PodInfo.PodIP = ""
			},
			expectError: "no candidate",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.options.ClaimTimeout = 100 * time.Millisecond
			testInfra, client := NewTestInfra(t)
			name := "test-sbx"
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: "default",
					Labels: map[string]string{
						v1alpha1.LabelSandboxTemplate: existTemplate,
					},
					Annotations:       map[string]string{},
					OwnerReferences:   GetSbsOwnerReference(),
					CreationTimestamp: metav1.Now(),
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
				tt.preModifier(sbx)
			}
			state, reason := sandboxutils.GetSandboxState(sbx)
			require.Equal(t, v1alpha1.SandboxStateAvailable, state, reason)
			CreateSandboxWithStatus(t, client.SandboxClient, sbx)
			require.Eventually(t, func() bool {
				_, ok, err := testInfra.Cache.sandboxInformer.GetStore().GetByKey(fmt.Sprintf("%s/%s", sbx.Namespace, sbx.Name))
				return err == nil && ok
			}, 100*time.Millisecond, 5*time.Millisecond)
			var ctx context.Context
			if tt.getContext == nil {
				ctx = t.Context()
			} else {
				ctx = tt.getContext()
			}
			opts, err := ValidateAndInitClaimOptions(tt.options)
			require.NoError(t, err)
			_, _, err = TryClaimSandbox(ctx, opts, &testInfra.pickCache, testInfra.Cache, client, testInfra.claimLockChannel, testInfra.createLimiter)
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
						v1alpha1.LabelSandboxTemplate:  template,
						v1alpha1.LabelSandboxIsClaimed: "true",
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
			CreateSandboxWithStatus(t, client.SandboxClient, sbx)
			time.Sleep(10 * time.Millisecond)

			gotSbx, err := testInfra.Cache.GetClaimedSandbox(sandboxutils.GetSandboxID(sbx))
			assert.NoError(t, err)
			if err != nil {
				return
			}
			result, err := checkSandboxReady(t.Context(), gotSbx)
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

func TestModifyPickedSandboxCPUResize(t *testing.T) {
	base := &Sandbox{
		Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sbx-1",
				Namespace: "default",
				Labels: map[string]string{
					v1alpha1.LabelSandboxTemplate:  "test-template",
					v1alpha1.LabelSandboxIsClaimed: "false",
				},
				Annotations: map[string]string{},
			},
			Spec: v1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
					Template: &corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "img",
									Resources: corev1.ResourceRequirements{
										Requests: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("250m"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
										Limits: corev1.ResourceList{
											corev1.ResourceCPU:    resource.MustParse("250m"),
											corev1.ResourceMemory: resource.MustParse("256Mi"),
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}

	err := modifyPickedSandbox(base, infra.LockTypeUpdate, infra.ClaimSandboxOptions{
		User:     "u1",
		Template: "test-template",
		InplaceUpdate: &config.InplaceUpdateOptions{
			Resources: &config.InplaceUpdateResourcesOptions{
				ScaleFactor: 2,
			},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(500), base.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.Equal(t, int64(500), base.Spec.Template.Spec.Containers[0].Resources.Limits.Cpu().MilliValue())
}

func TestModifyPickedSandboxCPUResizeCases(t *testing.T) {
	tests := []struct {
		name string

		templateSpec corev1.PodSpec
		factor       float64

		wantReqCPU     int64
		wantLimCPU     int64
		wantSidecarCPU int64
	}{
		{
			name: "requests only - scale up",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					},
				},
			},
			factor:     2,
			wantReqCPU: 200,
			wantLimCPU: 0,
		},
		{
			name: "limits only - scale up",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
					},
				},
			},
			factor:     2,
			wantReqCPU: 0,
			wantLimCPU: 1000,
		},
		{
			name: "no cpu resources - no change",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
					},
				},
			},
			factor:     2,
			wantReqCPU: 0,
			wantLimCPU: 0,
		},
		{
			name: "empty resources - no change",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:      "main",
						Image:     "img",
						Resources: corev1.ResourceRequirements{},
					},
				},
			},
			factor:     2,
			wantReqCPU: 0,
			wantLimCPU: 0,
		},
		{
			name: "scale down",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("500m"),
							},
						},
					},
				},
			},
			factor:     0.5,
			wantReqCPU: 250,
			wantLimCPU: 250,
		},
		{
			name: "multiple containers mixed",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					},
					{
						Name:  "sidecar",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					},
				},
			},
			factor:     2,
			wantReqCPU: 200, // container[0] requests: 100m * 2 = 200m
			wantLimCPU: 0,   // container[0] limits: not set
		},
		{
			name: "sidecar container limits scaled",
			templateSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("100m"),
							},
						},
					},
					{
						Name:  "sidecar",
						Image: "img",
						Resources: corev1.ResourceRequirements{
							Limits: corev1.ResourceList{
								corev1.ResourceCPU: resource.MustParse("200m"),
							},
						},
					},
				},
			},
			factor:         2,
			wantReqCPU:     200, // container[0] requests: 100m * 2 = 200m
			wantLimCPU:     0,   // container[0] limits: not set
			wantSidecarCPU: 400, // container[1] limits: 200m * 2 = 400m
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &Sandbox{
				Sandbox: &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx-1",
						Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxTemplate:  "test-template",
							v1alpha1.LabelSandboxIsClaimed: "false",
						},
						Annotations: map[string]string{},
					},
					Spec: v1alpha1.SandboxSpec{
						EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
							Template: &corev1.PodTemplateSpec{
								Spec: tt.templateSpec,
							},
						},
					},
				},
			}

			err := modifyPickedSandbox(sbx, infra.LockTypeUpdate, infra.ClaimSandboxOptions{
				User:     "u1",
				Template: "test-template",
				InplaceUpdate: &config.InplaceUpdateOptions{
					Resources: &config.InplaceUpdateResourcesOptions{
						ScaleFactor: tt.factor,
					},
				},
			})
			require.NoError(t, err)

			if tt.wantReqCPU > 0 {
				assert.Equal(t, tt.wantReqCPU, sbx.Spec.Template.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
			}
			if tt.wantLimCPU > 0 {
				assert.Equal(t, tt.wantLimCPU, sbx.Spec.Template.Spec.Containers[0].Resources.Limits.Cpu().MilliValue())
			}
			if tt.wantSidecarCPU > 0 {
				assert.Equal(t, tt.wantSidecarCPU, sbx.Spec.Template.Spec.Containers[1].Resources.Limits.Cpu().MilliValue())
			}
		})
	}
}

func TestModifyPickedSandboxCPUNilTemplate(t *testing.T) {
	sbx := &Sandbox{
		Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sbx-1",
				Namespace:   "default",
				Labels:      map[string]string{},
				Annotations: map[string]string{},
			},
			Spec: v1alpha1.SandboxSpec{
				EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
					Template: nil,
				},
			},
		},
	}

	err := modifyPickedSandbox(sbx, infra.LockTypeUpdate, infra.ClaimSandboxOptions{
		User:     "u1",
		Template: "test-template",
		InplaceUpdate: &config.InplaceUpdateOptions{
			Resources: &config.InplaceUpdateResourcesOptions{
				ScaleFactor: 2,
			},
		},
	})
	require.NoError(t, err)
	assert.Nil(t, sbx.Spec.Template)
}

func TestBuildContainerCPUTargets(t *testing.T) {
	tests := []struct {
		name       string
		podSpec    corev1.PodSpec
		wantNames  []string
		wantReqCPU map[string]int64
		wantLimCPU map[string]int64
	}{
		{
			name: "only requests",
			podSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c1", Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					}},
				},
			},
			wantNames:  []string{"c1"},
			wantReqCPU: map[string]int64{"c1": 100},
			wantLimCPU: map[string]int64{},
		},
		{
			name: "only limits",
			podSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c1", Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
					}},
				},
			},
			wantNames:  []string{"c1"},
			wantReqCPU: map[string]int64{},
			wantLimCPU: map[string]int64{"c1": 200},
		},
		{
			name: "no cpu resources",
			podSpec: corev1.PodSpec{
				Containers: []corev1.Container{
					{Name: "c1", Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("128Mi")},
					}},
				},
			},
			wantNames:  []string{},
			wantReqCPU: map[string]int64{},
			wantLimCPU: map[string]int64{},
		},
		{
			name: "init container with cpu",
			podSpec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Name: "init", Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
						Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
					}},
				},
				Containers: []corev1.Container{
					{Name: "main", Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
					}},
				},
			},
			wantNames:  []string{"init", "main"},
			wantReqCPU: map[string]int64{"init": 50, "main": 200},
			wantLimCPU: map[string]int64{"init": 100},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{Spec: tt.podSpec}
			targets := buildContainerCPUTargets(pod)
			if len(tt.wantNames) == 0 {
				assert.Empty(t, targets)
				return
			}
			for _, name := range tt.wantNames {
				target, ok := targets[name]
				require.True(t, ok, "expected container %s in targets", name)
				if req, ok := tt.wantReqCPU[name]; ok {
					assert.Equal(t, req, target.request.MilliValue())
				}
				if lim, ok := tt.wantLimCPU[name]; ok {
					assert.Equal(t, lim, target.limit.MilliValue())
				}
			}
		})
	}
}

func TestIsPodCPUResizeApplied(t *testing.T) {
	tests := []struct {
		name                  string
		targets               map[string]containerCPUTarget
		containerStatuses     []corev1.ContainerStatus
		initContainerStatuses []corev1.ContainerStatus
		want                  bool
	}{
		{
			name:    "empty targets",
			targets: map[string]containerCPUTarget{},
			want:    false,
		},
		{
			name:    "container status missing",
			targets: map[string]containerCPUTarget{"main": {request: resource.MustParse("100m"), hasRequest: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "sidecar", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}},
			},
			want: false,
		},
		{
			name:    "nil resources in status",
			targets: map[string]containerCPUTarget{"main": {request: resource.MustParse("100m"), hasRequest: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: nil},
			},
			want: false,
		},
		{
			name:    "request not matching",
			targets: map[string]containerCPUTarget{"main": {request: resource.MustParse("200m"), hasRequest: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}},
			},
			want: false,
		},
		{
			name:    "limit not matching",
			targets: map[string]containerCPUTarget{"main": {limit: resource.MustParse("500m"), hasLimit: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: &corev1.ResourceRequirements{
					Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")},
				}},
			},
			want: false,
		},
		{
			name:    "applied",
			targets: map[string]containerCPUTarget{"main": {request: resource.MustParse("200m"), limit: resource.MustParse("400m"), hasRequest: true, hasLimit: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("400m")},
				}},
			},
			want: true,
		},
		{
			name:    "init container applied",
			targets: map[string]containerCPUTarget{"init": {request: resource.MustParse("50m"), hasRequest: true}},
			containerStatuses: []corev1.ContainerStatus{
				{Name: "main", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
				}},
			},
			initContainerStatuses: []corev1.ContainerStatus{
				{Name: "init", Resources: &corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
				}},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses:     tt.containerStatuses,
					InitContainerStatuses: tt.initContainerStatuses,
				},
			}
			got := isPodCPUResizeApplied(pod, tt.targets)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestScaleCPUQuantity(t *testing.T) {
	tests := []struct {
		name   string
		q      resource.Quantity
		factor float64
		want   int64
	}{
		{
			name:   "scale up 250m * 2 = 500m",
			q:      resource.MustParse("250m"),
			factor: 2,
			want:   500,
		},
		{
			name:   "scale down 500m * 0.5 = 250m",
			q:      resource.MustParse("500m"),
			factor: 0.5,
			want:   250,
		},
		{
			name:   "fractional result rounds up",
			q:      resource.MustParse("100m"),
			factor: 1.5,
			want:   150,
		},
		{
			name:   "scale to 1m when result would be 0",
			q:      resource.MustParse("1m"),
			factor: 0.1,
			want:   1, // clamped to minimum 1m
		},
		{
			name:   "integer cpu 1 * 2 = 2000m",
			q:      resource.MustParse("1"),
			factor: 2,
			want:   2000,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := scaleCPUQuantity(tt.q, tt.factor)
			assert.Equal(t, tt.want, got.MilliValue())
		})
	}
}

func TestCheckSandboxInplaceFeasible(t *testing.T) {
	tests := []struct {
		name         string
		sbx          *v1alpha1.Sandbox
		expectReady  bool
		expectErr    bool
		errorMessage string
	}{
		{
			name: "observed generation not synced",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-1",
					Namespace:  "default",
					Generation: 2,
				},
				Status: v1alpha1.SandboxStatus{
					ObservedGeneration: 1,
				},
			},
			expectReady: false,
		},
		{
			name: "inplace update in progress is feasible",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-2",
					Namespace:  "default",
					Generation: 2,
				},
				Status: v1alpha1.SandboxStatus{
					ObservedGeneration: 2,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionInplaceUpdate),
							Status: metav1.ConditionFalse,
							Reason: v1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
						},
					},
				},
			},
			expectReady: true,
		},
		{
			name: "ready start container failed should return error",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-3",
					Namespace:  "default",
					Generation: 2,
				},
				Status: v1alpha1.SandboxStatus{
					ObservedGeneration: 2,
					Conditions: []metav1.Condition{
						{
							Type:    string(v1alpha1.SandboxConditionReady),
							Status:  metav1.ConditionFalse,
							Reason:  v1alpha1.SandboxReadyReasonStartContainerFailed,
							Message: "image pull failed",
						},
					},
				},
			},
			expectErr:    true,
			errorMessage: "image pull failed",
		},
		{
			name: "inplace update succeeded is feasible",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-4",
					Namespace:  "default",
					Generation: 2,
				},
				Status: v1alpha1.SandboxStatus{
					ObservedGeneration: 2,
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionInplaceUpdate),
							Status: metav1.ConditionTrue,
							Reason: v1alpha1.SandboxInplaceUpdateReasonSucceeded,
						},
					},
				},
			},
			expectReady: true,
		},
		{
			name: "ready-only should not be treated as inplace feasible",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "sbx-5",
					Namespace:  "default",
					Generation: 2,
				},
				Status: v1alpha1.SandboxStatus{
					ObservedGeneration: 2,
					Phase:              v1alpha1.SandboxRunning,
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
				},
			},
			expectReady: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready, err := checkSandboxInplaceFeasible(t.Context(), tt.sbx)
			assert.Equal(t, tt.expectReady, ready)
			if tt.expectErr {
				require.Error(t, err)
				if tt.errorMessage != "" {
					assert.Contains(t, err.Error(), tt.errorMessage)
				}
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestBuildCPUResizedPod(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("250m"),
							corev1.ResourceMemory: resource.MustParse("256Mi"),
						},
					},
				},
			},
		},
		Status: corev1.PodStatus{
			QOSClass: corev1.PodQOSGuaranteed,
		},
	}

	got, changed, err := buildCPUResizedPod(pod, 2)
	require.NoError(t, err)
	require.True(t, changed)
	assert.Equal(t, int64(500), got.Spec.Containers[0].Resources.Requests.Cpu().MilliValue())
	assert.Equal(t, int64(500), got.Spec.Containers[0].Resources.Limits.Cpu().MilliValue())
}

func TestValidateAndInitClaimOptions_CPUResize(t *testing.T) {
	_, err := ValidateAndInitClaimOptions(infra.ClaimSandboxOptions{
		User:     "u",
		Template: "t",
		InplaceUpdate: &config.InplaceUpdateOptions{
			Resources: &config.InplaceUpdateResourcesOptions{
				ScaleFactor: 1,
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cpu scale factor should be greater than 1")
}

func TestWaitForPodResizeState(t *testing.T) {
	targetPod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name: "main",
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("500m"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1000m"),
						},
					},
				},
			},
		},
	}
	tests := []struct {
		name             string
		returnOnFeasible bool
		conditions       []corev1.PodCondition
		containerStatus  []corev1.ContainerStatus
		resizeStatus     corev1.PodResizeStatus
		expectErr        string
	}{
		{
			name:             "return when feasible",
			returnOnFeasible: true,
			conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodResizeInProgress,
					Status: corev1.ConditionTrue,
				},
			},
		},
		{
			name:             "infeasible should fail",
			returnOnFeasible: true,
			conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodResizePending,
					Status:  corev1.ConditionTrue,
					Reason:  corev1.PodReasonInfeasible,
					Message: "insufficient cpu",
				},
			},
			expectErr: "infeasible",
		},
		{
			name:             "completed without feasible return",
			returnOnFeasible: false,
			conditions:       nil,
			containerStatus: []corev1.ContainerStatus{
				{
					Name: "main",
					Resources: &corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("500m"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1000m"),
						},
					},
				},
			},
			resizeStatus: "",
		},
		{
			name:             "non-feasible path should not succeed without resize signal or applied status",
			returnOnFeasible: false,
			conditions:       nil,
			resizeStatus:     "",
			expectErr:        "wait for pod resize state",
		},
		{
			name:             "deferred should timeout with pending reason",
			returnOnFeasible: false,
			conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodResizePending,
					Status:  corev1.ConditionTrue,
					Reason:  corev1.PodReasonDeferred,
					Message: "Node didn't have enough resource: cpu",
				},
			},
			expectErr: "last pending reason=Deferred",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := clients.NewFakeClientSet(t)
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Conditions:        tt.conditions,
					Resize:            tt.resizeStatus,
					ContainerStatuses: tt.containerStatus,
				},
			}
			createdPod, err := client.K8sClient.CoreV1().Pods("default").Create(t.Context(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			createdPod.Status = pod.Status
			_, err = client.K8sClient.CoreV1().Pods("default").UpdateStatus(t.Context(), createdPod, metav1.UpdateOptions{})
			require.NoError(t, err)

			err = waitForPodResizeState(t.Context(), client, "default", "test-pod", targetPod, 500*time.Millisecond, tt.returnOnFeasible)
			if tt.expectErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func KeepMakingAllSandboxesReady(ctx context.Context, client clients.SandboxClient) {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sandboxList, err := client.ApiV1alpha1().Sandboxes("default").List(context.Background(), metav1.ListOptions{})
			if err != nil {
				// Don't use require.NoError in goroutine - causes data race
				continue
			}
			for _, sbx := range sandboxList.Items {
				// Skip already ready sandboxes to reduce unnecessary updates
				currentState, _ := sandboxutils.GetSandboxState(&sbx)
				if currentState == v1alpha1.SandboxStateRunning {
					continue
				}

				sbx.Status = v1alpha1.SandboxStatus{
					Phase:              v1alpha1.SandboxRunning,
					ObservedGeneration: sbx.Generation, // Important: sync generation
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
				updated, err := client.ApiV1alpha1().Sandboxes(sbx.Namespace).UpdateStatus(context.Background(), &sbx, metav1.UpdateOptions{})
				if err != nil {
					fmt.Printf("failed to update sandbox status: %v\n", err)
					continue
				}
				// Record the expected version to help InplaceRefresh
				if updated != nil {
					utils.ResourceVersionExpectationExpect(updated)
				}
			}
		}
	}
}

func TestNewSandboxFromTemplate_RateLimitExceeded(t *testing.T) {
	utils.InitLogOutput()

	// Create a rate limiter with 0 burst to ensure it's always exhausted
	limiter := rate.NewLimiter(rate.Limit(1), 0)

	// Create test infrastructure
	infraInstance, client := NewTestInfra(t)
	defer infraInstance.Stop()

	template := "test-template"

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
	_, err := client.ApiV1alpha1().SandboxSets("default").Create(context.Background(), sbs, metav1.CreateOptions{})
	require.NoError(t, err)

	// Wait for cache to sync
	require.Eventually(t, func() bool {
		_, err := infraInstance.Cache.GetSandboxSet(template)
		return err == nil
	}, time.Second, 10*time.Millisecond)

	// Test: Call newSandboxFromSandboxSet when rate limiter is exhausted
	opts := infra.ClaimSandboxOptions{
		Template: template,
		User:     "test-user",
	}

	// Call the function
	sbx, _, err := newSandboxFromSandboxSet(opts, infraInstance.Cache, infraInstance.Client.SandboxClient, limiter)

	// Assertions
	assert.Nil(t, sbx, "sandbox should be nil when rate limited")
	assert.Error(t, err, "should return error when rate limited")

	// Check error message
	assert.Contains(t, err.Error(), "sandbox creation is not allowed by rate limiter", "error should indicate rate limit")
	assert.Contains(t, err.Error(), template, "error should contain template name")
}
