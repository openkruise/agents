package microvm

import (
	"context"
	"sync/atomic"
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
)

//goland:noinspection GoDeprecation
func TestPool_ClaimSandbox(t *testing.T) {
	// 测试用例
	tests := []struct {
		name        string
		modifier    func(sbx infra.Sandbox)
		expectError bool
	}{
		{
			name: "claim with modifier",
			modifier: func(sbx infra.Sandbox) {
				sbx.SetAnnotations(map[string]string{
					"test-annotation": "test-value",
				})
			},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建测试用的模板
			template := &infra.SandboxTemplate{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "code-interpreter",
					Namespace: "default",
				},
				Spec: infra.SandboxTemplateSpec{
					MinPoolSize: 1,
					MaxPoolSize: 5,
				},
			}
			template.Init("default")

			// 创建fake Client set
			client := fake.NewSimpleClientset()

			// 创建cache
			informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
			sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
			c, err := sandboxcr.NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
			if err != nil {
				t.Fatalf("Failed to create cache: %v", err)
			}

			// 启动缓存
			done := make(chan struct{})
			go c.Run(done)
			<-done
			defer c.Stop()

			// update sandbox info for claiming
			c.AddSandboxEventHandler(cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					sbx := obj.(*v1alpha1.Sandbox)
					sbx.Status.Info.SandboxId = "abc"
					sbx.Status.Info.NodeIP = "1.2.3.4"
					_, _ = client.ApiV1alpha1().Sandboxes("default").UpdateStatus(context.Background(), sbx, metav1.UpdateOptions{})
				},
			})

			// 创建测试用的pool
			pool := &Pool{
				template: template,
				client:   client,
				cache:    c,
			}
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
				list, err := client.ApiV1alpha1().Sandboxes("default").List(context.Background(), metav1.ListOptions{})
				assert.NoError(t, err)
				assert.Equal(t, 1, len(list.Items))
				got := list.Items[0]
				assert.Equal(t, sbx.GetName(), got.Name)
				assert.Equal(t, consts.SandboxStateRunning, sbx.GetLabels()[consts.LabelSandboxState])
				assert.NotEmpty(t, sbx.GetAnnotations()[consts.AnnotationLock])
				assert.Equal(t, user, sbx.GetAnnotations()[consts.AnnotationOwner])
				if tt.modifier != nil {
					assert.EqualValues(t, "test-value", got.Annotations["test-annotation"])
				}
			}
		})
	}
}

//goland:noinspection GoDeprecation
func TestPool_SyncFromCluster_Events(t *testing.T) {
	// 创建fake Client set
	client := fake.NewSimpleClientset()

	// 创建cache
	informerFactory := informers.NewSharedInformerFactoryWithOptions(client, time.Minute*10, informers.WithNamespace("default"))
	sandboxInformer := informerFactory.Api().V1alpha1().Sandboxes().Informer()
	c, err := sandboxcr.NewCache[*v1alpha1.Sandbox]("default", informerFactory, sandboxInformer)
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
		template: template,
		client:   client,
		cache:    c,
		eventer:  eventer,
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
		Status: v1alpha1.Status{
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
		Status: v1alpha1.Status{
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
