package sandbox

import (
	"context"
	"fmt"
	"strings"
	"testing"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	bypassScheme *runtime.Scheme
)

func init() {
	bypassScheme = runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(bypassScheme)
	_ = corev1.AddToScheme(bypassScheme)
}

func TestBypassPodReconciler_Reconcile(t *testing.T) {
	utils.InitKLogOutput()
	// 测试用例定义
	testCases := []struct {
		name            string
		existingPod     *corev1.Pod
		existingSandbox *agentsv1alpha1.Sandbox
		expectSandbox   *agentsv1alpha1.Sandbox
		expectErr       bool
		expectEvent     []string
	}{
		{
			name: "no sandbox, pod not paused",
			existingPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationSandboxPause: utils.False,
						utils.PodAnnotationEnablePaused: utils.True,
					},
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:latest",
						},
					},
				},
			},
			existingSandbox: nil,
			expectSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.SandboxAnnotationDisablePodCreation: utils.True,
						utils.SandboxAnnotationDisablePodDeletion: utils.True,
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
				},
			},
			expectErr:   false,
			expectEvent: []string{corev1.EventTypeNormal, "SandboxCreated"},
		},
		{
			name: "no sandbox, pod paused",
			existingPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationSandboxPause: utils.True,
						utils.PodAnnotationEnablePaused: utils.True,
					},
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "nginx:latest",
						},
					},
				},
			},
			existingSandbox: nil,
			expectSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.SandboxAnnotationDisablePodCreation: utils.True,
						utils.SandboxAnnotationDisablePodDeletion: utils.True,
					},
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
				},
			},
			expectErr:   false,
			expectEvent: []string{corev1.EventTypeNormal, "SandboxCreated"},
		},
		{
			name: "sandbox not paused, pod paused",
			existingPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationSandboxPause: utils.True,
						utils.PodAnnotationEnablePaused: utils.True,
					},
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
				},
			},
			existingSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			},
			expectSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
				},
			},
			expectErr:   false,
			expectEvent: []string{corev1.EventTypeNormal, "SandboxPaused"},
		},
		{
			name: "sandbox paused, pod paused",
			existingPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationSandboxPause: utils.True,
						utils.PodAnnotationEnablePaused: utils.True,
					},
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
				},
			},
			existingSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			},
			expectSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true, // 应该保持为 true
				},
			},
			expectErr: false,
		},
		{
			name: "sandbox paused, pod paused but recreating",
			existingPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Annotations: map[string]string{
						utils.PodAnnotationSandboxPause: utils.True,
						utils.PodAnnotationEnablePaused: utils.True,
						utils.PodAnnotationRecreating:   utils.True,
					},
					Labels: map[string]string{
						utils.PodLabelEnableAutoCreateSandbox: utils.True,
					},
				},
			},
			existingSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: true,
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "main",
									Image: "nginx:latest",
								},
							},
						},
					},
				},
			},
			expectSandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
				},
				Spec: agentsv1alpha1.SandboxSpec{
					Paused: false,
				},
			},
			expectErr:   false,
			expectEvent: []string{corev1.EventTypeNormal, "SandboxResumed"},
		},
		{
			name:            "pod not exists",
			existingPod:     nil,
			existingSandbox: nil,
			expectSandbox:   nil,
			expectErr:       false, // 应该忽略 NotFound 错误
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// 创建 fake client
			builder := fake.NewClientBuilder().
				WithScheme(bypassScheme).
				WithStatusSubresource(&agentsv1alpha1.Sandbox{})

			// 添加现有的对象
			if tc.existingPod != nil {
				builder = builder.WithObjects(tc.existingPod)
			}

			if tc.existingSandbox != nil {
				builder = builder.WithObjects(tc.existingSandbox)
			}

			fakeClient := builder.Build()

			recorder := record.NewFakeRecorder(10)

			// 创建 reconciler
			reconciler := &BypassPodReconciler{
				Client:   fakeClient,
				Scheme:   bypassScheme,
				Recorder: recorder,
			}

			// 执行 reconcile
			req := ctrl.Request{
				NamespacedName: types.NamespacedName{
					Name:      "test-pod",
					Namespace: "default",
				},
			}

			var err error
			var result ctrl.Result
			for {
				result, err = reconciler.Reconcile(context.Background(), req)
				if err != nil {
					break
				}
				if !result.Requeue {
					break
				}
			}

			// 检查错误
			if tc.expectErr && err == nil {
				t.Errorf("期望错误但没有错误")
				return
			}

			if !tc.expectErr && err != nil {
				t.Errorf("未期望错误但出现错误: %v", err)
				return
			}

			// 检查 Pod created-by 注入
			if tc.existingPod != nil {
				pod := &corev1.Pod{}
				err = fakeClient.Get(context.Background(), types.NamespacedName{
					Name:      "test-pod",
					Namespace: "default",
				}, pod)
				if err != nil {
					t.Errorf("未找到 Pod: %v", err)
					return
				}
				if pod.Annotations[utils.PodAnnotationCreatedBy] != utils.CreatedByExternal {
					t.Errorf("Pod 注解 %s 不匹配，期望: %s, 实际: %s", utils.PodAnnotationCreatedBy, utils.CreatedByExternal, pod.Annotations[utils.PodAnnotationCreatedBy])
				}
			}

			// 检查 Sandbox 是否按预期创建或更新
			sandbox := &agentsv1alpha1.Sandbox{}
			err = fakeClient.Get(context.Background(), types.NamespacedName{
				Name:      "test-pod",
				Namespace: "default",
			}, sandbox)

			if tc.expectSandbox != nil {
				if err != nil {
					t.Errorf("期望 Sandbox 存在但未找到: %v", err)
					return
				}

				// 检查关键字段
				if sandbox.Spec.Paused != tc.expectSandbox.Spec.Paused {
					t.Errorf("Sandbox Paused 状态不匹配，期望: %v, 实际: %v", tc.expectSandbox.Spec.Paused, sandbox.Spec.Paused)
				}

				// 检查注解
				if tc.expectSandbox.Annotations != nil {
					for key, expectedValue := range tc.expectSandbox.Annotations {
						actualValue := sandbox.Annotations[key]
						if actualValue != expectedValue {
							t.Errorf("Sandbox 注解 %s 不匹配，期望: %s, 实际: %s", key, expectedValue, actualValue)
						}
					}
				}
			} else {
				// 不应该创建 Sandbox 或者 Pod 不存在
				if tc.existingPod == nil {
					// Pod 不存在的情况，应该没有错误
					if err != nil && !errors.IsNotFound(err) {
						t.Errorf("不应该出现错误，但出现了: %v", err)
					}
				} else if tc.existingSandbox == nil && err == nil {
					// Pod 存在但 Sandbox 不应该被创建
					if err == nil {
						// 检查是否真的创建了 Sandbox
						err = fakeClient.Get(context.Background(), types.NamespacedName{
							Name:      "test-pod",
							Namespace: "default",
						}, sandbox)
						if err == nil {
							t.Errorf("不应该创建 Sandbox，但创建了")
						} else if !errors.IsNotFound(err) {
							t.Errorf("获取 Sandbox 时出现意外错误: %v", err)
						}
					}
				}
			}
			if err := CheckEvent(recorder, tc.expectEvent); err != nil {
				t.Errorf("事件检查失败: %v", err)
			}
		})
	}
}

func CheckEvent(eventRecorder *record.FakeRecorder, expect []string) error {
	select {
	case event := <-eventRecorder.Events:
		if len(expect) == 2 {
			tp, evt := expect[0], expect[1]
			if !strings.HasPrefix(event, fmt.Sprintf("%s %s", tp, evt)) {
				return fmt.Errorf("unexpected event: %s", event)
			} else {
				return nil
			}
		} else {
			return fmt.Errorf("unexpected event: %s", event)
		}
	default:
		if len(expect) == 2 {
			return fmt.Errorf("no event received")
		} else {
			return nil
		}
	}
}
