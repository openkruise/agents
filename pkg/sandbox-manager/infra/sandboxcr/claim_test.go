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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/utils/runtime"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	testutils "github.com/openkruise/agents/test/utils"
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
		RunCommandResult: runtime.RunCommandResult{
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
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1"),
						},
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
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				metrics := GetMetricsFromSandbox(t, sbx)
				assert.Greater(t, metrics.WaitReady, time.Duration(0))
			},
		},
		{
			name:      "claim with csi mount",
			available: 1,
			options: infra.ClaimSandboxOptions{
				User:         user,
				Template:     existTemplate,
				ClaimTimeout: 500 * time.Millisecond,
				InitRuntime:  &config.InitRuntimeOptions{},
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
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = runtime.AccessToken
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
			if tt.options.ClaimTimeout <= 0 {
				tt.options.ClaimTimeout = 50 * time.Millisecond
			}
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
		RunCommandResult: runtime.RunCommandResult{
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
			name: "start container failed, reserved",
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
						Type:   string(v1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
						Reason: v1alpha1.SandboxReadyReasonStartContainerFailed,
					},
				}
			},
			expectError: "sandbox start container failed",
		},
		{
			name: "start container failed, not reserved",
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
						Type:   string(v1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
						Reason: v1alpha1.SandboxReadyReasonStartContainerFailed,
					},
				}
			},
			expectError: "sandbox start container failed",
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
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = runtime.AccessToken
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
				sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = runtime.AccessToken
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
		extraConditions    []metav1.Condition
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
			condReason:         v1alpha1.SandboxReadyReasonUpgrading,
			expectResult:       false,
		},
		{
			name:               "not satisfied: inplace update condition in progress",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionTrue,
			condReason:         v1alpha1.SandboxReadyReasonPodReady,
			extraConditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionInplaceUpdate),
					Status: metav1.ConditionFalse,
					Reason: v1alpha1.SandboxInplaceUpdateReasonInplaceUpdating,
				},
			},
			expectResult: false,
		},
		{
			name:               "ready after inplace update failed",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionTrue,
			condReason:         v1alpha1.SandboxReadyReasonPodReady,
			extraConditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionInplaceUpdate),
					Status: metav1.ConditionTrue,
					Reason: v1alpha1.SandboxInplaceUpdateReasonFailed,
				},
			},
			expectResult: true,
		},
		{
			name:               "ready after inplace update succeeded",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionTrue,
			condReason:         v1alpha1.SandboxReadyReasonPodReady,
			extraConditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionInplaceUpdate),
					Status: metav1.ConditionTrue,
					Reason: v1alpha1.SandboxInplaceUpdateReasonSucceeded,
				},
			},
			expectResult: true,
		},
		{
			name:               "not satisfied: start container failed, deleted",
			generation:         1,
			observedGeneration: 1,
			condStatus:         metav1.ConditionFalse,
			condReason:         v1alpha1.SandboxReadyReasonStartContainerFailed,
			condMessage:        "by test",
			expectResult:       false,
			expectError:        retriableError{Message: "sandbox start container failed: by test"},
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
			conditions := []metav1.Condition{
				{
					Type:    string(v1alpha1.SandboxConditionReady),
					Status:  tt.condStatus,
					Reason:  tt.condReason,
					Message: tt.condMessage,
				},
			}
			conditions = append(conditions, tt.extraConditions...)
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
					Phase:      v1alpha1.SandboxRunning,
					Conditions: conditions,
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
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
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

		templateSpec   corev1.PodSpec
		inplaceReq     corev1.ResourceList
		inplaceLim     corev1.ResourceList
		wantReqCPU     int64
		wantLimCPU     int64
		wantSidecarCPU int64
	}{
		{
			name: "requests only - set target",
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
			inplaceReq: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
			wantReqCPU: 200,
			wantLimCPU: 0,
		},
		{
			name: "limits only - set target",
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
			inplaceLim: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
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
			inplaceReq: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
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
			inplaceReq: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
			wantReqCPU: 0,
			wantLimCPU: 0,
		},
		{
			name: "set lower target",
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
			inplaceReq: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			inplaceLim: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
			wantReqCPU: 250,
			wantLimCPU: 250,
		},
		{
			name: "only first container gets target - sidecar unchanged",
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
			inplaceReq:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
			inplaceLim:     corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m")},
			wantReqCPU:     300,
			wantLimCPU:     0,
			wantSidecarCPU: 200,
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
						Requests: tt.inplaceReq,
						Limits:   tt.inplaceLim,
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
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
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

func TestBuildResourceResizedPod(t *testing.T) {
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

	targetCPU := resource.MustParse("500m")
	requests := corev1.ResourceList{corev1.ResourceCPU: targetCPU}
	limits := corev1.ResourceList{corev1.ResourceCPU: targetCPU}
	got, changed := buildResourceResizedPod(pod, requests, limits)
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
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")},
			},
		},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "target cpu must be a positive value")
}

