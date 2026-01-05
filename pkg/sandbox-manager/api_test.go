package sandbox_manager

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils2 "github.com/openkruise/agents/pkg/utils"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
)

var testUser = "test-user"

func ConvertPodToSandboxCR(pod *corev1.Pod) *agentsv1alpha1.Sandbox {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: pod.ObjectMeta,
		Spec: agentsv1alpha1.SandboxSpec{
			SandboxTemplate: agentsv1alpha1.SandboxTemplate{
				Template: &corev1.PodTemplateSpec{
					Spec: pod.Spec,
				},
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxPhase(pod.Status.Phase),
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: pod.Status.PodIP,
			},
		},
	}
	cond := utils2.GetPodCondition(&pod.Status, corev1.PodReady)
	if cond != nil {
		sbx.Status.Conditions = append(sbx.Status.Conditions, metav1.Condition{
			Type:   string(agentsv1alpha1.SandboxConditionReady),
			Status: metav1.ConditionStatus(cond.Status),
		})
	}
	if strings.HasPrefix(pod.Name, "paused") {
		sbx.Spec.Paused = true
	}
	return sbx
}

func GetSbsOwnerReference() []metav1.OwnerReference {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sandboxset",
			UID:  "12345",
		},
	}
	return []metav1.OwnerReference{*metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind)}
}

func setupTestManager(t *testing.T) *SandboxManager {
	// 创建fake client set
	client := clients.NewFakeClientSet()
	manager, err := NewSandboxManager(client, nil, consts.InfraSandboxCR)
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
			pool1 := manager.GetInfra().NewPool("exist-1", "default", nil)
			pool2 := manager.GetInfra().NewPool("exist-2", "default", nil)
			manager.GetInfra().AddPool("exist-1", pool1)
			manager.GetInfra().AddPool("exist-2", pool2)

			client := manager.client.SandboxClient

			testSbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						agentsv1alpha1.LabelSandboxPool: "exist-1",
					},
					Annotations: map[string]string{},
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

			// 等待informer同步
			time.Sleep(100 * time.Millisecond)
			err := retry.OnError(wait.Backoff{
				Steps:    10,
				Duration: 10 * time.Millisecond,
				Factor:   1.0,
			}, func(err error) bool {
				return true
			}, func() error {
				sbx, err := manager.GetInfra().GetSandbox(context.Background(), sandboxutils.GetSandboxID(testSbx))
				if err != nil {
					return err
				}
				if state, _ := sbx.GetState(); state != agentsv1alpha1.SandboxStateAvailable {
					return fmt.Errorf("sandbox %s state %s is not available", sbx.GetName(), state)
				}
				return nil
			})
			assert.NoError(t, err)

			got, err := manager.ClaimSandbox(context.Background(), "test-user", tt.template, func(sbx infra.Sandbox) {
				sbx.SetTimeout(time.Duration(tt.timeout) * time.Second)
			})

			if tt.expectError {
				assert.Error(t, err)
				assert.Equal(t, tt.expectedErrorCode, errors.GetErrCode(err))
			} else {
				assert.NoError(t, err)
				time.Sleep(100 * time.Millisecond)
				// check route
				route, ok := manager.proxy.LoadRoute(got.GetSandboxID())
				assert.True(t, ok)
				assert.Equal(t, got.GetSandboxID(), route.ID)
				assert.Equal(t, testSbx.Status.PodInfo.PodIP, route.IP)
				assert.Equal(t, "test-user", route.Owner)
			}
		})
	}
}

func TestSandboxManager_GetClaimedSandbox(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client.SandboxClient

	runningPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "running-pod",
			Namespace: "default",
			Labels:    map[string]string{},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner: testUser,
			},
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

	pausedPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "paused-pod",
			Namespace: "default",
			Labels:    map[string]string{},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner: testUser,
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
		},
	}

	availablePod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "available-pod",
			Namespace:       "default",
			Labels:          map[string]string{},
			OwnerReferences: GetSbsOwnerReference(),
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

	pods := []*corev1.Pod{runningPod, pausedPod, availablePod}
	for _, pod := range pods {
		_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), ConvertPodToSandboxCR(pod), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod %s: %v", pod.Name, err)
		}
	}

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
			name:              "Get non-existent pod should return error",
			sandboxID:         "default--non-existent-pod",
			expectError:       true,
			expectedErrorCode: errors.ErrorNotFound,
			expectedState:     "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx, err := manager.GetClaimedSandbox(context.Background(), testUser, tt.sandboxID)

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
	manager := setupTestManager(t)
	manager.GetDebugInfo()
}
