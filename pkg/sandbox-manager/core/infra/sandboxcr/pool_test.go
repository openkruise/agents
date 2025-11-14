package sandboxcr

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/client/clientset/versioned/fake"
	informers "github.com/openkruise/agents/client/informers/externalversions"
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

//goland:noinspection GoDeprecation
func TestPool_ClaimSandbox(t *testing.T) {
	// 测试用例
	tests := []struct {
		name        string
		pending     int32
		modifier    func(sbx infra.Sandbox)
		expectError bool
		preModifier func(pod *corev1.Pod)
	}{
		{
			name:        "claim with available pending pods",
			pending:     2,
			modifier:    nil,
			expectError: false,
		},
		{
			name:        "claim with no pending pods",
			pending:     0,
			modifier:    nil,
			expectError: true,
		},
		{
			name:    "claim with modifier",
			pending: 2,
			modifier: func(sbx infra.Sandbox) {
				sbx.SetAnnotations(map[string]string{
					"test-annotation": "test-value",
				})
			},
			expectError: false,
		},
		{
			name:    "no stock",
			pending: 10,
			preModifier: func(pod *corev1.Pod) {
				pod.Annotations[consts.AnnotationLock] = "XX"
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试用的模板
			template := &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 5,
				},
			}
			template.Init("default")

			// 创建测试用的pods
			pods := []*corev1.Pod{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool:  "test-template",
							consts.LabelSandboxState: consts.SandboxStatePending,
						},
						Annotations: map[string]string{},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "pod2",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool:  "test-template",
							consts.LabelSandboxState: consts.SandboxStatePending,
						},
						Annotations: map[string]string{},
					},
				},
			}

			// 创建fake Client set
			client := fake.NewSimpleClientset()

			// 创建cache
			informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			c, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
			if err != nil {
				t.Fatalf("Failed to create cache: %v", err)
			}

			// 启动缓存
			done := make(chan struct{})
			go c.Run(done)
			<-done
			defer c.Stop()

			// 创建eventer
			eventer := events.NewEventer()

			// 创建测试用的pool
			pool := &Pool{
				template: template,
				client:   client,
				cache:    c,
				eventer:  eventer,
			}

			// 添加pods到fake Client
			for _, pod := range pods {
				if tt.preModifier != nil {
					tt.preModifier(pod)
				}
				sbx := ConvertPodToSandboxCR(pod)
				_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create sbx %s: %v", pod.Name, err)
				}
			}

			for _, pod := range pods {
				sbx := ConvertPodToSandboxCR(pod)
				if _, err = client.ApiV1alpha1().Sandboxes("default").Update(context.Background(), sbx, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Failed to create sbx %s: %v", pod.Name, err)
				}
			}
			c.Refresh()
			pool.Status.pending.Store(tt.pending)
			time.Sleep(100 * time.Millisecond)

			user := "test-user"
			// 执行测试
			sbx, err := pool.ClaimSandbox(context.Background(), user, tt.modifier)

			// 验证结果
			if tt.expectError {
				assert.Error(t, err)
				return
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, sbx)
				assert.Equal(t, consts.SandboxStateRunning, sbx.GetLabels()[consts.LabelSandboxState])
				assert.NotEmpty(t, sbx.GetAnnotations()[consts.AnnotationLock])
				assert.Equal(t, user, sbx.GetAnnotations()[consts.AnnotationOwner])
				// 如果有modifier，验证注解被添加
				if tt.modifier != nil {
					assert.Equal(t, "test-value", sbx.GetAnnotations()["test-annotation"])
				}
			}
		})
	}
}

//goland:noinspection GoDeprecation
func TestPool_Scale(t *testing.T) {
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
			noScaleOperation: true, // 无需调整
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
			total:            5, // 总共5个实例
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
			total:            6, // 总共6个实例
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
			client := fake.NewSimpleClientset()

			// 创建cache
			informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			c, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
			if err != nil {
				t.Fatalf("Failed to create cache: %v", err)
			}
			defer c.Stop()

			// 创建eventer
			eventer := events.NewEventer()

			// 初始化模板
			if tt.template != nil {
				tt.template.Init("default")
			}

			// 创建测试用的pool
			pool := &Pool{
				template:       tt.template,
				client:         client,
				cache:          c,
				eventer:        eventer,
				reconcileQueue: make(chan context.Context, 1),
			}

			// 设置计数器
			pool.Status.total.Store(tt.total)
			pool.Status.pending.Store(tt.pending)

			// 执行测试
			err = pool.Scale(context.Background())

			// 验证结果
			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			// 检查 Spec.Replicas 是否正确设置
			if !tt.noScaleOperation && tt.template != nil {
				assert.Equal(t, tt.expectedReplicas, pool.Spec.Replicas.Load())
			}

			// 检查是否有入队操作
			select {
			case <-pool.reconcileQueue:
				// 成功从队列中读取，说明有入队操作
			default:
				// 队列为空
				if !tt.noScaleOperation && tt.template != nil {
					t.Error("Expected reconcile request to be enqueued")
				}
			}
		})
	}
}

