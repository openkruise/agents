package k8s

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestInfra_SelectSandboxes(t *testing.T) {
	tests := []struct {
		name           string
		pods           []*corev1.Pod
		options        infra.SandboxSelectorOptions
		expectedCount  int
		expectedStates []string
	}{
		{
			name: "select running sandboxes",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStateRunning,
							consts.LabelSandboxPool:  "test-template",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStatePaused,
							consts.LabelSandboxPool:  "test-template",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning: true,
				WantPaused:  false,
				WantPending: false,
			},
			expectedCount:  1,
			expectedStates: []string{consts.SandboxStateRunning},
		},
		{
			name: "select paused sandboxes",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStateRunning,
							consts.LabelSandboxPool:  "test-template",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStatePaused,
							consts.LabelSandboxPool:  "test-template",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning: false,
				WantPaused:  true,
				WantPending: false,
			},
			expectedCount:  1,
			expectedStates: []string{consts.SandboxStatePaused},
		},
		{
			name: "select multiple states",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStateRunning,
							consts.LabelSandboxPool:  "test-template",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStatePaused,
							consts.LabelSandboxPool:  "test-template",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod3",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStatePending,
							consts.LabelSandboxPool:  "test-template",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning: true,
				WantPaused:  true,
				WantPending: false,
			},
			expectedCount:  2,
			expectedStates: []string{consts.SandboxStateRunning, consts.SandboxStatePaused},
		},
		{
			name: "select with labels",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStateRunning,
							consts.LabelSandboxPool:  "test-template",
							"custom-label":           "value1",
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStateRunning,
							consts.LabelSandboxPool:  "test-template",
							"custom-label":           "value2",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning: true,
				WantPaused:  false,
				WantPending: false,
				Labels: map[string]string{
					"custom-label": "value1",
				},
			},
			expectedCount:  1,
			expectedStates: []string{consts.SandboxStateRunning},
		},
		{
			name: "no matching sandboxes",
			pods: []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxState: consts.SandboxStateRunning,
							consts.LabelSandboxPool:  "test-template",
						},
					},
				},
			},
			options: infra.SandboxSelectorOptions{
				WantRunning: false,
				WantPaused:  true,
				WantPending: false,
			},
			expectedCount:  0,
			expectedStates: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建 fake client
			client := fake.NewClientset()

			// 创建 Pod
			for _, pod := range tt.pods {
				_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
				assert.NoError(t, err)
			}

			// 创建 cache
			cache, err := NewCache(client, "default")
			assert.NoError(t, err)

			// 启动缓存并等待同步
			done := make(chan struct{})
			go cache.Run(done)
			select {
			case <-done:
				// 缓存已同步
			case <-time.After(1 * time.Second):
				// 超时
				t.Fatal("Cache sync timeout")
			}

			// 创建 Infra 实例
			infraInstance := &Infra{
				BaseInfra: infra.BaseInfra{
					Namespace: "default",
				},
				Cache:  cache,
				Client: client,
			}

			// 调用 SelectSandboxes 方法
			sandboxes, err := infraInstance.SelectSandboxes(tt.options)
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedCount, len(sandboxes))

			// 验证返回的沙箱状态
			actualStates := make([]string, len(sandboxes))
			for i, sandbox := range sandboxes {
				actualStates[i] = sandbox.GetState()
			}

			// 排序以确保比较的一致性
			sort.Strings(actualStates)
			sort.Strings(tt.expectedStates)
			assert.Equal(t, tt.expectedStates, actualStates)

			// 停止缓存
			cache.Stop()
		})
	}
}

func TestInfra_syncWithCluster(t *testing.T) {
	// 创建测试用的模板
	template := &infra.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: infra.SandboxTemplateSpec{
			MinPoolSize: 1,
			MaxPoolSize: 5,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.LabelSandboxPool: "test-template",
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "test-container",
							Image: "nginx:latest",
						},
					},
				},
			},
		},
	}
	template.Init("default")

	tests := []struct {
		name        string
		pools       []*Pool
		expectError bool
	}{
		{
			name:        "sync with no pools",
			pools:       []*Pool{},
			expectError: false,
		},
		{
			name: "sync with one pool",
			pools: []*Pool{
				{
					template: template,
				},
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建 fake client
			client := fake.NewClientset()

			// 创建 cache
			cache, err := NewCache(client, "default")
			assert.NoError(t, err)

			// 启动缓存并等待同步
			done := make(chan struct{})
			go cache.Run(done)
			select {
			case <-done:
				// 缓存已同步
			case <-time.After(1 * time.Second):
				// 超时
				t.Fatal("Cache sync timeout")
			}

			// 创建 eventer
			eventer := events.NewEventer()

			// 创建 Infra 实例
			infraInstance := &Infra{
				BaseInfra: infra.BaseInfra{
					Namespace: "default",
				},
				Cache:   cache,
				Client:  client,
				Eventer: eventer,
			}

			// 添加池
			for _, pool := range tt.pools {
				pool.client = client
				pool.cache = cache
				pool.eventer = eventer
				infraInstance.AddPool(pool.template.Name, pool)
			}

			// 调用 syncWithCluster 方法
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			err = infraInstance.syncWithCluster(ctx)

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			// 停止缓存
			cache.Stop()
		})
	}
}