func TestValidateAndInitClaimOptions_InplaceUpdateValidation(t *testing.T) {
	tests := []struct {
		name        string
		opts        infra.ClaimSandboxOptions
		expectErr   bool
		errContains string
	}{
		{
			name: "inplace update requires image or resources",
			opts: infra.ClaimSandboxOptions{
				User:          "u",
				Template:      "t",
				InplaceUpdate: &config.InplaceUpdateOptions{},
			},
			expectErr:   true,
			errContains: "requires either image or resources",
		},
		{
			name: "resources require requests or limits",
			opts: infra.ClaimSandboxOptions{
				User:     "u",
				Template: "t",
				InplaceUpdate: &config.InplaceUpdateOptions{
					Resources: &config.InplaceUpdateResourcesOptions{},
				},
			},
			expectErr:   true,
			errContains: "resources must specify at least one of requests or limits",
		},
		{
			name: "negative cpu request rejected",
			opts: infra.ClaimSandboxOptions{
				User:     "u",
				Template: "t",
				InplaceUpdate: &config.InplaceUpdateOptions{
					Resources: &config.InplaceUpdateResourcesOptions{
						Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("-1")},
					},
				},
			},
			expectErr:   true,
			errContains: "target cpu must be a positive value",
		},
		{
			name: "cpu limit only is allowed",
			opts: infra.ClaimSandboxOptions{
				User:     "u",
				Template: "t",
				InplaceUpdate: &config.InplaceUpdateOptions{
					Resources: &config.InplaceUpdateResourcesOptions{
						Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")},
					},
				},
			},
			expectErr: false,
		},
		{
			name: "image only is allowed",
			opts: infra.ClaimSandboxOptions{
				User:     "u",
				Template: "t",
				InplaceUpdate: &config.InplaceUpdateOptions{
					Image: "nginx:stable",
				},
			},
			expectErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ValidateAndInitClaimOptions(tt.opts)
			if tt.expectErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			require.NoError(t, err)
		})
	}
}

// --- Pod resize state helpers (test-only, used by TestWaitForPodResizeState and related tests) ---

type containerCPUTarget struct {
	request    resource.Quantity
	hasRequest bool
	limit      resource.Quantity
	hasLimit   bool
}

func buildContainerCPUTargets(pod *corev1.Pod) map[string]containerCPUTarget {
	targets := make(map[string]containerCPUTarget, len(pod.Spec.Containers)+len(pod.Spec.InitContainers))
	for _, c := range pod.Spec.Containers {
		target := containerCPUTarget{}
		if c.Resources.Requests != nil {
			cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]
			if ok {
				target.request = cpuReq
				target.hasRequest = true
			}
		}
		if c.Resources.Limits != nil {
			cpuLim, ok := c.Resources.Limits[corev1.ResourceCPU]
			if ok {
				target.limit = cpuLim
				target.hasLimit = true
			}
		}
		if target.hasRequest || target.hasLimit {
			targets[c.Name] = target
		}
	}
	for _, c := range pod.Spec.InitContainers {
		target := containerCPUTarget{}
		if c.Resources.Requests != nil {
			cpuReq, ok := c.Resources.Requests[corev1.ResourceCPU]
			if ok {
				target.request = cpuReq
				target.hasRequest = true
			}
		}
		if c.Resources.Limits != nil {
			cpuLim, ok := c.Resources.Limits[corev1.ResourceCPU]
			if ok {
				target.limit = cpuLim
				target.hasLimit = true
			}
		}
		if target.hasRequest || target.hasLimit {
			targets[c.Name] = target
		}
	}
	return targets
}

