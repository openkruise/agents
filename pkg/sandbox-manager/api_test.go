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

package sandbox_manager

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

var testUser = "test-user"

func GetSbsOwnerReference() []metav1.OwnerReference {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sandboxset",
			UID:  "12345",
		},
	}
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind)}
}

func getSandboxForApiTest(name string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("test-sandbox-%s", name),
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner: testUser,
			},
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxIsClaimed: "true",
			},
			CreationTimestamp: metav1.Now(),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
		},
	}
}

func setupTestManager(t *testing.T, opts ...config.SandboxManagerOptions) (*SandboxManager, ctrlclient.Client) {
	t.Helper()
	infraOption := config.SandboxManagerOptions{}
	if len(opts) > 0 {
		infraOption = opts[0]
	}
	infraOption = config.InitOptions(infraOption)

	cache, fc, err := cachetest.NewTestCache(t)
	if err != nil {
		t.Fatalf("Failed to create test cache: %v", err)
	}

	proxyServer := proxy.NewServer(infraOption)
	infraInstance := sandboxcr.NewInfraBuilder(infraOption).
		WithCache(cache).
		WithAPIReader(fc).
		WithProxy(proxyServer).
		Build()

	if err := infraInstance.Run(t.Context()); err != nil {
		t.Fatalf("Failed to run infra: %v", err)
	}

	manager := &SandboxManager{
		infra: infraInstance,
		proxy: proxyServer,
	}

	return manager, fc
}

func CreateSandboxWithStatus(t *testing.T, client ctrlclient.Client, sbx *agentsv1alpha1.Sandbox) {
	t.Helper()
	ctx := t.Context()
	err := client.Create(ctx, sbx)
	assert.NoError(t, err)
	err = client.Status().Update(ctx, sbx)
	assert.NoError(t, err)
}

func TestSandboxManager_ClaimSandbox(t *testing.T) {
	utils.InitLogOutput()
	now := time.Now()
	username := "test-user"
	tests := []struct {
		name              string
		opts              infra.ClaimSandboxOptions
		templateSetup     map[string]int
		expectError       string
		expectedErrorCode errors.ErrorCode
		postCheck         func(t *testing.T, sbx infra.Sandbox)
	}{
		{
			name: "Non-existent template should return error",
			opts: infra.ClaimSandboxOptions{
				User:     username,
				Template: "non-existent-template",
			},
			expectError:       "non-existent-template not found",
			expectedErrorCode: errors.ErrorNotFound,
		},
		{
			name: "No user",
			opts: infra.ClaimSandboxOptions{
				Template: "exist-1",
			},
			templateSetup: map[string]int{
				"exist-1": 1,
			},
			expectError:       "user is required",
			expectedErrorCode: errors.ErrorInternal,
		},
		{
			name: "Claim with timeout",
			opts: infra.ClaimSandboxOptions{
				User:     username,
				Template: "exist-1",
				Modifier: func(sandbox infra.Sandbox) {
					sandbox.SetTimeout(infra.TimeoutOptions{
						ShutdownTime: now.Add(time.Second),
						PauseTime:    now.Add(time.Second),
					})
				},
			},
			templateSetup: map[string]int{
				"exist-1": 1,
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				timeout := sbx.GetTimeout()
				assert.WithinDuration(t, now.Add(time.Second), timeout.ShutdownTime, 2*time.Second)
				assert.WithinDuration(t, now.Add(time.Second), timeout.PauseTime, 2*time.Second)
			},
		},
		{
			name: "Claim failed with no stock",
			opts: infra.ClaimSandboxOptions{
				User:     username,
				Template: "exist-1",
			},
			templateSetup: map[string]int{
				"exist-1": 0,
			},
			expectError:       "no stock",
			expectedErrorCode: errors.ErrorInternal,
		},
		{
			name: "Claim with inplace update",
			opts: infra.ClaimSandboxOptions{
				User:     username,
				Template: "exist-1",
				InplaceUpdate: &config.InplaceUpdateOptions{
					Image: "new-image",
				},
			},
			templateSetup: map[string]int{
				"exist-1": 1,
			},
			postCheck: func(t *testing.T, sbx infra.Sandbox) {
				assert.Equal(t, "new-image", sbx.GetImage())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := setupTestManager(t)
			testIP := "1.2.3.4"
			createAt := metav1.Now()
			for template, available := range tt.templateSetup {
				sbs := &agentsv1alpha1.SandboxSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      template,
						Namespace: "default",
					},
				}
				err := client.Create(t.Context(), sbs)
				require.NoError(t, err)
				for i := 0; i < available; i++ {
					testSbx := &agentsv1alpha1.Sandbox{
						ObjectMeta: metav1.ObjectMeta{
							Name:      fmt.Sprintf("%s-%d", template, i),
							Namespace: "default",
							Labels: map[string]string{
								agentsv1alpha1.LabelSandboxTemplate: template,
							},
							CreationTimestamp: createAt,
							Annotations:       map[string]string{},
							OwnerReferences: []metav1.OwnerReference{
								{
									APIVersion:         agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String(),
									Kind:               agentsv1alpha1.SandboxSetControllerKind.Kind,
									Name:               "test-sandboxset",
									UID:                "12345",
									Controller:         ptr.To(true),
									BlockOwnerDeletion: ptr.To(true),
								},
							},
						},
						Spec: agentsv1alpha1.SandboxSpec{
							EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
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
						Status: agentsv1alpha1.SandboxStatus{
							Phase: agentsv1alpha1.SandboxRunning,
							Conditions: []metav1.Condition{
								{
									Type:   string(agentsv1alpha1.SandboxConditionReady),
									Status: metav1.ConditionTrue,
								},
							},
							PodInfo: agentsv1alpha1.PodInfo{
								PodIP: testIP,
							},
						},
					}
					CreateSandboxWithStatus(t, client, testSbx)
				}
				require.Eventually(t, func() bool {
					list, err := manager.GetInfra().GetCache().ListSandboxesInPool(t.Context(), infracache.ListSandboxesInPoolOptions{
						Pool: template,
					})
					if err != nil {
						return false
					}
					return len(list) == available
				}, 100*time.Millisecond, 5*time.Millisecond)
			}

			tt.opts.ClaimTimeout = 100 * time.Millisecond
			var claimed infra.Sandbox
			err := retry.OnError(wait.Backoff{
				Duration: 100 * time.Millisecond,
				Factor:   1,
				Steps:    20,
			}, func(err error) bool {
				return strings.Contains(err.Error(), "no stock")
			}, func() error {
				got, err := manager.ClaimSandbox(t.Context(), tt.opts)
				if err == nil {
					claimed = got
				}
				return err
			})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Equal(t, tt.expectedErrorCode, errors.GetErrCode(err))
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				tt.postCheck(t, claimed)
				// check route
				assert.Eventually(t, func() bool {
					route, ok := manager.proxy.LoadRoute(claimed.GetSandboxID())
					if !ok {
						return false
					}
					idMatch := route.ID == claimed.GetSandboxID()
					ipMatch := route.IP == testIP
					ownerMatch := route.Owner == username
					return idMatch && ipMatch && ownerMatch
				}, time.Second, 10*time.Millisecond)
			}
		})
	}
}

