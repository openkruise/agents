package sandbox_manager

import (
	"context"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

func ConvertPodToSandboxCR(pod *corev1.Pod) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: pod.ObjectMeta,
		Spec: agentsv1alpha1.SandboxSpec{
			Template: corev1.PodTemplateSpec{
				Spec: pod.Spec,
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPhase(pod.Status.Phase),
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: pod.Status.PodIP,
			},
		},
	}
}

func setupTestManager(t *testing.T) *SandboxManager {
	// Create fake client set
	client := clients.NewFakeClientSet()
	manager, err := NewSandboxManager("default", client, nil, consts.InfraSandboxCR)
	if err != nil {
		t.Fatalf("Failed to create manager: %v", err)
	}

	err = manager.infra.Run(context.Background())
	if err != nil {
		t.Fatalf("Failed to run infra: %v", err)
	}

	return manager
}

func CreateSandboxWithStatus(t *testing.T, client versioned.Interface, sbx *agentsv1alpha1.Sandbox) {
	ctx := context.Background()
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(ctx, sbx, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = client.ApiV1alpha1().Sandboxes("default").UpdateStatus(ctx, sbx, metav1.UpdateOptions{})
	assert.NoError(t, err)
}

func TestSandboxManager_ClaimSandbox(t *testing.T) {
	utils.InitLogOutput()
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
			template: "exist-1",
			timeout:  1234,
		},
		{
			name:              "Claim failed",
			template:          "exist-2", // pool has no pending sandboxes
			timeout:           1234,
			expectError:       true,
			expectedErrorCode: errors.ErrorInternal,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := setupTestManager(t)
			pool1 := manager.GetInfra().NewPool("exist-1", "default")
			pool2 := manager.GetInfra().NewPool("exist-2", "default")
			manager.GetInfra().AddPool("exist-1", pool1)
			manager.GetInfra().AddPool("exist-2", pool2)

			client := manager.client.SandboxClient

			// Create test pod
			testSbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxID:    "test-sandbox",
						agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
						agentsv1alpha1.LabelSandboxPool:  "exist-1",
					},
					Annotations: map[string]string{},
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

			CreateSandboxWithStatus(t, client, testSbx)

			// Wait for informer sync
			time.Sleep(100 * time.Millisecond)
			err := retry.OnError(wait.Backoff{
				Steps:    10,
				Duration: 10 * time.Millisecond,
				Factor:   1.0,
			}, func(err error) bool {
				return true
			}, func() error {
				_, err := manager.GetInfra().GetSandbox(testSbx.Name)
				return err
			})
			assert.NoError(t, err)

			got, err := manager.ClaimSandbox(context.Background(), "test-user", tt.template, tt.timeout)

			if tt.expectError {
				assert.Error(t, err)
				assert.Equal(t, tt.expectedErrorCode, errors.GetErrCode(err))
			} else {
				assert.NoError(t, err)
				time.Sleep(100 * time.Millisecond)
				// check route
				route, ok := manager.proxy.LoadRoute(got.GetName())
				assert.True(t, ok)
				assert.Equal(t, testSbx.Name, route.ID)
				assert.Equal(t, testSbx.Status.PodInfo.PodIP, route.IP)
				assert.Equal(t, "test-user", route.Owner)
			}
		})
	}
}