func isPodCPUResizeApplied(pod *corev1.Pod, targets map[string]containerCPUTarget) bool {
	if len(targets) == 0 {
		return false
	}
	statuses := make(map[string]*corev1.ContainerStatus, len(pod.Status.ContainerStatuses)+len(pod.Status.InitContainerStatuses))
	for i := range pod.Status.ContainerStatuses {
		statuses[pod.Status.ContainerStatuses[i].Name] = &pod.Status.ContainerStatuses[i]
	}
	for i := range pod.Status.InitContainerStatuses {
		statuses[pod.Status.InitContainerStatuses[i].Name] = &pod.Status.InitContainerStatuses[i]
	}
	for name, target := range targets {
		status, ok := statuses[name]
		if !ok || status.Resources == nil {
			return false
		}
		if target.hasRequest {
			actualReq, ok := status.Resources.Requests[corev1.ResourceCPU]
			if !ok || actualReq.Cmp(target.request) != 0 {
				return false
			}
		}
		if target.hasLimit {
			actualLim, ok := status.Resources.Limits[corev1.ResourceCPU]
			if !ok || actualLim.Cmp(target.limit) != 0 {
				return false
			}
		}
	}
	return true
}

type podResizeStateSnapshot struct {
	pendingTrue       bool
	pendingReason     string
	pendingMessage    string
	inProgressTrue    bool
	inProgressReason  string
	inProgressMessage string
	resizeStatus      corev1.PodResizeStatus
	resizeApplied     bool
}

func getPodCondition(pod *corev1.Pod, condType corev1.PodConditionType) *corev1.PodCondition {
	for i := range pod.Status.Conditions {
		cond := &pod.Status.Conditions[i]
		if cond.Type == condType {
			return cond
		}
	}
	return nil
}

func inspectPodResizeState(pod *corev1.Pod, targets map[string]containerCPUTarget) podResizeStateSnapshot {
	pending := getPodCondition(pod, corev1.PodResizePending)
	inProgress := getPodCondition(pod, corev1.PodResizeInProgress)

	state := podResizeStateSnapshot{
		resizeStatus:  pod.Status.Resize,
		resizeApplied: isPodCPUResizeApplied(pod, targets),
	}
	if pending != nil && pending.Status == corev1.ConditionTrue {
		state.pendingTrue = true
		state.pendingReason = pending.Reason
		state.pendingMessage = pending.Message
	}
	if inProgress != nil && inProgress.Status == corev1.ConditionTrue {
		state.inProgressTrue = true
		state.inProgressReason = inProgress.Reason
		state.inProgressMessage = inProgress.Message
	}
	return state
}

func (s podResizeStateSnapshot) hasResizeSignal() bool {
	return s.pendingTrue || s.inProgressTrue || s.resizeStatus != ""
}

func (s podResizeStateSnapshot) terminalError() error {
	if s.pendingTrue && s.pendingReason == corev1.PodReasonInfeasible {
		return fmt.Errorf("pod resize is infeasible: %s", s.pendingMessage)
	}
	if s.inProgressTrue && s.inProgressReason == corev1.PodReasonError {
		return fmt.Errorf("pod resize has error: %s", s.inProgressMessage)
	}
	if s.resizeStatus == corev1.PodResizeStatusInfeasible {
		return fmt.Errorf("pod resize is infeasible")
	}
	return nil
}

func (s podResizeStateSnapshot) isSettledWithoutDeferral() bool {
	return !s.pendingTrue && !s.inProgressTrue && s.resizeStatus != corev1.PodResizeStatusDeferred
}

func shouldReturnOnCompleted(state podResizeStateSnapshot, sawResizeSignal bool) bool {
	if state.resizeApplied {
		return true
	}
	return sawResizeSignal && state.isSettledWithoutDeferral()
}