func TestSandboxManager_NamespaceAwareSandboxOptions(t *testing.T) {
	manager, client := setupTestManager(t)
	sandboxes := []*agentsv1alpha1.Sandbox{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sandbox-a",
				Namespace:   "team-a",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: testUser},
				Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				PodInfo:    agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sandbox-b",
				Namespace:   "team-b",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: testUser},
				Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				PodInfo:    agentsv1alpha1.PodInfo{PodIP: "10.0.0.2"},
			},
		},
	}
	for _, sbx := range sandboxes {
		CreateSandboxWithStatus(t, client, sbx)
	}

	list, _, err := manager.ListSandboxes(t.Context(), infra.SelectSandboxesOptions{
		Namespace: "team-a",
		User:      testUser,
	}, nil)
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "team-a", list[0].GetNamespace())
	assert.Equal(t, "sandbox-a", list[0].GetName())

	got, err := manager.GetClaimedSandbox(t.Context(), testUser, infra.GetClaimedSandboxOptions{
		Namespace: "team-b",
		SandboxID: sandboxutils.GetSandboxID(sandboxes[1]),
	})
	require.NoError(t, err)
	assert.Equal(t, "team-b", got.GetNamespace())
	assert.Equal(t, "sandbox-b", got.GetName())

	_, err = manager.GetClaimedSandbox(t.Context(), testUser, infra.GetClaimedSandboxOptions{
		Namespace: "team-a",
		SandboxID: sandboxutils.GetSandboxID(sandboxes[1]),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestSandboxManager_GetClaimedSandbox(t *testing.T) {
	manager, client := setupTestManager(t)

	runningSbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "default",
			Labels:    map[string]string{},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner: testUser,
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			},
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		},
	}

	pausedSbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "paused-pod",
			Namespace: "default",
			Labels:    map[string]string{},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner: testUser,
			},
		},
		Spec: agentsv1alpha1.SandboxSpec{
			Paused: true,
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPaused,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionPaused),
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	availableSbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "available-pod",
			Namespace:       "default",
			Labels:          map[string]string{},
			OwnerReferences: GetSbsOwnerReference(),
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			},
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		},
	}

	failedSbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "failed-pod",
			Namespace: "default",
			Labels:    map[string]string{},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner: testUser,
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionFalse,
				},
			},
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		},
	}

	sandboxes := []*agentsv1alpha1.Sandbox{runningSbx, pausedSbx, availableSbx, failedSbx}
	now := metav1.Now()
	for _, sbx := range sandboxes {
		sbx.CreationTimestamp = now
		sbx.Labels[agentsv1alpha1.LabelSandboxIsClaimed] = "true"
		CreateSandboxWithStatus(t, client, sbx)
	}

	tests := []struct {
		name              string
		sandboxID         string
		expectError       bool
		expectedErrorCode errors.ErrorCode
		expectedState     string
	}{
		{
			name:              "Get running pod",
			sandboxID:         "default--running-pod",
			expectError:       false,
			expectedErrorCode: "",
			expectedState:     agentsv1alpha1.SandboxStateRunning,
		},
		{
			name:              "Get paused pod",
			sandboxID:         "default--paused-pod",
			expectError:       false,
			expectedErrorCode: "",
			expectedState:     agentsv1alpha1.SandboxStatePaused,
		},
		{
			name:              "Get available pod should return error",
			sandboxID:         "default--available-pod",
			expectError:       true,
			expectedErrorCode: errors.ErrorNotFound,
			expectedState:     "",
		},
		{
			name:              "Get failed pod should return error",
			sandboxID:         "default--failed-pod",
			expectError:       true,
			expectedErrorCode: errors.ErrorBadRequest,
			expectedState:     "",
		},
		{
			name:              "Get non-existent pod should return error",
			sandboxID:         "default--non-existent-pod",
			expectError:       true,
			expectedErrorCode: errors.ErrorNotFound,
			expectedState:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			sbx, err := manager.GetClaimedSandbox(ctx, testUser, infra.GetClaimedSandboxOptions{SandboxID: tt.sandboxID})

			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if errors.GetErrCode(err) != tt.expectedErrorCode {
					t.Errorf("Expected error code %s, got %s", tt.expectedErrorCode, errors.GetErrCode(err))
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				if sbx == nil {
					t.Errorf("Expected pod but got nil")
				} else if state, reason := sbx.GetState(); state != tt.expectedState {
					t.Errorf("Expected pod state %s, got %s(%s)", tt.expectedState, state, reason)
				}
			}
		})
	}
}

