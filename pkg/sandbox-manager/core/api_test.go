package core

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/k8s"
	"github.com/openkruise/agents/pkg/sandbox-manager/utils"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func setupTestManager(t *testing.T) *SandboxManager {
	// 创建fake client set
	client := clients.NewFakeClientSet()
	abs, err := filepath.Abs("../../../assets/template/builtin_templates")
	if err != nil {
		t.Fatalf("Failed to get template dir: %v", err)
	}
	manager, err := NewSandboxManager("default", abs, client, nil, nil, consts.InfraK8S)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	err = manager.infra.Run(context.Background())
	if err != nil {
		t.Fatalf("Failed to run infra: %v", err)
	}

	return manager
}

func CreatePodWithStatus(t *testing.T, client kubernetes.Interface, pod *corev1.Pod) {
	ctx := context.Background()
	_, err := client.CoreV1().Pods("default").Create(ctx, pod, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = client.CoreV1().Pods("default").UpdateStatus(ctx, pod, metav1.UpdateOptions{})
	assert.NoError(t, err)
}

func TestSandboxManager_ClaimPodAsSandbox(t *testing.T) {
	utils.InitKLogOutput()
	tests := []struct {
		name              string
		template          string
		timeout           int
		expectError       bool
		expectedErrorCode errors.ErrorCode
	}{
		{
			name:              "Non-existent template should return error",
			template:          "non-existent-template",
			timeout:           0,
			expectError:       true,
			expectedErrorCode: errors.ErrorNotFound,
		},
		{
			name:     "Claim success",
			template: "code-interpreter",
			timeout:  1234,
		},
		{
			name:              "Claim failed",
			template:          "browser", // pool has no pending sandboxes
			timeout:           1234,
			expectError:       true,
			expectedErrorCode: errors.ErrorInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := setupTestManager(t)
			client := manager.client

			// 创建测试用的pod
			testPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxID:    "test-pod",
						consts.LabelSandboxState: consts.SandboxStatePending,
						consts.LabelSandboxPool:  "code-interpreter", // loaded from built-in template
					},
					Annotations: map[string]string{},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
					PodIP: "1.2.3.4",
				},
			}

			CreatePodWithStatus(t, client.K8sClient, testPod)

			// 等待informer同步
			time.Sleep(100 * time.Millisecond)

			get, err := client.K8sClient.CoreV1().Pods("default").Get(context.Background(), testPod.Name, metav1.GetOptions{})
			assert.NoError(t, err)
			sbx, err := manager.GetInfra().GetSandbox(get.Name)
			assert.NoError(t, err)
			assert.Equal(t, testPod.Name, sbx.GetName())
			pool, ok := manager.GetInfra().GetPoolByObject(sbx)
			assert.True(t, ok)
			err = pool.(*k8s.Pool).Refresh(context.Background())
			assert.NoError(t, err)

			got, err := manager.ClaimSandbox(context.Background(), "test-user", tt.template, tt.timeout)

			if tt.expectError {
				assert.Error(t, err)
				assert.Equal(t, tt.expectedErrorCode, errors.GetErrCode(err))
			} else {
				assert.NoError(t, err)
				time.Sleep(100 * time.Millisecond)
				// check timer
				err = got.LoadTimers(func(after time.Duration, eventType consts.EventType) {
					offset := time.Duration(tt.timeout)*time.Second - after
					assert.True(t, offset > 0)
					assert.True(t, offset < time.Second)
					assert.Equal(t, consts.SandboxKill, eventType)
				})
				assert.NoError(t, err)
				// check router
				route, ok := manager.proxy.LoadRoute(got.GetName())
				assert.True(t, ok)
				assert.Equal(t, testPod.Name, route.ID)
				assert.Equal(t, testPod.Status.PodIP, route.IP)
				assert.Equal(t, "test-user", route.Owner)
			}
		})
	}
}

