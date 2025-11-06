package microvm

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/sandboxcr"
	"github.com/stretchr/testify/assert"
	"gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/api/v1alpha1"
	"gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/client/clientset/versioned/fake"
	informers "gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/client/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

//goland:noinspection GoDeprecation
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
			client := fake.NewSimpleClientset()

			// 创建 Pod
			for _, pod := range tt.pods {
				sbx := ConvertPodToSandboxCR(pod)
				_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
				assert.NoError(t, err)
			}

			// 创建 cache
			informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			cache, err := sandboxcr.NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
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

//goland:noinspection GoDeprecation
func TestInfra_GetSandbox(t *testing.T) {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxID:    "test-pod",
				consts.LabelSandboxPool:  "test-pool",
				consts.LabelSandboxState: consts.SandboxStateRunning,
			},
		},
	}
	eventer := events.NewEventer()
	client := fake.NewSimpleClientset()
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
	assert.NoError(t, err)
	infraInstance, err := NewInfra("default", ".", eventer, client)
	assert.NoError(t, err)
	err = infraInstance.Run(context.Background())
	assert.NoError(t, err)
	sandbox, err := infraInstance.GetSandbox("test-pod")
	assert.NoError(t, err)
	_, ok := sandbox.(*Sandbox)
	assert.True(t, ok)
	sandboxes, err := infraInstance.SelectSandboxes(infra.SandboxSelectorOptions{WantRunning: true})
	assert.NoError(t, err)
	assert.Equal(t, 1, len(sandboxes))
	_, ok = sandboxes[0].(*Sandbox)
	assert.True(t, ok)
	noSandboxes, err := infraInstance.SelectSandboxes(infra.SandboxSelectorOptions{WantPaused: true})
	assert.NoError(t, err)
	assert.Equal(t, 0, len(noSandboxes))
}