func TestSandboxManager_Debug(t *testing.T) {
	manager, _ := setupTestManager(t)
	manager.GetDebugInfo()
}

func TestSandboxManager_PauseSandbox(t *testing.T) {
	utils.InitLogOutput()

	tests := []struct {
		name          string
		initSandbox   func(sbx *agentsv1alpha1.Sandbox)
		expectError   bool
		expectedState string
		expectedIP    string
	}{
		{
			name: "pause running sandbox successfully",
			initSandbox: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxRunning
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
				}
				sbx.Spec.Paused = false
				sbx.Status.PodInfo.PodIP = "10.0.0.1"
			},
			expectError:   false,
			expectedState: agentsv1alpha1.SandboxStatePaused,
			expectedIP:    "10.0.0.1",
		},
		{
			name: "pause already paused sandbox should fail",
			initSandbox: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxPaused
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionTrue,
					},
				}
				sbx.Spec.Paused = true
				sbx.Status.PodInfo.PodIP = "10.0.0.2"
			},
			expectError:   true,
			expectedState: "",
			expectedIP:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := setupTestManager(t)
			mgr := manager.GetInfra().GetCache().(*infracache.Cache).GetMockManager()

			sandbox := getSandboxForApiTest(tt.name)
			tt.initSandbox(sandbox)
			mgr.AddWaitReconcileKey(sandbox)

			CreateSandboxWithStatus(t, client, sandbox)

			// Get sandbox
			sbx, err := manager.GetClaimedSandbox(t.Context(), testUser, infra.GetClaimedSandboxOptions{
				SandboxID: sandboxutils.GetSandboxID(sandbox),
			})
			if err != nil {
				t.Fatalf("Failed to get sandbox: %v", err)
			}

			time.AfterFunc(50*time.Millisecond, func() {
				updated := &agentsv1alpha1.Sandbox{}
				getErr := client.Get(t.Context(), ctrlclient.ObjectKeyFromObject(sandbox), updated)
				assert.NoError(t, getErr)
				updated.Status.Phase = agentsv1alpha1.SandboxPaused
				updated.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionTrue,
					},
				}
				updateErr := client.Status().Update(t.Context(), updated)
				assert.NoError(t, updateErr)
			})

			// Pause sandbox
			err = manager.PauseSandbox(t.Context(), sbx, infra.PauseOptions{})

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			// Verify route is synced (InplaceRefresh should have updated it)
			route, ok := manager.proxy.LoadRoute(sandboxutils.GetSandboxID(sandbox))
			assert.True(t, ok, "Route should be synced")
			assert.Equal(t, sandboxutils.GetSandboxID(sandbox), route.ID)
			assert.Equal(t, tt.expectedIP, route.IP)
			assert.Equal(t, testUser, route.Owner)
			// Verify sandbox state matches expected
			if tt.expectedState != "" {
				actualSbx, err := manager.GetClaimedSandbox(t.Context(), testUser, infra.GetClaimedSandboxOptions{
					SandboxID: sandboxutils.GetSandboxID(sandbox),
				})
				if err == nil {
					actualState, _ := actualSbx.GetState()
					assert.Equal(t, tt.expectedState, actualState, "Sandbox state should match")
				}
			}
		})
	}
}

