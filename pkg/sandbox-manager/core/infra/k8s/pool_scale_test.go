package k8s

import (
	"context"
	"testing"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestNativeK8sPool_Scale(t *testing.T) {
	// 测试用例
	tests := []struct {
		name             string
		template         *infra.SandboxTemplate
		total            int32
		pending          int32
		expectUsage      intstr.IntOrString
		minPoolSize      int32
		maxPoolSize      int32
		expectedReplicas int32
		expectError      bool
		noScaleOperation bool // 标记是否应该执行scale操作
	}{
		{
			name: "正常扩容场景",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 10,
					ExpectUsage: ptr.To(intstr.Parse("50%")), // 期望使用率50%
				},
			},
			total:            4, // 总共4个实例
			pending:          1, // 1个未分配
			expectedReplicas: 5, // 实际使用3个，超出 50% 1 个，扩容到 5
			expectError:      false,
			noScaleOperation: false,
		},
		{
			name: "无需扩展场景",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 10,
					ExpectUsage: ptr.To(intstr.Parse("50%")), // 期望使用率50%
				},
			},
			total:            4, // 总共4个实例
			pending:          2, // 2个未分配
			expectedReplicas: 4,
			expectError:      false,
			noScaleOperation: false,
		},
		{
			name: "达到最大池大小限制",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 5,
					ExpectUsage: ptr.To(intstr.Parse("10%")), // 期望使用率10%
				},
			},
			total:            4, // 总共4个实例
			pending:          0, // 0个未分配（全部在使用中）
			expectedReplicas: 5, // 实际使用4个，期望扩容到 6 个（多用了 2 个），但是最大只有 5
			expectError:      false,
			noScaleOperation: false,
		},
		{
			name: "达到最小池大小限制",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 2,
					MaxPoolSize: 10,
					ExpectUsage: ptr.To(intstr.Parse("90%")), // 期望使用率90%
				},
			},
			total:            5, // 总共4个实例
			pending:          5, // 5个未分配
			expectedReplicas: 2, // 实际使用0个，少用5个，期望缩容 5 个，命中最小值2
			expectError:      false,
			noScaleOperation: false,
		},
		{
			name: "正常缩容",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 10,
					ExpectUsage: ptr.To(intstr.Parse("50%")), // 期望使用率50%
				},
			},
			total:            6, // 总共4个实例
			pending:          4, // 4个未分配
			expectedReplicas: 5, // 实际使用2个，期望使用3个，少用的 1 个进行缩容
			expectError:      false,
			noScaleOperation: false,
		},
		{
			name:             "模板为nil",
			template:         nil,
			total:            4,
			pending:          2,
			expectError:      true,
			noScaleOperation: true,
		},
		{
			name: "期望使用率解析错误",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 10,
					ExpectUsage: ptr.To(intstr.Parse("invalid")), // 无效的期望使用率
				},
			},
			total:            4,
			pending:          2,
			expectError:      true,
			noScaleOperation: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建fake Client set
			client := fake.NewClientset()

			// 创建cache
			c, err := NewCache(client, "default")
			if err != nil {
				t.Fatalf("Failed to create cache: %v", err)
			}
			defer c.Stop()

			// 创建eventer
			eventer := events.NewEventer()

			// 初始化模板
			if tt.template != nil {
				tt.template.Init("default")

				// 创建deployment
				deployment := &appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tt.template.Name,
						Namespace: tt.template.Namespace,
						Labels: map[string]string{
							consts.LabelSandboxPool: tt.template.Name,
						},
					},
					Spec: appsv1.DeploymentSpec{
						Replicas: &tt.total,
					},
				}
				_, err = client.AppsV1().Deployments(tt.template.Namespace).Create(context.Background(), deployment, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create deployment: %v", err)
				}
			}

			// 创建测试用的pool
			pool := &Pool{
				template: tt.template,
				client:   client,
				cache:    c,
				eventer:  eventer,
			}

			// 设置计数器
			pool.total.Store(tt.total)
			pool.pending.Store(tt.pending)

			// 执行测试
			err = pool.Scale(context.Background())

			// 验证结果
			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			// 如果应该执行scale操作，则检查Deployment的replicas是否正确更新
			if !tt.noScaleOperation && tt.template != nil {
				deployment, err := client.AppsV1().Deployments(tt.template.Namespace).Get(context.Background(), tt.template.Name, metav1.GetOptions{})
				assert.NoError(t, err)
				assert.NotNil(t, deployment)
				if deployment != nil {
					assert.Equal(t, tt.expectedReplicas, *deployment.Spec.Replicas)
				}
			}

			// 如果不应该执行scale操作，则检查Deployment是否未被修改
			if tt.noScaleOperation && tt.template != nil {
				deployment, err := client.AppsV1().Deployments(tt.template.Namespace).Get(context.Background(), tt.template.Name, metav1.GetOptions{})
				assert.NoError(t, err)
				assert.NotNil(t, deployment)
				if deployment != nil {
					// 应该保持原来的值
					assert.Equal(t, tt.total, *deployment.Spec.Replicas)
				}
			}
		})
	}
}

func TestNativeK8sPool_Scale_WithNoDeployment(t *testing.T) {
	// 创建fake Client set
	client := fake.NewClientset()

	// 创建cache
	c, err := NewCache(client, "default")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}
	defer c.Stop()

	// 创建eventer
	eventer := events.NewEventer()

	// 创建测试用的模板
	template := &infra.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: infra.SandboxTemplateSpec{
			MinPoolSize: 1,
			MaxPoolSize: 10,
			ExpectUsage: &[]intstr.IntOrString{intstr.FromInt32(50)}[0], // 期望使用率50%
		},
	}
	template.Init("default")

	// 创建测试用的pool
	pool := &Pool{
		template: template,
		client:   client,
		cache:    c,
		eventer:  eventer,
	}

	// 设置计数器
	pool.total.Store(4)
	pool.pending.Store(2)

	// 执行测试 - 应该返回错误，因为Deployment不存在
	err = pool.Scale(context.Background())

	// 验证结果 - 应该返回not found错误
	assert.Error(t, err)
	assert.True(t, errors.IsNotFound(err))
}