//goland:noinspection GoDeprecation
func TestPool_ContinuousScale(t *testing.T) {
	utils.InitKLogOutput()
	template := &infra.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelTemplateHash: "aaa",
			},
		},
		Spec: infra.SandboxTemplateSpec{
			MinPoolSize: 1,
			MaxPoolSize: 10,
			ExpectUsage: ptr.To(intstr.Parse("50%")), // 期望使用率50%
		},
	}
	template.Init("default")
	client := fake.NewSimpleClientset()
	eventer := events.NewEventer()

	// 创建cache
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	c, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}
	c.Run(nil)
	defer c.Stop()

	// 创建测试用的pool
	pool := &Pool{
		template:       template,
		client:         client,
		cache:          c,
		eventer:        eventer,
		reconcileQueue: make(chan context.Context, 1),
	}
	go pool.Run()
	defer pool.Stop()

	// 创建两个测试 Sandbox
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		sandbox := getBaseSandbox(int32(i))
		sandbox.Labels = map[string]string{
			consts.LabelSandboxState: consts.SandboxStateRunning,
			consts.LabelSandboxPool:  template.Name,
			consts.LabelTemplateHash: template.Labels[consts.LabelTemplateHash],
		}
		sandbox.Status.Phase = v1alpha1.SandboxRunning
		CreateSandboxWithStatus(t, client, sandbox)
	}

	// 加载初始3个Sandbox的数据并检查
	time.Sleep(time.Millisecond * 100)
	pool.Spec.Replicas.Store(2)
	err = pool.SyncFromCluster(ctx)
	assert.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	err = pool.Reconcile(ctx)
	assert.NoError(t, err)
	assert.Equal(t, int32(3), pool.Status.claimed.Load())

	// 第一次连续reconcile，预期扩容1个到4
	for i := 0; i < 10; i++ {
		time.Sleep(10 * time.Millisecond)
		err = pool.Scale(ctx)
		assert.NoError(t, err)
	}
	assert.Equal(t, int32(4), pool.Spec.Replicas.Load())
	list, err := client.ApiV1alpha1().Sandboxes(template.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Equal(t, 4, len(list.Items))
	for _, item := range list.Items {
		if item.Labels[consts.LabelSandboxState] == "" {
			item.Labels[consts.LabelSandboxState] = consts.SandboxStatePending
		}
		_, err = client.ApiV1alpha1().Sandboxes(template.Namespace).Update(ctx, &item, metav1.UpdateOptions{})
		assert.NoError(t, err)
		item.Status.Phase = v1alpha1.SandboxRunning
		_, err = client.ApiV1alpha1().Sandboxes(template.Namespace).UpdateStatus(ctx, &item, metav1.UpdateOptions{})
		assert.NoError(t, err)
	}

	// 第2次连续reconcile，预期继续扩容1个到5
	for i := 0; i < 10; i++ {
		time.Sleep(10 * time.Millisecond)
		err = pool.Scale(ctx)
		assert.NoError(t, err)
	}
	assert.Equal(t, int32(5), pool.Spec.Replicas.Load())
	list, err = client.ApiV1alpha1().Sandboxes(template.Namespace).List(ctx, metav1.ListOptions{})
	assert.NoError(t, err)
	assert.Equal(t, 5, len(list.Items))
}