func TestSandboxManager_ResumeSandbox(t *testing.T) {
	utils.InitLogOutput()

	tests := []struct {
		name          string
		initSandbox   func(sbx *agentsv1alpha1.Sandbox)
		expectError   bool
		expectedState string
		expectedIP    string
		ipChanged     bool
	}{
		{
			name: "resume paused sandbox successfully",
			initSandbox: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxPaused
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionTrue,
					},
				}
				sbx.Spec.Paused = true
				sbx.Status.PodInfo.PodIP = "10.0.0.1"
			},
			expectError:   false,
			expectedState: agentsv1alpha1.SandboxStateRunning,
			expectedIP:    "10.0.0.1",
			ipChanged:     false,
		},
		{
			name: "resume paused sandbox with IP change",
			initSandbox: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxPaused
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionTrue,
					},
				}
				sbx.Spec.Paused = true
				sbx.Status.PodInfo.PodIP = "10.0.0.1"
			},
			expectError:   false,
			expectedState: agentsv1alpha1.SandboxStateRunning,
			expectedIP:    "10.0.0.2", // IP changed after resume
			ipChanged:     true,
		},
		{
			name: "resume already running sandbox should fail",
			initSandbox: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxRunning
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
				}
				sbx.Spec.Paused = false
				sbx.Status.PodInfo.PodIP = "10.0.0.1"
			},
			expectError:   true,
			expectedState: "",
			expectedIP:    "",
			ipChanged:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := setupTestManager(t)
			mgr := manager.GetInfra().GetCache().(*infracache.Cache).GetMockManager()

			sandbox := getSandboxForApiTest(tt.name)
			tt.initSandbox(sandbox)

			CreateSandboxWithStatus(t, client, sandbox)
			mgr.AddWaitReconcileKey(sandbox)

			// Get sandbox
			sbx, err := manager.GetClaimedSandbox(t.Context(), testUser, infra.GetClaimedSandboxOptions{
				SandboxID: sandboxutils.GetSandboxID(sandbox),
			})
			if err != nil {
				t.Fatalf("Failed to get sandbox: %v", err)
			}

			// Set initial route in proxy
			initialRoute := sbx.GetRoute()
			manager.proxy.SetRoute(t.Context(), initialRoute)

			// Resume sandbox
			if !tt.expectError {
				// Simulate controller updating sandbox status after resume
				time.AfterFunc(50*time.Millisecond, func() {
					updated := &agentsv1alpha1.Sandbox{}
					err := client.Get(t.Context(), ctrlclient.ObjectKeyFromObject(sandbox), updated)
					if err != nil {
						return
					}
					updated.Status.Phase = agentsv1alpha1.SandboxRunning
					updated.Status.Conditions = []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
						},
					}
					if tt.ipChanged {
						updated.Status.PodInfo.PodIP = tt.expectedIP
					}
					_ = client.Status().Update(t.Context(), updated)
				})
			}

			err = manager.ResumeSandbox(t.Context(), sbx)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			// Verify route is synced
			route, ok := manager.proxy.LoadRoute(sandboxutils.GetSandboxID(sandbox))
			assert.True(t, ok, "Route should be synced")
			assert.Equal(t, sandboxutils.GetSandboxID(sandbox), route.ID)
			assert.Equal(t, tt.expectedIP, route.IP)
			assert.Equal(t, testUser, route.Owner)
			assert.Equal(t, tt.expectedState, route.State)
		})
	}
}

func TestSandboxManager_CloneSandbox(t *testing.T) {
	utils.InitLogOutput()

	checkpointID := "test-checkpoint-clone"
	user := "test-user"

	// Define context key types for sandbox override
	type sbxOverrideKey struct{}
	type sbxOverride struct {
		Name       string
		RuntimeURL string
	}

	tests := []struct {
		name              string
		opts              infra.CloneSandboxOptions
		sbxOverride       sbxOverride
		setupResources    bool
		expectError       bool
		expectedErrorCode errors.ErrorCode
	}{
		{
			name: "successful clone",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     checkpointID,
				WaitReadyTimeout: 30 * time.Second,
			},
			sbxOverride:    sbxOverride{Name: "test-sandbox-clone-success"},
			setupResources: true,
			expectError:    false,
		},
		{
			name: "clone with non-existent checkpoint",
			opts: infra.CloneSandboxOptions{
				User:             user,
				CheckPointID:     "non-existent-checkpoint",
				WaitReadyTimeout: 30 * time.Second,
			},
			setupResources:    false,
			expectError:       true,
			expectedErrorCode: errors.ErrorInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := setupTestManager(t)

			// Decorator: DefaultCreateSandbox - set sandbox ready after creation
			origCreateSandbox := sandboxcr.DefaultCreateSandbox
			sandboxcr.DefaultCreateSandbox = func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, c ctrlclient.Client) (*agentsv1alpha1.Sandbox, error) {
				if override, ok := ctx.Value(sbxOverrideKey{}).(sbxOverride); ok {
					if override.Name != "" {
						sbx.Name = override.Name
					}
				}
				created, err := origCreateSandbox(ctx, sbx, c)
				if err != nil {
					return nil, err
				}
				// Update Sandbox status to Ready
				created.Status = agentsv1alpha1.SandboxStatus{
					Phase:              agentsv1alpha1.SandboxRunning,
					ObservedGeneration: created.Generation,
					Conditions: []metav1.Condition{
						{
							Type:   string(agentsv1alpha1.SandboxConditionReady),
							Status: metav1.ConditionTrue,
							Reason: agentsv1alpha1.SandboxReadyReasonPodReady,
						},
					},
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "1.2.3.4",
					},
				}
				if err = c.Status().Update(ctx, created); err != nil {
					return nil, err
				}
				return created, nil
			}
			t.Cleanup(func() { sandboxcr.DefaultCreateSandbox = origCreateSandbox })

			if tt.setupResources {
				// Create SandboxTemplate with same name as checkpoint
				sbt := &agentsv1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      checkpointID,
						Namespace: "default",
					},
					Spec: agentsv1alpha1.SandboxTemplateSpec{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "test-image"},
								},
							},
						},
					},
				}
				err := client.Create(t.Context(), sbt)
				require.NoError(t, err)

				// Create Checkpoint with same name as SandboxTemplate
				cp := &agentsv1alpha1.Checkpoint{
					ObjectMeta: metav1.ObjectMeta{
						Name:      checkpointID,
						Namespace: "default",
						Labels: map[string]string{
							agentsv1alpha1.LabelSandboxTemplate: checkpointID,
						},
					},
					Status: agentsv1alpha1.CheckpointStatus{
						CheckpointId: checkpointID,
					},
				}
				err = client.Create(t.Context(), cp)
				require.NoError(t, err)
				cp.Status.CheckpointId = checkpointID
				err = client.Status().Update(t.Context(), cp)
				require.NoError(t, err)
			}

			// Build context with sbxOverride if needed
			ctx := t.Context()
			if tt.sbxOverride.Name != "" {
				ctx = context.WithValue(ctx, sbxOverrideKey{}, tt.sbxOverride)
			}

			tt.opts.CloneTimeout = 100 * time.Millisecond
			// Call CloneSandbox
			sbx, err := manager.CloneSandbox(ctx, tt.opts)

			if tt.expectError {
				require.Error(t, err)
				assert.Equal(t, tt.expectedErrorCode, errors.GetErrCode(err))
				assert.Nil(t, sbx)
			} else {
				require.NoError(t, err)
				require.NotNil(t, sbx)
				assert.Equal(t, user, sbx.GetAnnotations()[agentsv1alpha1.AnnotationOwner])
				assert.Equal(t, checkpointID, sbx.GetLabels()[agentsv1alpha1.LabelSandboxTemplate])
				assert.Equal(t, "true", sbx.GetLabels()[agentsv1alpha1.LabelSandboxIsClaimed])
			}
		})
	}
}

