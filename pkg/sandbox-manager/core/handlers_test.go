package core

import (
	"context"
	"testing"
	"time"

	consts2 "github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	k8s2 "github.com/openkruise/agents/pkg/sandbox-manager/core/infra/k8s"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/proxy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Handle(manager *SandboxManager, pod *corev1.Pod, handler func(events.Event) error) error {
	// 创建事件
	evt := events.Event{
		Type:    consts2.SandboxCreated,
		Sandbox: manager.infra.(*k8s2.Infra).AsSandbox(pod),
		Context: context.Background(),
	}

	// 调用处理函数
	return handler(evt)
}

func TestHandlePodReady(t *testing.T) {
	tests := []struct {
		name          string
		pod           *corev1.Pod
		expectError   bool
		checkRoute    bool
		expectedRoute proxy.Route
	}{
		{
			name: "Pod with pool label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-1",
					Namespace: "default",
					Labels: map[string]string{
						consts2.LabelSandboxPool:  "test-template",
						consts2.LabelSandboxState: consts2.SandboxStateRunning,
					},
				},
				Status: corev1.PodStatus{
					PodIP: "10.0.0.1",
				},
			},
			expectError: false,
			checkRoute:  true,
			expectedRoute: proxy.Route{
				ID: "test-pod-1",
				IP: "10.0.0.1",
			},
		},
		{
			name: "Pod without pool label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-2",
					Namespace: "default",
					Labels: map[string]string{
						consts2.LabelSandboxState: consts2.SandboxStateRunning,
					},
				},
				Status: corev1.PodStatus{
					PodIP: "10.0.0.2",
				},
			},
			expectError: true,
			checkRoute:  false,
		},
		{
			name: "Pod with empty state",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-3",
					Namespace: "default",
					Labels: map[string]string{
						consts2.LabelSandboxPool: "test-template",
					},
				},
				Status: corev1.PodStatus{
					PodIP: "10.0.0.3",
				},
			},
			expectError: false,
			checkRoute:  true,
			expectedRoute: proxy.Route{
				ID: "test-pod-3",
				IP: "10.0.0.3",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := setupTestManager(t)

			// 创建测试池
			if _, exists := tt.pod.Labels[consts2.LabelSandboxPool]; exists {
				// 直接访问pools字段
				manager.infra.AddPool("test-template", &k8s2.Pool{})
			}

			// 添加pod到fake client
			_, err := manager.client.CoreV1().Pods("default").Create(context.Background(), tt.pod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Failed to create test pod: %v", err)
			}

			// 等待informer同步
			time.Sleep(100 * time.Millisecond)

			err = Handle(manager, tt.pod, manager.handleSandboxCreated)

			// 检查错误
			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
				return
			}

			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			// 检查路由是否正确设置
			if tt.checkRoute {
				route, ok := manager.proxy.LoadRoute(tt.expectedRoute.ID)
				if !ok {
					t.Error("Expected route not found")
					return
				}

				if route.IP != tt.expectedRoute.IP {
					t.Errorf("Expected route IP %s, got %s", tt.expectedRoute.IP, route.IP)
				}

				if route.ID != tt.expectedRoute.ID {
					t.Errorf("Expected route ID %s, got %s", tt.expectedRoute.ID, route.ID)
				}
			}

			// 检查pod标签是否正确更新
			updatedPod, err := manager.client.CoreV1().Pods("default").Get(context.Background(), tt.pod.Name, metav1.GetOptions{})
			if err != nil {
				t.Fatalf("Failed to get updated pod: %v", err)
			}

			sandboxID := updatedPod.Labels[consts2.LabelSandboxID]
			if sandboxID != tt.pod.Name {
				t.Errorf("Expected sandbox ID label %s, got %s", tt.pod.Name, sandboxID)
			}

			sandboxState := updatedPod.Labels[consts2.LabelSandboxState]
			expectedState := tt.pod.Labels[consts2.LabelSandboxState]
			if expectedState == "" {
				expectedState = consts2.SandboxStatePending
			}
			if sandboxState != expectedState {
				t.Errorf("Expected sandbox state %s, got %s", expectedState, sandboxState)
			}
		})
	}
}

func TestHandleSandboxKill(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		expectError bool
	}{
		{
			name: "Valid pod",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod-1",
					Namespace: "default",
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := setupTestManager(t)
			finalizer := "sandbox-finalizer"
			tt.pod.Finalizers = append(tt.pod.Finalizers, finalizer)

			// 添加pod到fake client
			_, err := manager.client.CoreV1().Pods("default").Create(context.Background(), tt.pod, metav1.CreateOptions{})
			if err != nil {
				t.Fatalf("Failed to create test pod: %v", err)
			}

			// 设置路由
			manager.proxy.SetRoute(tt.pod.Name, proxy.Route{
				ID: tt.pod.Name,
				IP: "10.0.0.1",
			})

			// 等待informer同步
			time.Sleep(100 * time.Millisecond)

			err = Handle(manager, tt.pod, manager.handleSandboxKill)

			// 检查错误
			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
				return
			}

			// 对于这个处理函数，我们检查没有错误发生
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}

			// 检查路由是否被删除
			_, ok := manager.proxy.LoadRoute(tt.pod.Name)
			if ok {
				t.Error("Expected route to be deleted")
			}

		})
	}
}