//goland:noinspection GoDeprecation
func TestPool_SyncFromCluster_Events(t *testing.T) {
	// 创建fake Client set
	client := fake.NewSimpleClientset()

	// 创建cache
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	c, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}
	defer c.Stop()

	// 创建测试用的模板
	template := &infra.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
		},
		Spec: infra.SandboxTemplateSpec{
			MinPoolSize: 1,
			MaxPoolSize: 10,
		},
	}
	template.Init("default")

	// 记录事件触发次数
	var eventTriggerCount int32

	// 创建eventer并注册处理器
	eventer := events.NewEventer()
	eventer.RegisterHandler(consts.SandboxCreated, &events.Handler{
		Name: "test-handler",
		HandleFunc: func(evt events.Event) error {
			atomic.AddInt32(&eventTriggerCount, 1)
			return nil
		},
	})

	// 创建测试用的pool
	pool := &Pool{
		template:       template,
		client:         client,
		cache:          c,
		eventer:        eventer,
		reconcileQueue: make(chan context.Context, 10),
	}

	// 创建测试用的Sandbox，其中一个是Ready状态
	readySandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ready-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxPool: "test-template",
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
				},
			},
		},
	}

	notReadySandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "not-ready-sandbox",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxPool: "test-template",
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: v1alpha1.SandboxPending,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionFalse,
				},
			},
		},
	}

	// 添加Sandbox到fake client
	_, err = client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), readySandbox, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create ready sandbox: %v", err)
	}

	_, err = client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), notReadySandbox, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create not ready sandbox: %v", err)
	}

	// 启动cache
	done := make(chan struct{})
	go c.Run(done)
	<-done

	// 等待cache同步
	time.Sleep(100 * time.Millisecond)

	// 执行测试
	err = pool.SyncFromCluster(context.Background())
	if err != nil {
		t.Fatalf("SyncFromCluster failed: %v", err)
	}

	// 等待事件处理完成
	time.Sleep(100 * time.Millisecond)

	// 验证只有ready的sandbox触发了事件
	if count := atomic.LoadInt32(&eventTriggerCount); count != 1 {
		t.Errorf("Expected event to be triggered 1 time, but got %d", count)
	}
}

//goland:noinspection GoDeprecation
func TestPool_SyncFromCluster(t *testing.T) {
	// 测试用例
	tests := []struct {
		name          string
		template      *infra.SandboxTemplate
		existingSbxs  []*v1alpha1.Sandbox
		expectedTotal int32
		expectError   bool
	}{
		{
			name: "正常同步场景",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 10,
				},
			},
			existingSbxs: []*v1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
						Conditions: []metav1.Condition{
							{
								Type:   string(v1alpha1.SandboxConditionReady),
								Status: metav1.ConditionTrue,
							},
						},
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx2",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxPending,
						Conditions: []metav1.Condition{
							{
								Type:   string(v1alpha1.SandboxConditionReady),
								Status: metav1.ConditionFalse,
							},
						},
					},
				},
			},
			expectedTotal: 2,
			expectError:   false,
		},
		{
			name: "超出最大池大小限制",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 3,
				},
			},
			existingSbxs: []*v1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx2",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx3",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx4",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
					},
				},
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx5",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
					},
				},
			},
			expectedTotal: 3, // 超出最大值，应该被限制为3
			expectError:   false,
		},
		{
			name: "低于最小池大小限制",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 3,
					MaxPoolSize: 10,
				},
			},
			existingSbxs: []*v1alpha1.Sandbox{
				{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sbx1",
						Namespace: "default",
						Labels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
					Status: v1alpha1.SandboxStatus{
						Phase: v1alpha1.SandboxRunning,
					},
				},
			},
			expectedTotal: 3, // 低于最小值，应该被设置为3
			expectError:   false,
		},
		{
			name: "没有现有Sandbox",
			template: &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 10,
				},
			},
			existingSbxs:  []*v1alpha1.Sandbox{}, // 没有现有的Sandbox
			expectedTotal: 1,                     // 应该设置为最小值
			expectError:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建fake Client set
			client := fake.NewSimpleClientset()

			// 创建cache
			informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			c, err := NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
			if err != nil {
				t.Fatalf("Failed to create cache: %v", err)
			}
			defer c.Stop()

			// 创建eventer
			eventer := events.NewEventer()

			// 初始化模板
			if tt.template != nil {
				tt.template.Init("default")
			}

			// 创建测试用的pool
			pool := &Pool{
				template:       tt.template,
				client:         client,
				cache:          c,
				eventer:        eventer,
				reconcileQueue: make(chan context.Context, 10), // 增加缓冲区大小
			}

			// 添加现有的Sandbox到fake client
			if tt.existingSbxs != nil {
				for _, sbx := range tt.existingSbxs {
					_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), sbx, metav1.CreateOptions{})
					if err != nil {
						t.Fatalf("Failed to create sandbox %s: %v", sbx.Name, err)
					}
				}
			}

			// 启动cache
			done := make(chan struct{})
			go c.Run(done)
			<-done

			// 等待cache同步
			time.Sleep(100 * time.Millisecond)

			// 执行测试
			err = pool.SyncFromCluster(context.Background())

			// 验证结果
			if tt.expectError {
				assert.Error(t, err)
				return
			}

			assert.NoError(t, err)

			// 检查 Spec.Replicas 是否正确设置
			assert.Equal(t, tt.expectedTotal, pool.Spec.Replicas.Load())
		})
	}
}
