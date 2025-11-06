package acs

import (
	"context"
	"testing"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/k8s"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSandbox_SetPause(t *testing.T) {
	tests := []struct {
		name            string
		initialState    string
		pause           bool
		expectedState   string
		expectedNoPatch bool // 期望不进行patch操作
	}{
		{
			name:            "pause running sandbox",
			initialState:    consts.SandboxStateRunning,
			pause:           true,
			expectedState:   consts.SandboxStatePaused,
			expectedNoPatch: false,
		},
		{
			name:            "resume paused sandbox",
			initialState:    consts.SandboxStatePaused,
			pause:           false,
			expectedState:   consts.SandboxStateRunning,
			expectedNoPatch: false,
		},
		{
			name:            "pause already paused sandbox",
			initialState:    consts.SandboxStatePaused,
			pause:           true,
			expectedState:   consts.SandboxStatePaused,
			expectedNoPatch: true,
		},
		{
			name:            "resume already running sandbox",
			initialState:    consts.SandboxStateRunning,
			pause:           false,
			expectedState:   consts.SandboxStateRunning,
			expectedNoPatch: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建带有初始状态的 Pod
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: tt.initialState,
					},
					Annotations: map[string]string{},
				},
			}

			// 使用 fake client
			client := fake.NewClientset(pod)

			// 创建 k8s Sandbox 实例
			k8sSandbox := &k8s.Sandbox{
				Pod:    pod,
				Client: client,
			}

			// 创建 ACS Sandbox 实例
			s := &Sandbox{
				Sandbox: k8sSandbox,
			}

			// 调用 SetPause 方法
			var err error
			if tt.pause {
				err = s.Pause(context.Background())
			} else {
				err = s.Resume(context.Background())
			}
			assert.NoError(t, err)

			// 获取更新后的 Pod
			updatedPod, err := client.CoreV1().Pods("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			// 验证 Pod 状态是否正确更新
			if !tt.expectedNoPatch {
				// 应该进行了 patch 操作
				assert.Equal(t, tt.expectedState, updatedPod.Labels[consts.LabelSandboxState])

				expectedAnnotation := "false"
				if tt.pause {
					expectedAnnotation = "true"
				}
				assert.Equal(t, expectedAnnotation, updatedPod.Annotations[consts.AnnotationACSPause])
			} else {
				// 不应该进行 patch 操作，状态应该保持不变
				assert.Equal(t, tt.initialState, updatedPod.Labels[consts.LabelSandboxState])
			}
		})
	}
}