func TestSandboxManager_GetOwnerOfSandbox(t *testing.T) {
	tests := []struct {
		name          string
		sandboxID     string
		setupRoute    bool
		expectedOwner string
		expectedOk    bool
	}{
		{
			name:          "non-existent sandbox returns empty owner and false",
			sandboxID:     "non-existent-sandbox",
			setupRoute:    false,
			expectedOwner: "",
			expectedOk:    false,
		},
		{
			name:          "existing sandbox returns owner and true",
			sandboxID:     "default--test-sandbox",
			setupRoute:    true,
			expectedOwner: testUser,
			expectedOk:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, _ := setupTestManager(t)

			if tt.setupRoute {
				manager.proxy.SetRoute(t.Context(), proxy.Route{
					ID:    tt.sandboxID,
					IP:    "10.0.0.1",
					Owner: testUser,
					State: agentsv1alpha1.SandboxStateRunning,
				})
			}

			owner, ok := manager.GetOwnerOfSandbox(tt.sandboxID)

			assert.Equal(t, tt.expectedOk, ok)
			assert.Equal(t, tt.expectedOwner, owner)
		})
	}
}

func TestSandboxManager_ListSandboxes(t *testing.T) {
	utils.InitLogOutput()
	manager, client := setupTestManager(t)

	// Create 4 sandboxes with names that sort alphabetically
	sandboxNames := []string{"aaa-sandbox", "bbb-sandbox", "ccc-sandbox", "ddd-sandbox"}
	for _, name := range sandboxNames {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: "default",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: testUser,
				},
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxIsClaimed: "true",
				},
				CreationTimestamp: metav1.Now(),
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase: agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
				},
				PodInfo: agentsv1alpha1.PodInfo{
					PodIP: "10.0.0.1",
				},
			},
		}
		CreateSandboxWithStatus(t, client, sbx)
	}

	t.Run("without paginator", func(t *testing.T) {
		sandboxes, nextToken, err := manager.ListSandboxes(t.Context(), infra.SelectSandboxesOptions{User: testUser}, nil)

		assert.NoError(t, err)
		assert.Empty(t, nextToken, "nextToken should be empty when paginator is nil")
		assert.Len(t, sandboxes, len(sandboxNames), "should return all sandboxes")
	})

	t.Run("with paginator", func(t *testing.T) {
		paginator := &utils.Paginator[infra.Sandbox]{
			Limit: 2, // Limit to 2 items per page, so 4 sandboxes will produce nextToken
			GetKey: func(sbx infra.Sandbox) string {
				return sbx.GetName()
			},
			Filter: func(sbx infra.Sandbox) bool {
				return true
			},
		}

		sandboxes, nextToken, err := manager.ListSandboxes(t.Context(), infra.SelectSandboxesOptions{User: testUser}, paginator)

		assert.NoError(t, err)
		assert.Len(t, sandboxes, 2, "should return limited number of sandboxes")
		assert.NotEmpty(t, nextToken, "nextToken should not be empty when there are more items")

		// Verify sandboxes are sorted by name
		assert.Equal(t, "aaa-sandbox", sandboxes[0].GetName(), "first sandbox should be aaa-sandbox")
		assert.Equal(t, "bbb-sandbox", sandboxes[1].GetName(), "second sandbox should be bbb-sandbox")

		// Verify nextToken is the key of the last item
		assert.Equal(t, "bbb-sandbox", nextToken, "nextToken should be the name of the last returned sandbox")
	})
}

