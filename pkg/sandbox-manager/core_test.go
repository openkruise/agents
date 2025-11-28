//goland:noinspection GoDeprecation
package sandbox_manager

import (
	"context"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	proxy2 "github.com/openkruise/agents/pkg/sandbox-manager/proxy"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type QuickSandbox struct {
	ID    string
	IP    string
	Owner string
}

//goland:noinspection GoDeprecation
func TestSandboxManager_RefreshProxy(t *testing.T) {
	tests := []struct {
		name           string
		initialRoutes  []proxy2.Route
		fakeSandboxes  map[string]QuickSandbox
		expectedRoutes []proxy2.Route
	}{
		{
			name: "更新路由当IP改变时",
			initialRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.1", Owner: "user1"},
			},
			fakeSandboxes: map[string]QuickSandbox{
				"sandbox1": {ID: "sandbox1", IP: "10.0.0.2", Owner: "user1"},
			},
			expectedRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.2", Owner: "user1"},
			},
		},
		{
			name: "更新路由当Owner改变时",
			initialRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.1", Owner: "user1"},
			},
			fakeSandboxes: map[string]QuickSandbox{
				"sandbox1": {ID: "sandbox1", IP: "10.0.0.1", Owner: "user2"},
			},
			expectedRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.1", Owner: "user2"},
			},
		},
		{
			name: "删除不存在的沙箱路由",
			initialRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.1", Owner: "user1"},
				{ID: "sandbox2", IP: "10.0.0.2", Owner: "user2"},
			},
			fakeSandboxes: map[string]QuickSandbox{
				"sandbox1": {ID: "sandbox1", IP: "10.0.0.1", Owner: "user1"},
				// sandbox2 不存在
			},
			expectedRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.1", Owner: "user1"},
			},
		},
		{
			name: "同时更新和删除多个路由",
			initialRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.1", Owner: "user1"},
				{ID: "sandbox2", IP: "10.0.0.2", Owner: "user2"},
				{ID: "sandbox3", IP: "10.0.0.3", Owner: "user3"},
			},
			fakeSandboxes: map[string]QuickSandbox{
				"sandbox1": {ID: "sandbox1", IP: "10.0.0.10", Owner: "user1"}, // IP changed
				// sandbox2 不存在，应该被删除
				"sandbox3": {ID: "sandbox3", IP: "10.0.0.3", Owner: "user4"}, // Owner changed
			},
			expectedRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.10", Owner: "user1"},
				{ID: "sandbox3", IP: "10.0.0.3", Owner: "user4"},
			},
		},
		{
			name:          "没有路由时",
			initialRoutes: []proxy2.Route{},
			fakeSandboxes: map[string]QuickSandbox{
				"sandbox1": {ID: "sandbox1", IP: "10.0.0.1", Owner: "user1"},
			},
			expectedRoutes: []proxy2.Route{},
		},
		{
			name: "所有沙箱都不存在",
			initialRoutes: []proxy2.Route{
				{ID: "sandbox1", IP: "10.0.0.1", Owner: "user1"},
				{ID: "sandbox2", IP: "10.0.0.2", Owner: "user2"},
			},
			fakeSandboxes:  map[string]QuickSandbox{},
			expectedRoutes: []proxy2.Route{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建假的基础设施
			eventer := events.NewEventer()
			client := fake.NewSimpleClientset()
			infraInstance, err := sandboxcr.NewInfra("default", eventer, client)
			assert.NoError(t, err)
			assert.NoError(t, infraInstance.Run(context.Background()))
			for _, sandbox := range tt.fakeSandboxes {
				pod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: sandbox.ID,
						Labels: map[string]string{
							v1alpha1.LabelSandboxID:    sandbox.ID,
							v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
							v1alpha1.LabelSandboxPool:  "test-pool",
						},
						Annotations: map[string]string{
							v1alpha1.AnnotationOwner: sandbox.Owner,
						},
					},
					Status: corev1.PodStatus{
						PodIP: sandbox.IP,
					},
				}
				_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), ConvertPodToSandboxCR(pod), metav1.CreateOptions{})
				assert.NoError(t, err)
			}

			var createNum int
			for i := 0; i < 10; i++ {
				created, err := infraInstance.SelectSandboxes(infra.SandboxSelectorOptions{WantRunning: true})
				assert.NoError(t, err)
				createNum = len(created)
				if createNum == len(tt.fakeSandboxes) {
					break
				}
				time.Sleep(time.Millisecond * 50)
			}
			assert.Equal(t, len(tt.fakeSandboxes), createNum)

			// 创建代理服务器
			proxyServer := proxy2.NewServer(nil)

			// 设置初始路由
			for _, route := range tt.initialRoutes {
				proxyServer.SetRoute(route.ID, route)
			}

			// 创建沙箱管理器
			manager := &SandboxManager{
				infra: infraInstance,
				proxy: proxyServer,
			}

			// 执行测试
			manager.RefreshProxy(context.Background())

			// 验证结果
			actualRoutes := proxyServer.ListRoutes()

			// 检查路由数量
			if len(actualRoutes) != len(tt.expectedRoutes) {
				t.Errorf("期望路由数量 %d, 实际 %d", len(tt.expectedRoutes), len(actualRoutes))
			}

			// 检查每个期望的路由是否存在
			for _, expectedRoute := range tt.expectedRoutes {
				found := false
				for _, actualRoute := range actualRoutes {
					if actualRoute.ID == expectedRoute.ID &&
						actualRoute.IP == expectedRoute.IP &&
						actualRoute.Owner == expectedRoute.Owner {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("未找到期望的路由: %+v", expectedRoute)
				}
			}

			// 检查是否有额外的路由
			for _, actualRoute := range actualRoutes {
				found := false
				for _, expectedRoute := range tt.expectedRoutes {
					if actualRoute.ID == expectedRoute.ID &&
						actualRoute.IP == expectedRoute.IP &&
						actualRoute.Owner == expectedRoute.Owner {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("发现意外的路由: %+v", actualRoute)
				}
			}
		})
	}
}