func waitForPodResizeState(ctx context.Context, client *clients.ClientSet, namespace, name string,
	targetPod *corev1.Pod, timeout time.Duration) error {
	log := klog.FromContext(ctx).WithValues("pod", klog.KRef(namespace, name))
	if timeout <= 0 {
		return nil
	}
	targets := buildContainerCPUTargets(targetPod)

	sawResizeSignal := false
	lastPendingReason := ""
	lastPendingMessage := ""
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		pod, err := client.K8sClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		state := inspectPodResizeState(pod, targets)
		if state.hasResizeSignal() {
			sawResizeSignal = true
		}
		if state.pendingTrue {
			lastPendingReason = state.pendingReason
			lastPendingMessage = state.pendingMessage
		}
		if err := state.terminalError(); err != nil {
			return false, err
		}

		return shouldReturnOnCompleted(state, sawResizeSignal), nil
	})
	if err != nil {
		log.Error(err, "wait for pod resize state timeout")
		if lastPendingReason != "" {
			return fmt.Errorf("wait for pod resize state: %w (last pending reason=%s, message=%s)", err, lastPendingReason, lastPendingMessage)
		}
		return fmt.Errorf("wait for pod resize state: %w", err)
	}
	return nil
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
		name            string
		conditions      []corev1.PodCondition
		containerStatus []corev1.ContainerStatus
		resizeStatus    corev1.PodResizeStatus
		expectErr       string
	}{
		{
			name: "infeasible should fail",
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
			name:       "completed resize",
			conditions: nil,
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
			name:         "should not succeed without resize signal or applied status",
			conditions:   nil,
			resizeStatus: "",
			expectErr:    "wait for pod resize state",
		},
		{
			name: "deferred should timeout with pending reason",
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

			err = waitForPodResizeState(t.Context(), client, "default", "test-pod", targetPod, 500*time.Millisecond)
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
	defer infraInstance.Stop(t.Context())

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
	sbx, _, err := newSandboxFromSandboxSet(opts, infraInstance.Cache, infraInstance.Client, limiter)

	// Assertions
	assert.Nil(t, sbx, "sandbox should be nil when rate limited")
	assert.Error(t, err, "should return error when rate limited")

	// Check error message
	assert.Contains(t, err.Error(), "sandbox creation is not allowed by rate limiter", "error should indicate rate limit")
	assert.Contains(t, err.Error(), template, "error should contain template name")
}

func TestModifyPickedSandbox_CSIMount(t *testing.T) {
	tests := []struct {
		name             string
		lockType         infra.LockType
		opts             infra.ClaimSandboxOptions
		expectedAnnos    map[string]string
		notExpectedAnnos []string
	}{
		{
			name:     "with csi mount config",
			lockType: infra.LockTypeUpdate,
			opts: infra.ClaimSandboxOptions{
				CSIMount: &config.CSIMountOptions{
					MountOptionListRaw: `{"mountOptionList":[{"pvName":"test-pv","mountPath":"/data"}]}`,
				},
			},
			expectedAnnos: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest:            "", // should not be set
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `{"mountOptionList":[{"pvName":"test-pv","mountPath":"/data"}]}`,
			},
		},
		{
			name:     "with empty csi mount config",
			lockType: infra.LockTypeUpdate,
			opts: infra.ClaimSandboxOptions{
				CSIMount: &config.CSIMountOptions{
					MountOptionListRaw: "",
				},
			},
			notExpectedAnnos: []string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig,
			},
		},
		{
			name:     "with nil csi mount config",
			lockType: infra.LockTypeUpdate,
			opts: infra.ClaimSandboxOptions{
				CSIMount: nil,
			},
			notExpectedAnnos: []string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig,
			},
		},
		{
			name:     "with both init runtime and csi mount",
			lockType: infra.LockTypeUpdate,
			opts: infra.ClaimSandboxOptions{
				InitRuntime: &config.InitRuntimeOptions{
					EnvVars: map[string]string{
						"KEY1": "value1",
						"KEY2": "value2",
					},
				},
				CSIMount: &config.CSIMountOptions{
					MountOptionListRaw: `{"mountOptionList":[{"pvName":"csi-pv","mountPath":"/mnt/data"}]}`,
				},
			},
			expectedAnnos: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest:            `{"envVars":{"KEY1":"value1","KEY2":"value2"}}`,
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `{"mountOptionList":[{"pvName":"csi-pv","mountPath":"/mnt/data"}]}`,
			},
		},
		{
			name:     "csi mount with modifier",
			lockType: infra.LockTypeUpdate,
			opts: infra.ClaimSandboxOptions{
				Modifier: func(sbx infra.Sandbox) {
					sbx.SetAnnotations(map[string]string{
						"custom-annotation": "custom-value",
					})
				},
				CSIMount: &config.CSIMountOptions{
					MountOptionListRaw: `{"mountOptionList":[{"pvName":"test-pv","mountPath":"/custom"}]}`,
				},
			},
			expectedAnnos: map[string]string{
				"custom-annotation": "custom-value",
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `{"mountOptionList":[{"pvName":"test-pv","mountPath":"/custom"}]}`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test sandbox
			sbx := &Sandbox{
				Sandbox: &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test-sandbox",
						Namespace:   "default",
						Annotations: make(map[string]string),
					},
				},
			}

			// Call modifyPickedSandbox
			err := modifyPickedSandbox(sbx, tt.lockType, tt.opts)
			require.NoError(t, err)

			// Check expected annotations
			annotations := sbx.GetAnnotations()

			// Verify claim time annotation is always set
			assert.NotEmpty(t, annotations[v1alpha1.AnnotationClaimTime])

			// Verify claimed label is set
			labels := sbx.GetLabels()
			assert.Equal(t, v1alpha1.True, labels[v1alpha1.LabelSandboxIsClaimed])

			// Check expected annotations
			for key, expectedValue := range tt.expectedAnnos {
				if expectedValue != "" {
					assert.Equal(t, expectedValue, annotations[key], "annotation %s should match", key)
				} else {
					assert.Empty(t, annotations[key], "annotation %s should be empty", key)
				}
			}

			// Check not expected annotations
			for _, key := range tt.notExpectedAnnos {
				assert.Empty(t, annotations[key], "annotation %s should not be set", key)
			}
		})
	}
}

