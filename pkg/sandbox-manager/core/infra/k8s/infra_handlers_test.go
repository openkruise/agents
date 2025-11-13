package k8s

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func testHandleFuncPreparation(t *testing.T, poolExists bool, client kubernetes.Interface,
	eventer *events.Eventer) *Infra {
	// 创建测试用的模板
	template := &infra.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
		},
		Spec: infra.SandboxTemplateSpec{
			MinPoolSize: 1,
			MaxPoolSize: 5,
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

	// 创建 Infra 实例
	infraInstance, err := NewInfra("default", ".", eventer, client, nil)
	assert.NoError(t, err)
	err = infraInstance.Run(context.Background())
	assert.NoError(t, err)

	// 如果需要，添加池
	if poolExists {
		pool := &Pool{
			template: template,
			client:   client,
			cache:    infraInstance.Cache,
			eventer:  eventer,
		}
		infraInstance.AddPool("test-pool", pool)
	}

	// 如果池存在，获取池
	if pool, ok := infraInstance.GetPoolByTemplate("test-pool"); ok && poolExists {
		// 验证池类型正确
		_ = pool.(*Pool)
	}
	return infraInstance
}

func TestInfra_HandlePodUpdate(t *testing.T) {
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
			client := fake.NewClientset()

			oldPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels:    map[string]string{},
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionFalse,
						},
					},
				},
			}
			newPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels:    map[string]string{},
				},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			}
			if tt.poolExists {
				oldPod.Labels[consts.LabelSandboxPool] = "test-pool"
				newPod.Labels[consts.LabelSandboxPool] = "test-pool"
			}

			_, err := client.CoreV1().Pods("default").Create(context.Background(), oldPod, metav1.CreateOptions{})
			assert.NoError(t, err)

			eventer := events.NewEventer()
			testHandleFuncPreparation(t, tt.poolExists, client, eventer)

			var triggered atomic.Bool

			eventer.RegisterHandler(consts.SandboxCreated, &events.Handler{
				Name: "test-handler",
				HandleFunc: func(evt events.Event) error {
					triggered.Store(true)
					return nil
				},
			})

			// 触发 handlePodUpdate
			_, err = client.CoreV1().Pods("default").UpdateStatus(context.Background(), newPod, metav1.UpdateOptions{})
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

func TestInfra_HandlePodDelete(t *testing.T) {
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
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					UID:       "test-uid",
					Labels: map[string]string{
						consts.LabelSandboxPool: "test-pool",
					},
				},
			}
			// 创建 fake client
			client := fake.NewClientset(pod)

			// 创建 eventer
			eventer := events.NewEventer()

			testHandleFuncPreparation(t, tt.poolExists, client, eventer)

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
			err := client.CoreV1().Pods("default").Delete(context.Background(), pod.Name, metav1.DeleteOptions{})
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

func TestInfra_HandleDeploymentUpdate(t *testing.T) {
	tests := []struct {
		name               string
		isPool             bool
		expectStatusUpdate bool
	}{
		{
			name:               "deployment update with existing pool calls status update",
			isPool:             true,
			expectStatusUpdate: true,
		},
		{
			name:               "non-pool deployment does nothing",
			isPool:             false,
			expectStatusUpdate: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
				},
			}
			newDeployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pool",
					Namespace: "default",
				},
				Status: appsv1.DeploymentStatus{
					Replicas: 10,
				},
			}
			if !tt.isPool {
				deployment.Name = "not-a-pool"
				newDeployment.Name = "not-a-pool"
			}

			// 创建 fake client
			client := fake.NewClientset(deployment)

			// 创建 eventer
			eventer := events.NewEventer()

			infraInstance := testHandleFuncPreparation(t, true, client, eventer)

			// 调用 handleDeploymentUpdate
			_, err := client.AppsV1().Deployments("default").UpdateStatus(context.Background(), newDeployment, metav1.UpdateOptions{})
			assert.NoError(t, err)

			var expectReplicas int32
			if tt.expectStatusUpdate {
				expectReplicas = 10
			}
			var continueOK int
			for i := 0; i < 20; i++ {
				// 等待处理完成
				time.Sleep(100 * time.Millisecond)
				if pool, ok := infraInstance.GetPoolByTemplate("test-pool"); ok {
					if pool.(*Pool).total.Load() == expectReplicas {
						continueOK++
					} else {
						continueOK = 0
					}
				}
				if continueOK >= 3 {
					break
				}
			}
			assert.True(t, continueOK >= 3, "Expected status update check failed")
		})
	}
}

func TestInfra_HandleDeploymentDelete(t *testing.T) {
	tests := []struct {
		name   string
		isPool bool
	}{
		{
			name:   "deployment delete removes pool",
			isPool: true,
		},
		{
			name:   "deployment delete without pool does nothing",
			isPool: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "default",
				},
			}
			if tt.isPool {
				deployment.Name = "test-pool"
			} else {
				deployment.Name = "not-a-pool"
			}
			// 创建 fake client
			client := fake.NewClientset(deployment)

			// 创建 eventer
			eventer := events.NewEventer()

			infraInstance := testHandleFuncPreparation(t, true, client, eventer)

			// 调用 handleDeploymentDelete
			err := client.AppsV1().Deployments("default").Delete(context.Background(), deployment.Name, metav1.DeleteOptions{})
			assert.NoError(t, err)

			var success bool
			for i := 0; i < 5; i++ {
				// 等待处理完成
				time.Sleep(100 * time.Millisecond)
				// 验证池是否被删除
				_, ok := infraInstance.GetPoolByTemplate("test-pool")
				if tt.isPool {
					success = !ok
				} else {
					success = ok
				}
				if success {
					break
				}
			}
			assert.True(t, success, "Expected pool removal check failed")
		})
	}
}
