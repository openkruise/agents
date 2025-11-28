package sandbox_manager

import (
	"context"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/proxy"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Handle(manager *SandboxManager, sbx *agentsv1alpha1.Sandbox, handler func(events.Event) error) error {
	// 创建事件
	evt := events.Event{
		Type:    consts.SandboxCreated,
		Sandbox: manager.infra.(*sandboxcr.Infra).AsSandbox(sbx),
		Context: context.Background(),
	}

	// 调用处理函数
	return handler(evt)
}

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

			sbx := ConvertPodToSandboxCR(tt.pod)
			_, err := manager.client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
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

			err = Handle(manager, sbx, manager.handleSandboxKill)

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