func TestPickAnAvailableSandbox_PrefersMatchingRevision(t *testing.T) {
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
				{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			},
			PodInfo: v1alpha1.PodInfo{PodIP: "1.2.3.4"},
		}
		created, err = client.ApiV1alpha1().Sandboxes(created.Namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
		if err != nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
		return created, nil
	}
	t.Cleanup(func() { DefaultCreateSandbox = origCreateSandbox })

	template := "test-prefer-template"
	updateRevision := "rev-new-abc"
	oldRevision := "rev-old-xyz"

	tests := []struct {
		name             string
		matchingCount    int
		nonMatchingCount int
		expectMatching   bool
	}{
		{
			name:           "all matching, picks matching",
			matchingCount:  3,
			expectMatching: true,
		},
		{
			name:             "all non-matching, picks non-matching",
			nonMatchingCount: 3,
			expectMatching:   false,
		},
		{
			name:             "mixed: should prefer matching",
			matchingCount:    1,
			nonMatchingCount: 5,
			expectMatching:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testInfra, clientSet := NewTestInfra(t)

			// Create the SandboxSet with UpdateRevision
			sbs := &v1alpha1.SandboxSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      template,
					Namespace: "default",
				},
				Spec: v1alpha1.SandboxSetSpec{
					Replicas: int32(tt.matchingCount + tt.nonMatchingCount),
					EmbeddedSandboxTemplate: v1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "test"}}},
						},
					},
				},
				Status: v1alpha1.SandboxSetStatus{
					UpdateRevision: updateRevision,
				},
			}
			_, err := clientSet.ApiV1alpha1().SandboxSets("default").Create(t.Context(), sbs, metav1.CreateOptions{})
			require.NoError(t, err)
			_, err = clientSet.ApiV1alpha1().SandboxSets("default").UpdateStatus(t.Context(), sbs, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				return testInfra.HasTemplate(template)
			}, 200*time.Millisecond, 5*time.Millisecond)

			now := metav1.Now()
			ownerRefs := []metav1.OwnerReference{*metav1.NewControllerRef(sbs, v1alpha1.SandboxSetControllerKind)}

			// Create matching (new revision) sandboxes
			for i := 0; i < tt.matchingCount; i++ {
				sbx := &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("match-%d", i), Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxTemplate:  template,
							v1alpha1.LabelSandboxIsClaimed: "false",
							v1alpha1.LabelTemplateHash:     updateRevision,
						},
						Annotations: map[string]string{}, CreationTimestamp: now, OwnerReferences: ownerRefs,
					},
					Status: v1alpha1.SandboxStatus{
						Phase:      v1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
						PodInfo:    v1alpha1.PodInfo{PodIP: "1.2.3.4"},
					},
				}
				CreateSandboxWithStatus(t, clientSet.SandboxClient, sbx)
			}

			// Create non-matching (old revision) sandboxes
			for i := 0; i < tt.nonMatchingCount; i++ {
				sbx := &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name: fmt.Sprintf("old-%d", i), Namespace: "default",
						Labels: map[string]string{
							v1alpha1.LabelSandboxTemplate:  template,
							v1alpha1.LabelSandboxIsClaimed: "false",
							v1alpha1.LabelTemplateHash:     oldRevision,
						},
						Annotations: map[string]string{}, CreationTimestamp: now, OwnerReferences: ownerRefs,
					},
					Status: v1alpha1.SandboxStatus{
						Phase:      v1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
						PodInfo:    v1alpha1.PodInfo{PodIP: "1.2.3.4"},
					},
				}
				CreateSandboxWithStatus(t, clientSet.SandboxClient, sbx)
			}

			// Wait for cache sync
			totalSandboxes := tt.matchingCount + tt.nonMatchingCount
			require.Eventually(t, func() bool {
				objs, err := testInfra.Cache.ListSandboxesInPool(template)
				return err == nil && len(objs) >= totalSandboxes
			}, 200*time.Millisecond, 5*time.Millisecond)

			// Claim a sandbox
			opts := infra.ClaimSandboxOptions{
				User:         "test-user",
				Template:     template,
				ClaimTimeout: 100 * time.Millisecond,
			}
			opts, err = ValidateAndInitClaimOptions(opts)
			require.NoError(t, err)

			sbx, _, claimErr := testInfra.ClaimSandbox(t.Context(), opts)
			require.NoError(t, claimErr)
			require.NotNil(t, sbx)

			hash := sbx.GetLabels()[v1alpha1.LabelTemplateHash]
			if tt.expectMatching {
				assert.Equal(t, updateRevision, hash, "should prefer sandbox with matching template hash")
			} else {
				assert.Equal(t, oldRevision, hash, "should fall back to non-matching sandbox")
			}
		})
	}
}