func TestSandboxManager_GetClaimedPod(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client

	// 创建测试用的pods
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "running-pod",
				consts.LabelSandboxState: consts.SandboxStateRunning,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	pausedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "paused-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "paused-pod",
				consts.LabelSandboxState: consts.SandboxStatePaused,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "pending-pod",
				consts.LabelSandboxState: consts.SandboxStatePending,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	// 添加pods到fake client
	pods := []*corev1.Pod{runningPod, pausedPod, pendingPod}
	for _, pod := range pods {
		_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod %s: %v", pod.Name, err)
		}
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	tests := []struct {
		name              string
		sandboxID         string
		expectError       bool
		expectedErrorCode errors.ErrorCode
		expectedState     string
	}{
		{
			name:              "Get running pod",
			sandboxID:         "running-pod",
			expectError:       false,
			expectedErrorCode: "",
			expectedState:     consts.SandboxStateRunning,
		},
		{
			name:              "Get paused pod",
			sandboxID:         "paused-pod",
			expectError:       false,
			expectedErrorCode: "",
			expectedState:     consts.SandboxStatePaused,
		},
		{
			name:              "Get pending pod should return error",
			sandboxID:         "pending-pod",
			expectError:       true,
			expectedErrorCode: errors.ErrorNotFound,
			expectedState:     "",
		},
		{
			name:              "Get non-existent pod should return error",
			sandboxID:         "non-existent-pod",
			expectError:       true,
			expectedErrorCode: errors.ErrorNotFound,
			expectedState:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx, err := manager.GetClaimedSandbox(tt.sandboxID)

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
				} else if sbx.GetState() != tt.expectedState {
					t.Errorf("Expected pod state %s, got %s", tt.expectedState, sbx.GetState())
				}
			}
		})
	}
}

func TestSandboxManager_ListClaimedPods(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client

	// 创建测试用的pods
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "running-pod-1",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxID:    "running-pod-1",
					consts.LabelSandboxState: consts.SandboxStateRunning,
					"custom-label":           "value1",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "running-pod-2",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxID:    "running-pod-2",
					consts.LabelSandboxState: consts.SandboxStateRunning,
					"custom-label":           "value2",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "paused-pod-1",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxID:    "paused-pod-1",
					consts.LabelSandboxState: consts.SandboxStatePaused,
					"custom-label":           "value1",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pending-pod",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxID:    "pending-pod",
					consts.LabelSandboxState: consts.SandboxStatePending,
					"custom-label":           "value1",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "internal-label-pod",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxID:       "internal-label-pod",
					consts.LabelSandboxState:    consts.SandboxStateRunning,
					consts.InternalPrefix + "x": "internal-value",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
	}

	// 添加pods到fake client
	for _, pod := range pods {
		_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod %s: %v", pod.Name, err)
		}
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	tests := []struct {
		name           string
		state          string
		selector       map[string]string
		expectedCount  int
		expectedStates []string
	}{
		{
			name:           "List all claimed pods",
			state:          "",
			selector:       map[string]string{},
			expectedCount:  4, // 3 running + 1 paused
			expectedStates: []string{consts.SandboxStateRunning, consts.SandboxStatePaused},
		},
		{
			name:           "List only running pods",
			state:          consts.SandboxStateRunning,
			selector:       map[string]string{},
			expectedCount:  3,
			expectedStates: []string{consts.SandboxStateRunning},
		},
		{
			name:           "List only paused pods",
			state:          consts.SandboxStatePaused,
			selector:       map[string]string{},
			expectedCount:  1,
			expectedStates: []string{consts.SandboxStatePaused},
		},
		{
			name:           "List pods with custom label",
			state:          "",
			selector:       map[string]string{"custom-label": "value1"},
			expectedCount:  2, // 1 running + 1 paused (pending is excluded)
			expectedStates: []string{consts.SandboxStateRunning, consts.SandboxStatePaused},
		},
		{
			name:           "List pods with custom label value2",
			state:          "",
			selector:       map[string]string{"custom-label": "value2"},
			expectedCount:  1, // 1 running
			expectedStates: []string{consts.SandboxStateRunning},
		},
		{
			name:           "List pods should ignore internal labels",
			state:          "",
			selector:       map[string]string{consts.InternalPrefix + "x": "internal-value"},
			expectedCount:  4, // 3 running + 1 paused, internal labels are ignored
			expectedStates: []string{consts.SandboxStateRunning, consts.SandboxStatePaused},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxes, err := manager.ListClaimedSandboxes(tt.state, tt.selector)

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
			}

			if len(sandboxes) != tt.expectedCount {
				t.Errorf("Expected %d sandboxes, got %d", tt.expectedCount, len(sandboxes))
			}

			// 验证状态
			stateCount := make(map[string]int)
			for _, sbx := range sandboxes {
				state := sbx.GetState()
				stateCount[state]++
			}

			// 验证返回的pods是否只包含预期的状态
			for _, expectedState := range tt.expectedStates {
				if stateCount[expectedState] == 0 {
					t.Errorf("Expected to find sandboxes with state %s, but none found", expectedState)
				}
			}

			// 验证没有返回不期望的状态
			for state := range stateCount {
				found := false
				for _, expectedState := range tt.expectedStates {
					if state == expectedState {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("Found sandboxes with unexpected state %s", state)
				}
			}
		})
	}
}