func TestSandboxManager_GetClaimedPod(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client.SandboxClient

	// Create test pods
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxID:    "running-pod",
				agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateRunning,
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
				agentsv1alpha1.LabelSandboxID:    "paused-pod",
				agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStatePaused,
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
				agentsv1alpha1.LabelSandboxID:    "pending-pod",
				agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	// Add pods to fake client
	pods := []*corev1.Pod{runningPod, pausedPod, pendingPod}
	for _, pod := range pods {
		_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), ConvertPodToSandboxCR(pod), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod %s: %v", pod.Name, err)
		}
	}

	// Wait for informer sync
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
			expectedState:     agentsv1alpha1.SandboxStateRunning,
		},
		{
			name:              "Get paused pod",
			sandboxID:         "paused-pod",
			expectError:       false,
			expectedErrorCode: "",
			expectedState:     agentsv1alpha1.SandboxStatePaused,
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
	client := manager.client.SandboxClient

	// Create test pods
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "running-pod-1",
				Namespace: "default",
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxID:    "running-pod-1",
					agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateRunning,
					"custom-label":                   "value1",
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
					agentsv1alpha1.LabelSandboxID:    "running-pod-2",
					agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateRunning,
					"custom-label":                   "value2",
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
					agentsv1alpha1.LabelSandboxID:    "paused-pod-1",
					agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStatePaused,
					"custom-label":                   "value1",
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
					agentsv1alpha1.LabelSandboxID:    "pending-pod",
					agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
					"custom-label":                   "value1",
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
					agentsv1alpha1.LabelSandboxID:       "internal-label-pod",
					agentsv1alpha1.LabelSandboxState:    agentsv1alpha1.SandboxStateRunning,
					agentsv1alpha1.InternalPrefix + "x": "internal-value",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
	}

	// Add pods to fake client
	for _, pod := range pods {
		_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), ConvertPodToSandboxCR(pod), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod %s: %v", pod.Name, err)
		}
	}

	// Wait for informer sync
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
			expectedStates: []string{agentsv1alpha1.SandboxStateRunning, agentsv1alpha1.SandboxStatePaused},
		},
		{
			name:           "List only running pods",
			state:          agentsv1alpha1.SandboxStateRunning,
			selector:       map[string]string{},
			expectedCount:  3,
			expectedStates: []string{agentsv1alpha1.SandboxStateRunning},
		},
		{
			name:           "List only paused pods",
			state:          agentsv1alpha1.SandboxStatePaused,
			selector:       map[string]string{},
			expectedCount:  1,
			expectedStates: []string{agentsv1alpha1.SandboxStatePaused},
		},
		{
			name:           "List pods with custom label",
			state:          "",
			selector:       map[string]string{"custom-label": "value1"},
			expectedCount:  2, // 1 running + 1 paused (pending is excluded)
			expectedStates: []string{agentsv1alpha1.SandboxStateRunning, agentsv1alpha1.SandboxStatePaused},
		},
		{
			name:           "List pods with custom label value2",
			state:          "",
			selector:       map[string]string{"custom-label": "value2"},
			expectedCount:  1, // 1 running
			expectedStates: []string{agentsv1alpha1.SandboxStateRunning},
		},
		{
			name:           "List pods should ignore internal labels",
			state:          "",
			selector:       map[string]string{agentsv1alpha1.InternalPrefix + "x": "internal-value"},
			expectedCount:  4, // 3 running + 1 paused, internal labels are ignored
			expectedStates: []string{agentsv1alpha1.SandboxStateRunning, agentsv1alpha1.SandboxStatePaused},
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

			// Verify status
			stateCount := make(map[string]int)
			for _, sbx := range sandboxes {
				state := sbx.GetState()
				stateCount[state]++
			}

			// Verify that returned pods only contain expected states
			for _, expectedState := range tt.expectedStates {
				if stateCount[expectedState] == 0 {
					t.Errorf("Expected to find sandboxes with state %s, but none found", expectedState)
				}
			}

			// Verify that no unexpected states are returned
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
	client := manager.client.SandboxClient

	// Create test pods
	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxID:    "running-pod",
				agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateRunning,
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
				agentsv1alpha1.LabelSandboxID:    "pending-pod",
				agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateAvailable,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	// Add pods to fake client
	pods := []*corev1.Pod{runningPod, pendingPod}
	for _, pod := range pods {
		_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), ConvertPodToSandboxCR(pod), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod %s: %v", pod.Name, err)
		}
	}

	// Wait for informer sync
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

				// Verify pod has been deleted
				_, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), tt.sandboxID, metav1.GetOptions{})
				if err == nil {
					t.Errorf("Expected pod to be deleted but it still exists")
				}
			}
		})
	}
}

func TestSandboxManager_Debug(t *testing.T) {
	manager := setupTestManager(t)
	manager.GetDebugInfo()
}