func TestSandboxManager_DeleteSandbox(t *testing.T) {
	utils.InitLogOutput()
	manager, client := setupTestManager(t)

	tests := []struct {
		name          string
		initSandbox   func(sbx *agentsv1alpha1.Sandbox)
		mockDeleteErr error
		expectError   bool
	}{
		{
			name: "delete running sandbox successfully",
			initSandbox: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxRunning
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
				}
				sbx.Status.PodInfo.PodIP = "10.0.0.1"
			},
			mockDeleteErr: nil,
			expectError:   false,
		},
		{
			name: "delete paused sandbox successfully",
			initSandbox: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxPaused
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionPaused),
						Status: metav1.ConditionTrue,
					},
				}
				sbx.Spec.Paused = true
				sbx.Status.PodInfo.PodIP = "10.0.0.2"
			},
			mockDeleteErr: nil,
			expectError:   false,
		},
		{
			name: "delete sandbox with kill error",
			initSandbox: func(sbx *agentsv1alpha1.Sandbox) {
				sbx.Status.Phase = agentsv1alpha1.SandboxRunning
				sbx.Status.Conditions = []metav1.Condition{
					{
						Type:   string(agentsv1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
				}
				sbx.Status.PodInfo.PodIP = "10.0.0.3"
			},
			mockDeleteErr: fmt.Errorf("mock delete error"),
			expectError:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      fmt.Sprintf("test-sandbox-delete-%s", tt.name),
					Namespace: "default",
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationOwner: testUser,
					},
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxIsClaimed: "true",
					},
					CreationTimestamp: metav1.Now(),
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
			}
			tt.initSandbox(sandbox)

			CreateSandboxWithStatus(t, client, sandbox)

			// Get sandbox
			sbx, err := manager.GetClaimedSandbox(t.Context(), testUser, infra.GetClaimedSandboxOptions{
				SandboxID: sandboxutils.GetSandboxID(sandbox),
			})
			if err != nil {
				t.Fatalf("Failed to get sandbox: %v", err)
			}

			// Set initial route
			initialRoute := sbx.GetRoute()
			manager.proxy.SetRoute(t.Context(), initialRoute)

			// Decorator: DefaultDeleteSandbox - control delete result (set after getting sandbox)
			if tt.mockDeleteErr != nil {
				origDeleteSandbox := sandboxcr.DefaultDeleteSandbox
				sandboxcr.DefaultDeleteSandbox = func(ctx context.Context, s *agentsv1alpha1.Sandbox, c ctrlclient.Client) error {
					return tt.mockDeleteErr
				}
				t.Cleanup(func() { sandboxcr.DefaultDeleteSandbox = origDeleteSandbox })
			}

			// Delete sandbox
			err = manager.DeleteSandbox(t.Context(), sbx)

			if tt.expectError {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.mockDeleteErr.Error())
			} else {
				assert.NoError(t, err)
				// After successful deletion, verify sandbox is not found
				// Use a short timeout context to avoid long retry in GetClaimedSandbox
				ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
				defer cancel()
				_, getErr := manager.GetClaimedSandbox(ctx, testUser, infra.GetClaimedSandboxOptions{
					SandboxID: sandboxutils.GetSandboxID(sandbox),
				})
				assert.Error(t, getErr, "sandbox should not be found after deletion")
			}
		})
	}
}