func TestSandboxManager_DeleteClaimedPod(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client

	// 创建测试用的pods
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "running-pod",
				consts.LabelSandboxState: consts.SandboxStateRunning,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	pendingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "pending-pod",
				consts.LabelSandboxState: consts.SandboxStatePending,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	// 添加pods到fake client
	pods := []*corev1.Pod{runningPod, pendingPod}
	for _, pod := range pods {
		_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod %s: %v", pod.Name, err)
		}
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	tests := []struct {
		name              string
		sandboxID         string
		expectError       bool
		expectedErrorCode errors.ErrorCode
	}{
		{
			name:              "Delete running pod",
			sandboxID:         "running-pod",
			expectError:       false,
			expectedErrorCode: "",
		},
		{
			name:              "Delete pending pod should return error",
			sandboxID:         "pending-pod",
			expectError:       true,
			expectedErrorCode: errors.ErrorNotFound,
		},
		{
			name:              "Delete non-existent pod should return error",
			sandboxID:         "non-existent-pod",
			expectError:       true,
			expectedErrorCode: errors.ErrorNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := manager.DeleteClaimedSandbox(context.Background(), tt.sandboxID)

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

				// 验证pod已被删除
				_, err := client.CoreV1().Pods("default").Get(context.Background(), tt.sandboxID, metav1.GetOptions{})
				if err == nil {
					t.Errorf("Expected pod to be deleted but it still exists")
				}
			}
		})
	}
}

func TestSandboxManager_SetSandboxTimeout(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client

	// 创建测试用的pods
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "running-pod",
				consts.LabelSandboxState: consts.SandboxStateRunning,
			},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{},
		},
	}

	pausedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "paused-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "paused-pod",
				consts.LabelSandboxState: consts.SandboxStatePaused,
			},
		},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{},
		},
	}

	// 添加pods到fake client
	pods := []*corev1.Pod{runningPod, pausedPod}
	for _, pod := range pods {
		_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod %s: %v", pod.Name, err)
		}
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	tests := []struct {
		name              string
		pod               *corev1.Pod
		seconds           int
		expectError       bool
		expectedErrorCode errors.ErrorCode
	}{
		{
			name:              "Set timeout for running pod",
			pod:               runningPod,
			seconds:           10,
			expectError:       false,
			expectedErrorCode: "",
		},
		{
			name:              "Set timeout for paused pod should return error",
			pod:               pausedPod,
			seconds:           10,
			expectError:       true,
			expectedErrorCode: errors.ErrorConflict,
		},
		{
			name:              "Set timeout with invalid seconds",
			pod:               runningPod,
			seconds:           -1,
			expectError:       true, // Should fail in SetTimer
			expectedErrorCode: errors.ErrorBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := manager.infra.(*k8s.Infra).AsSandbox(tt.pod)
			err := manager.SetSandboxTimeout(context.Background(), sbx, tt.seconds)

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

				// 验证定时器条件是否设置
				updatedPod, err := manager.client.CoreV1().Pods("default").Get(context.Background(), tt.pod.Name, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("Failed to get updated pod: %v", err)
				}

				hasTimerCondition := false
				for _, condition := range updatedPod.Status.Conditions {
					if condition.Type == corev1.PodConditionType("SandboxTimer."+string(consts.SandboxKill)) {
						hasTimerCondition = true
						break
					}
				}

				if !hasTimerCondition {
					t.Errorf("Expected timer condition to be set")
				}
			}
		})
	}
}

func TestSandboxManager_Debug(t *testing.T) {
	manager := setupTestManager(t)
	manager.GetDebugInfo()
}
