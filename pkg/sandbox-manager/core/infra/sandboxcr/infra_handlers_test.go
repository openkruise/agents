package sandboxcr

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func testHandleFuncPreparation(t *testing.T, poolExists bool, client sandboxclient.Interface,
	eventer *events.Eventer) *Infra {
	// 创建测试用的模板
	template := &infra.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
		},
		Spec: infra.SandboxTemplateSpec{
			MinPoolSize: 2,
			MaxPoolSize: 5,
			ExpectUsage: ptr.To(intstr.Parse("50%")),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.LabelSandboxPool: "test-pool",
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
	// hack: create pending sandboxes directly
	template.Spec.Template.Labels[consts.LabelSandboxState] = consts.SandboxStatePending

	// 创建 Infra 实例
	infraInstance, err := NewInfra("default", ".", eventer, client)
	assert.NoError(t, err)

	// 如果需要，添加池
	if poolExists {
		pool := &Pool{
			template:       template,
			client:         client,
			cache:          infraInstance.Cache,
			eventer:        eventer,
			reconcileQueue: make(chan context.Context, 1),
		}
		infraInstance.AddPool("test-pool", pool)
	}
	err = infraInstance.Run(context.Background())
	assert.NoError(t, err)

	// 如果池存在，等待初始化完成
	if pool, ok := infraInstance.GetPoolByTemplate("test-pool"); ok && poolExists {
		time.Sleep(time.Second * 2)
		var total int32
		for i := 0; i < 5; i++ {
			total = pool.(*Pool).Status.total.Load()
			if total == 2 {
				break
			}
			time.Sleep(time.Millisecond * 200)
		}
		assert.Equal(t, 2, int(total))
	}
	return infraInstance
}

//goland:noinspection GoDeprecation
func TestInfra_HandleSandboxAdd(t *testing.T) {
	utils.InitKLogOutput()
	tests := []struct {
		name       string
		poolExists bool
		expectPods int
	}{
		{
			name:       "pod with existing pool triggers event",
			poolExists: true,
			expectPods: 2, // should be deleted by reconcile
		},
		{
			name:       "pod without pool does not trigger event",
			poolExists: false,
			expectPods: 1, // no pool and no reconcile, not deleted
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建 fake client
			client := fake.NewSimpleClientset()

			// 创建 eventer
			eventer := events.NewEventer()
			testHandleFuncPreparation(t, tt.poolExists, client, eventer)

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					UID:       "test-uid-123",
					Labels: map[string]string{
						consts.LabelSandboxPool: "test-pool",
					},
				},
			}

			sbx := ConvertPodToSandboxCR(pod)
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
			assert.NoError(t, err)

			sandboxes := 0
			for i := 0; i < 5; i++ {
				// 等待处理完成
				list, err := client.ApiV1alpha1().Sandboxes("default").List(context.Background(), metav1.ListOptions{})
				assert.NoError(t, err)
				sandboxes = len(list.Items)
				if sandboxes == 2 {
					break
				}
				time.Sleep(time.Millisecond * 200)
			}
			assert.Equal(t, tt.expectPods, sandboxes)
		})
	}
}

//goland:noinspection GoDeprecation
func TestInfra_HandleSandboxUpdate(t *testing.T) {
	utils.InitKLogOutput()
	tests := []struct {
		name            string
		poolExists      bool
		expectTriggered bool
	}{
		{
			name:            "pod update with existing pool calls pool update",
			poolExists:      true,
			expectTriggered: true,
		},
		{
			name:            "pod update without pool does nothing",
			poolExists:      false,
			expectTriggered: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建 fake client
			client := fake.NewSimpleClientset()

			eventer := events.NewEventer()
			infraInstance := testHandleFuncPreparation(t, tt.poolExists, client, eventer)
			var hash string
			if tt.poolExists {
				pool, ok := infraInstance.GetPoolByTemplate("test-pool")
				assert.True(t, ok)
				hash = pool.GetTemplate().Labels[consts.LabelTemplateHash]
			}

			oldSbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelTemplateHash: hash,
						consts.LabelSandboxPool:  "test-pool",
					},
				},
				Status: v1alpha1.SandboxStatus{
					Conditions: []metav1.Condition{
						{
							Type:   string(v1alpha1.SandboxConditionReady),
							Status: metav1.ConditionFalse,
						},
					},
				},
			}
			newSbx := oldSbx.DeepCopy()
			newSbx.Status.Conditions = []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			}
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), oldSbx, metav1.CreateOptions{})
			assert.NoError(t, err)

			var triggered atomic.Bool

			eventer.RegisterHandler(consts.SandboxCreated, &events.Handler{
				Name: "test-handler",
				HandleFunc: func(evt events.Event) error {
					triggered.Store(true)
					return nil
				},
			})
			// 触发 handlePodUpdate
			_, err = client.ApiV1alpha1().Sandboxes("default").UpdateStatus(context.Background(), newSbx, metav1.UpdateOptions{})
			assert.NoError(t, err)

			var ok bool
			for i := 0; i < 5; i++ {
				// 等待处理完成
				time.Sleep(100 * time.Millisecond)
				ok = triggered.Load() == tt.expectTriggered
				if ok {
					break
				}
			}
			assert.True(t, ok, "Expected triggering event check failed")
		})
	}
}

//goland:noinspection GoDeprecation
func TestInfra_HandleSandboxDelete(t *testing.T) {
	utils.InitKLogOutput()
	tests := []struct {
		name        string
		poolExists  bool
		expectEvent bool
	}{
		{
			name:        "pod delete with existing pool",
			poolExists:  true,
			expectEvent: true,
		},
		{
			name:        "pod delete without pool",
			poolExists:  false,
			expectEvent: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建 fake client
			client := fake.NewSimpleClientset()

			// 创建 eventer
			eventer := events.NewEventer()

			infraInstance := testHandleFuncPreparation(t, tt.poolExists, client, eventer)
			var hash string
			if tt.poolExists {
				pool, ok := infraInstance.GetPoolByTemplate("test-pool")
				assert.True(t, ok)
				hash = pool.GetTemplate().Labels[consts.LabelTemplateHash]
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					UID:       "test-uid-123",
					Labels: map[string]string{
						consts.LabelSandboxPool:  "test-pool",
						consts.LabelTemplateHash: hash,
						consts.LabelSandboxState: consts.SandboxStateRunning,
					},
				},
			}
			sbx := ConvertPodToSandboxCR(pod)
			_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
			assert.NoError(t, err)

			// 创建通道来捕获事件
			deleted := make(chan struct{}, 1)
			eventer.RegisterHandler(consts.SandboxKill, &events.Handler{
				Name: "test-delete-handler",
				HandleFunc: func(evt events.Event) error {
					deleted <- struct{}{}
					return nil
				},
			})

			// 调用 handlePodDelete
			err = client.ApiV1alpha1().Sandboxes("default").Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
			assert.NoError(t, err)

			// 等待事件处理完成
			time.Sleep(100 * time.Millisecond)

			// 验证事件是否按预期触发
			if tt.expectEvent {
				var ok bool
				select {
				case <-deleted:
					ok = true
				case <-time.After(1 * time.Second):
				}
				assert.True(t, ok, "Expected event check failed")
			}
		})
	}
}