func TestSandboxManager_ListCheckpoints(t *testing.T) {
	utils.InitLogOutput()

	tests := []struct {
		name                  string
		user                  string
		setupCheckpoints      func(client ctrlclient.Client)
		paginator             *utils.Paginator[infra.CheckpointInfo]
		expectError           bool
		expectedErrorCode     errors.ErrorCode
		expectedCheckpointIDs []string
		expectedNextToken     string
		expectedCount         int
	}{
		{
			name: "list checkpoints without paginator",
			user: "user1",
			setupCheckpoints: func(client ctrlclient.Client) {
				createCheckpointForTest(t, client, "cp-1", "user1", "sandbox-1", "checkpoint-id-1", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-2", "user1", "sandbox-2", "checkpoint-id-2", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-3", "user1", "sandbox-3", "checkpoint-id-3", agentsv1alpha1.CheckpointSucceeded)
			},
			paginator:             nil,
			expectError:           false,
			expectedCheckpointIDs: []string{"checkpoint-id-1", "checkpoint-id-2", "checkpoint-id-3"},
			expectedNextToken:     "",
			expectedCount:         3,
		},
		{
			name: "list checkpoints with paginator",
			user: "user1",
			setupCheckpoints: func(client ctrlclient.Client) {
				createCheckpointForTest(t, client, "cp-a", "user1", "sandbox-a", "checkpoint-id-a", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-b", "user1", "sandbox-b", "checkpoint-id-b", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-c", "user1", "sandbox-c", "checkpoint-id-c", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-d", "user1", "sandbox-d", "checkpoint-id-d", agentsv1alpha1.CheckpointSucceeded)
			},
			paginator: &utils.Paginator[infra.CheckpointInfo]{
				Limit: 2,
				GetKey: func(cp infra.CheckpointInfo) string {
					return cp.Name
				},
				Filter: func(cp infra.CheckpointInfo) bool {
					return true
				},
			},
			expectError:           false,
			expectedCheckpointIDs: []string{"checkpoint-id-a", "checkpoint-id-b"},
			expectedNextToken:     "cp-b",
			expectedCount:         2,
		},
		{
			name: "list checkpoints with paginator and next token",
			user: "user1",
			setupCheckpoints: func(client ctrlclient.Client) {
				createCheckpointForTest(t, client, "cp-a", "user1", "sandbox-a", "checkpoint-id-a", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-b", "user1", "sandbox-b", "checkpoint-id-b", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-c", "user1", "sandbox-c", "checkpoint-id-c", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-d", "user1", "sandbox-d", "checkpoint-id-d", agentsv1alpha1.CheckpointSucceeded)
			},
			paginator: &utils.Paginator[infra.CheckpointInfo]{
				Limit:     2,
				NextToken: "cp-b",
				GetKey: func(cp infra.CheckpointInfo) string {
					return cp.Name
				},
				Filter: func(cp infra.CheckpointInfo) bool {
					return true
				},
			},
			expectError:           false,
			expectedCheckpointIDs: []string{"checkpoint-id-c", "checkpoint-id-d"},
			expectedNextToken:     "",
			expectedCount:         2,
		},
		{
			name: "filter checkpoints by user",
			user: "user1",
			setupCheckpoints: func(client ctrlclient.Client) {
				createCheckpointForTest(t, client, "cp-user1", "user1", "sandbox-1", "checkpoint-user1", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-user2", "user2", "sandbox-2", "checkpoint-user2", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-user3", "user3", "sandbox-3", "checkpoint-user3", agentsv1alpha1.CheckpointSucceeded)
			},
			paginator:             nil,
			expectError:           false,
			expectedCheckpointIDs: []string{"checkpoint-user1"},
			expectedNextToken:     "",
			expectedCount:         1,
		},
		{
			name: "only return succeeded checkpoints",
			user: "user1",
			setupCheckpoints: func(client ctrlclient.Client) {
				createCheckpointForTest(t, client, "cp-succeeded", "user1", "sandbox-1", "checkpoint-succeeded", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-pending", "user1", "sandbox-2", "checkpoint-pending", agentsv1alpha1.CheckpointPending)
				createCheckpointForTest(t, client, "cp-failed", "user1", "sandbox-3", "checkpoint-failed", agentsv1alpha1.CheckpointFailed)
				createCheckpointForTest(t, client, "cp-creating", "user1", "sandbox-4", "checkpoint-creating", agentsv1alpha1.CheckpointCreating)
			},
			paginator:             nil,
			expectError:           false,
			expectedCheckpointIDs: []string{"checkpoint-succeeded"},
			expectedNextToken:     "",
			expectedCount:         1,
		},
		{
			name: "return empty list when user has no checkpoints",
			user: "non-existent-user",
			setupCheckpoints: func(client ctrlclient.Client) {
				createCheckpointForTest(t, client, "cp-1", "user1", "sandbox-1", "checkpoint-id-1", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-2", "user2", "sandbox-2", "checkpoint-id-2", agentsv1alpha1.CheckpointSucceeded)
			},
			paginator:             nil,
			expectError:           false,
			expectedCheckpointIDs: []string{},
			expectedNextToken:     "",
			expectedCount:         0,
		},
		{
			name: "paginator with filter",
			user: "user1",
			setupCheckpoints: func(client ctrlclient.Client) {
				createCheckpointForTest(t, client, "cp-a", "user1", "sandbox-a", "checkpoint-a", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-b", "user1", "sandbox-b", "checkpoint-b", agentsv1alpha1.CheckpointSucceeded)
				createCheckpointForTest(t, client, "cp-c", "user1", "sandbox-c", "checkpoint-c", agentsv1alpha1.CheckpointSucceeded)
			},
			paginator: &utils.Paginator[infra.CheckpointInfo]{
				Limit: 10,
				GetKey: func(cp infra.CheckpointInfo) string {
					return cp.Name
				},
				Filter: func(cp infra.CheckpointInfo) bool {
					// Only return checkpoints with name starting with "cp-a" or "cp-c"
					return cp.Name == "cp-a" || cp.Name == "cp-c"
				},
			},
			expectError:           false,
			expectedCheckpointIDs: []string{"checkpoint-a", "checkpoint-c"},
			expectedNextToken:     "",
			expectedCount:         2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := setupTestManager(t)

			// Setup checkpoints for this test case
			tt.setupCheckpoints(client)

			// Call ListCheckpoints
			checkpoints, nextToken, err := manager.ListCheckpoints(t.Context(), infra.SelectSucceededCheckpointsOptions{
				User: tt.user,
			}, tt.paginator)

			if tt.expectError {
				require.Error(t, err)
				assert.Equal(t, tt.expectedErrorCode, errors.GetErrCode(err))
			} else {
				require.NoError(t, err)
				assert.Len(t, checkpoints, tt.expectedCount, "checkpoint count mismatch")
				assert.Equal(t, tt.expectedNextToken, nextToken, "nextToken mismatch")

				// Verify checkpoint IDs
				actualIDs := make([]string, len(checkpoints))
				for i, cp := range checkpoints {
					actualIDs[i] = cp.CheckpointID
				}
				assert.ElementsMatch(t, tt.expectedCheckpointIDs, actualIDs, "checkpoint IDs mismatch")
			}
		})
	}
}

// Helper function to create a checkpoint for testing
func createCheckpointForTest(t *testing.T, client ctrlclient.Client, name, owner, sandboxID, checkpointID string, phase agentsv1alpha1.CheckpointPhase) {
	t.Helper()
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner:     owner,
				agentsv1alpha1.AnnotationSandboxID: sandboxID,
			},
		},
	}
	err := client.Create(t.Context(), cp)
	require.NoError(t, err)
	cp.Status = agentsv1alpha1.CheckpointStatus{
		Phase:        phase,
		CheckpointId: checkpointID,
	}
	err = client.Status().Update(t.Context(), cp)
	require.NoError(t, err)
}

func TestSandboxManager_DeleteCheckpoint(t *testing.T) {
	utils.InitLogOutput()
	namespace := "default"

	tests := []struct {
		name                 string
		checkpointID         string
		user                 string // the user requesting deletion
		setup                bool   // whether to create checkpoint + template
		withOwnerRef         bool
		mockDeleteTemplate   error
		mockDeleteCheckpoint error
		expectError          string // empty string means no error expected, non-empty means error should contain this text
	}{
		{
			name:         "delete checkpoint with owner reference successfully",
			checkpointID: "cp-success-ownerref",
			user:         "test-user",
			setup:        true,
			withOwnerRef: true,
			expectError:  "",
		},
		{
			name:         "delete checkpoint without owner reference successfully",
			checkpointID: "cp-success-no-ownerref",
			user:         "test-user",
			setup:        true,
			withOwnerRef: false,
			expectError:  "",
		},
		{
			name:         "checkpoint not found",
			checkpointID: "non-existent-checkpoint",
			user:         "test-user",
			setup:        false,
			expectError:  "not found in cache",
		},
		{
			name:               "delete template fails",
			checkpointID:       "cp-tmpl-fail",
			user:               "test-user",
			setup:              true,
			withOwnerRef:       true,
			mockDeleteTemplate: fmt.Errorf("mock template delete error"),
			expectError:        "mock template delete error",
		},
		{
			name:                 "explicit delete checkpoint fails",
			checkpointID:         "cp-explicit-fail",
			user:                 "test-user",
			setup:                true,
			withOwnerRef:         false,
			mockDeleteCheckpoint: fmt.Errorf("mock checkpoint delete error"),
			expectError:          "mock checkpoint delete error",
		},
		{
			name:         "owner mismatch",
			checkpointID: "cp-owner-mismatch",
			user:         "different-user",
			setup:        true,
			withOwnerRef: true,
			expectError:  "is not owned by user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager, client := setupTestManager(t)

			if tt.setup {
				// Create SandboxTemplate
				tmpl := &agentsv1alpha1.SandboxTemplate{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tt.checkpointID,
						Namespace: namespace,
					},
					Spec: agentsv1alpha1.SandboxTemplateSpec{
						Template: &corev1.PodTemplateSpec{
							Spec: corev1.PodSpec{
								Containers: []corev1.Container{
									{Name: "main", Image: "test"},
								},
							},
						},
					},
				}
				err := client.Create(t.Context(), tmpl)
				require.NoError(t, err)

				// Create Checkpoint with owner annotation
				cp := &agentsv1alpha1.Checkpoint{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tt.checkpointID,
						Namespace: namespace,
						Annotations: map[string]string{
							agentsv1alpha1.AnnotationOwner: "test-user",
						},
					},
				}
				if tt.withOwnerRef {
					cp.OwnerReferences = []metav1.OwnerReference{
						{
							APIVersion:         agentsv1alpha1.SandboxTemplateControllerKind.GroupVersion().String(),
							Kind:               agentsv1alpha1.SandboxTemplateControllerKind.Kind,
							Name:               tmpl.Name,
							UID:                tmpl.UID,
							Controller:         ptr.To(true),
							BlockOwnerDeletion: ptr.To(true),
						},
					}
				}
				err = client.Create(t.Context(), cp)
				require.NoError(t, err)

				// Update status with checkpointId
				cp.Status.CheckpointId = tt.checkpointID
				err = client.Status().Update(t.Context(), cp)
				require.NoError(t, err)

				// Wait for informer sync
				require.Eventually(t, func() bool {
					return manager.GetInfra().HasCheckpoint(t.Context(), infra.HasCheckpointOptions{
						CheckpointID: tt.checkpointID,
					})
				}, time.Second, 10*time.Millisecond)

				// Cleanup
				t.Cleanup(func() {
					_ = client.Delete(t.Context(), tmpl)
					_ = client.Delete(t.Context(), cp)
				})
			}

			// Set up decorator mocks
			if tt.mockDeleteTemplate != nil {
				orig := sandboxcr.DefaultDeleteSandboxTemplate
				sandboxcr.DefaultDeleteSandboxTemplate = func(ctx context.Context, c ctrlclient.Client, namespace, name string) error {
					return tt.mockDeleteTemplate
				}
				t.Cleanup(func() { sandboxcr.DefaultDeleteSandboxTemplate = orig })
			}
			if tt.mockDeleteCheckpoint != nil {
				orig := sandboxcr.DefaultDeleteCheckpointCR
				sandboxcr.DefaultDeleteCheckpointCR = func(ctx context.Context, c ctrlclient.Client, namespace, name string) error {
					return tt.mockDeleteCheckpoint
				}
				t.Cleanup(func() { sandboxcr.DefaultDeleteCheckpointCR = orig })
			}

			err := manager.DeleteCheckpoint(t.Context(), tt.user, infra.DeleteCheckpointOptions{
				CheckpointID: tt.checkpointID,
			})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