func TestModifyPickedSandbox_InitRuntime(t *testing.T) {
	tests := []struct {
		name             string
		opts             infra.ClaimSandboxOptions
		expectedAnnos    map[string]string
		notExpectedAnnos []string
	}{
		{
			name: "with init runtime options",
			opts: infra.ClaimSandboxOptions{
				InitRuntime: &config.InitRuntimeOptions{
					EnvVars: map[string]string{
						"ENV1": "value1",
						"ENV2": "value2",
					},
					AccessToken: "test-token",
				},
			},
			expectedAnnos: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest: `{"envVars":{"ENV1":"value1","ENV2":"value2"},"accessToken":"test-token"}`,
				v1alpha1.AnnotationRuntimeAccessToken: "test-token",
			},
		},
		{
			name: "with init runtime without access token",
			opts: infra.ClaimSandboxOptions{
				InitRuntime: &config.InitRuntimeOptions{
					EnvVars: map[string]string{
						"TEST_ENV": "test_value",
					},
				},
			},
			expectedAnnos: map[string]string{
				v1alpha1.AnnotationInitRuntimeRequest: `{"envVars":{"TEST_ENV":"test_value"}}`,
			},
			notExpectedAnnos: []string{
				v1alpha1.AnnotationRuntimeAccessToken,
			},
		},
		{
			name: "without init runtime",
			opts: infra.ClaimSandboxOptions{
				InitRuntime: nil,
			},
			notExpectedAnnos: []string{
				v1alpha1.AnnotationInitRuntimeRequest,
				v1alpha1.AnnotationRuntimeAccessToken,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &Sandbox{
				Sandbox: &v1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test-sandbox",
						Namespace:   "default",
						Annotations: make(map[string]string),
					},
				},
			}

			err := modifyPickedSandbox(sbx, infra.LockTypeUpdate, tt.opts)
			require.NoError(t, err)

			annotations := sbx.GetAnnotations()

			// Verify claim time annotation is always set
			assert.NotEmpty(t, annotations[v1alpha1.AnnotationClaimTime])

			// Check expected annotations
			for key, expectedValue := range tt.expectedAnnos {
				if expectedValue != "" {
					assert.Equal(t, expectedValue, annotations[key], "annotation %s should match", key)
				} else {
					assert.Empty(t, annotations[key], "annotation %s should be empty", key)
				}
			}

			// Check not expected annotations
			for _, key := range tt.notExpectedAnnos {
				assert.Empty(t, annotations[key], "annotation %s should not be set", key)
			}
		})
	}
}
