package sandboxcr

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	sandboxclient "github.com/openkruise/agents/client/clientset/versioned"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testHandleFuncPreparation(t *testing.T, poolExists bool, client sandboxclient.Interface,
	eventer *events.Eventer) *Infra {
	// 创建测试用的模板
	sbs := &v1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pool",
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxSetSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						v1alpha1.LabelSandboxPool: "test-pool",
						// hack: create available sandboxes directly
						v1alpha1.LabelSandboxState: v1alpha1.SandboxStateAvailable,
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
	// create 2 sandboxes
	for i := 0; i < 2; i++ {
		sbx := &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-pod-%d", i),
				Namespace: "default",
				Labels: map[string]string{
					v1alpha1.LabelTemplateHash: "TODO",
					v1alpha1.LabelSandboxPool:  sbs.Name,
				},
			},
			Spec: v1alpha1.SandboxSpec{
				Template: sbs.Spec.Template,
			},
		}
		_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
		assert.NoError(t, err)
	}
	infraInstance, err := NewInfra("default", eventer, client)
	assert.NoError(t, err)

	if poolExists {
		pool := infraInstance.NewPool(sbs.Name, sbs.Namespace)
		infraInstance.AddPool(sbs.Name, pool)
	}
	err = infraInstance.Run(context.Background())
	assert.NoError(t, err)

	if _, ok := infraInstance.GetPoolByTemplate(sbs.Name); ok && poolExists {
		time.Sleep(time.Second)
	}
	return infraInstance
}

//goland:noinspection GoDeprecation
func TestInfra_HandleSandboxDelete(t *testing.T) {
	utils.InitLogOutput()
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
				_, ok := infraInstance.GetPoolByTemplate("test-pool")
				assert.True(t, ok)
			}

			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					UID:       "test-uid-123",
					Labels: map[string]string{
						v1alpha1.LabelSandboxPool:  "test-pool",
						v1alpha1.LabelTemplateHash: hash,
						v1alpha1.LabelSandboxState: v1alpha1.SandboxStateRunning,
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
