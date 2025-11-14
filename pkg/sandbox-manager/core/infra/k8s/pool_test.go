package k8s

import (
	"context"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func TestPool_SyncWithCluster(t *testing.T) {

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

	// 测试用例
	tests := []struct {
		name          string
		existingDep   *appsv1.Deployment
		expectError   bool
		expectCreated bool
	}{
		{
			name:          "create new deployment",
			existingDep:   nil,
			expectError:   false,
			expectCreated: true,
		},
		{
			name: "update existing deployment",
			existingDep: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxPool: "test-template",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &[]int32{2}[0],
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							consts.LabelSandboxPool: "test-template",
						},
					},
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
									Image: "nginx:old",
								},
							},
						},
					},
				},
			},
			expectError:   false,
			expectCreated: false,
		},
		{
			name: "conflict with non-sandbox deployment",
			existingDep: &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxPool: "different-value",
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: &[]int32{2}[0],
				},
			},
			expectError:   true,
			expectCreated: false,
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

			// 启动缓存
			done := make(chan struct{})
			go c.Run(done)
			<-done

			// 创建eventer
			eventer := events.NewEventer()

			// 创建测试用的pool
			pool := &Pool{
				template: template,
				client:   client,
				cache:    c,
				eventer:  eventer,
			}

			// 如果需要预先创建Deployment
			if tt.existingDep != nil {
				_, err := client.AppsV1().Deployments("default").Create(context.Background(), tt.existingDep, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create existing deployment: %v", err)
				}
				// 等待informer同步
				time.Sleep(100 * time.Millisecond)
			}

			c.Refresh()
			// 执行测试
			err = pool.SyncWithCluster(context.Background())

			// 验证结果
			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}

				// 检查Deployment是否存在
				dep, err := client.AppsV1().Deployments("default").Get(context.Background(), "test-template", metav1.GetOptions{})
				if err != nil {
					t.Errorf("Failed to get deployment: %v", err)
				} else if dep == nil {
					t.Error("Expected deployment to exist")
				}
			}
		})
	}
}

func TestPool_Refresh(t *testing.T) {
	utils.InitKLogOutput()
	// 创建fake Client set
	client := fake.NewClientset()

	// 创建cache
	c, err := NewCache(client, "default")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// 启动缓存
	done := make(chan struct{})
	go c.Run(done)
	<-done

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
			MaxPoolSize: 5,
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

	// 创建测试用的deployment
	replicas := int32(3)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-template",
			Namespace: "default",
			Labels: map[string]string{
				consts.LabelSandboxPool: "test-template",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
		},
		Status: appsv1.DeploymentStatus{
			Replicas: 3,
		},
	}

	// 添加deployment到fake Client
	_, err = client.AppsV1().Deployments("default").Create(context.Background(), deployment, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create deployment: %v", err)
	}

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
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod2",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxPool:  "test-template",
					consts.LabelSandboxState: consts.SandboxStateRunning,
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod3",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxPool:  "test-template",
					consts.LabelSandboxState: consts.SandboxStatePaused,
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod4",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxPool: "test-template",
					// 无状态
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
			},
		},
		{
			// 尚未就绪的 Pending Pod
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod5",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxPool: "test-template",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
			},
		},
		{
			// 需要被 GC 的 unmanaged pod
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod6",
				Namespace: "default",
				Labels: map[string]string{
					consts.LabelSandboxPool: "test-template",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodFailed,
			},
		},
	}
	var mu sync.Mutex
	podReadyEvents := map[string]bool{}
	// 注册事件，以监控正确调用
	eventer.RegisterHandler(consts.SandboxCreated, &events.Handler{
		HandleFunc: func(evt events.Event) error {
			mu.Lock()
			defer mu.Unlock()
			podReadyEvents[evt.Sandbox.GetName()] = true
			return nil
		},
	})

	// 添加pods到fake Client
	for _, pod := range pods {
		if err = CreatePodAndReady(client, pod); err != nil {
			t.Fatalf("Failed to create pod %s: %v", pod.Name, err)
		}
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	// 执行测试
	err = pool.Refresh(context.Background())
	if err != nil {
		t.Errorf("Unexpected error: %v", err)
	}

	// 验证计数器
	if pool.total.Load() != 3 {
		t.Errorf("Expected total to be 3, got %d", pool.total.Load())
	}
	if pool.pending.Load() != 1 {
		t.Errorf("Expected pending to be 1, got %d", pool.pending.Load())
	}
	if pool.running.Load() != 1 {
		t.Errorf("Expected running to be 1, got %d", pool.running.Load())
	}
	if pool.paused.Load() != 1 {
		t.Errorf("Expected paused to be 1, got %d", pool.paused.Load())
	}

	// 检查事件执行结果
	var readyMeet bool
	for i := 0; i < 5; i++ {
		mu.Lock()
		readyMeet = reflect.DeepEqual(podReadyEvents, map[string]bool{"pod1": true, "pod2": true, "pod3": true, "pod4": true})
		mu.Unlock()
		if readyMeet {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !readyMeet {
		t.Errorf("Expected pod ready events not met, got %v", podReadyEvents)
	}

	// 检查 GC 效果
	list, err := client.CoreV1().Pods("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Errorf("Failed to list pods: %v", err)
	}
	if len(list.Items) != 5 {
		t.Errorf("Expected 5 pods, got %d", len(list.Items))
	}

	// 停止缓存
	c.Stop()
}

func TestPool_AddState(t *testing.T) {
	// 创建fake Client set
	client := fake.NewClientset()

	// 创建cache
	c, err := NewCache(client, "default")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// 启动缓存
	done := make(chan struct{})
	go c.Run(done)
	<-done

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
			MaxPoolSize: 5,
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

	// 测试用例
	tests := []struct {
		name        string
		state       string
		delta       int32
		expected    int32
		counterName string
	}{
		{
			name:        "add pending state",
			state:       consts.SandboxStatePending,
			delta:       1,
			expected:    1,
			counterName: "pending",
		},
		{
			name:        "subtract pending state",
			state:       consts.SandboxStatePending,
			delta:       -1,
			expected:    0,
			counterName: "pending",
		},
		{
			name:        "add running state",
			state:       consts.SandboxStateRunning,
			delta:       2,
			expected:    2,
			counterName: "running",
		},
		{
			name:        "subtract running state",
			state:       consts.SandboxStateRunning,
			delta:       -1,
			expected:    1,
			counterName: "running",
		},
		{
			name:        "add paused state",
			state:       consts.SandboxStatePaused,
			delta:       3,
			expected:    3,
			counterName: "paused",
		},
		{
			name:        "subtract paused state",
			state:       consts.SandboxStatePaused,
			delta:       -2,
			expected:    1,
			counterName: "paused",
		},
		{
			name:        "unknown state",
			state:       "unknown",
			delta:       5,
			expected:    0,
			counterName: "unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pool.addState(tt.state, tt.delta)

			switch tt.counterName {
			case "pending":
				if pool.pending.Load() != tt.expected {
					t.Errorf("Expected pending to be %d, got %d", tt.expected, pool.pending.Load())
				}
			case "running":
				if pool.running.Load() != tt.expected {
					t.Errorf("Expected running to be %d, got %d", tt.expected, pool.running.Load())
				}
			case "paused":
				if pool.paused.Load() != tt.expected {
					t.Errorf("Expected paused to be %d, got %d", tt.expected, pool.paused.Load())
				}
			}
		})
	}

	// 停止缓存
	c.Stop()
}

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
			client := fake.NewClientset()

			// 创建cache
			c, err := NewCache(client, "default")
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
				_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
				if err != nil {
					t.Fatalf("Failed to create sbx %s: %v", pod.Name, err)
				}
			}

			// 设置pending计数
			pool.pending.Store(2)

			for _, pod := range pods {
				if _, err = client.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("Failed to create sbx %s: %v", pod.Name, err)
				}
			}
			c.Refresh()
			pool.pending.Store(tt.pending)

			user := "test-user"
			// 执行测试
			sbx, err := pool.ClaimSandbox(context.Background(), user, tt.modifier)

			// 验证结果
			if tt.expectError {
				if err == nil {
					t.Error("Expected error but got none")
				}
				return
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
					return
				}
				if sbx == nil {
					t.Error("Expected sbx but got nil")
					return
				}
				if sbx.GetLabels()[consts.LabelSandboxState] != consts.SandboxStateRunning {
					t.Errorf("Expected sbx state to be running, got %s", sbx.GetLabels()[consts.LabelSandboxState])
					return
				}
				if sbx.GetAnnotations()[consts.AnnotationLock] == "" {
					t.Errorf("Expected sbx to be locked")
					return
				}
				if sbx.GetAnnotations()[consts.AnnotationOwner] != user {
					t.Errorf("Expected sbx owner to be %s, got %s", user, sbx.GetAnnotations()[consts.AnnotationOwner])
					return
				}

				// 如果有modifier，验证注解被添加
				if tt.modifier != nil {
					if sbx.GetAnnotations()["test-annotation"] != "test-value" {
						t.Error("Expected annotation to be added by modifier")
					}
				}
			}
		})
	}
}

func TestPool_OnDeploymentStatusUpdate(t *testing.T) {
	// 创建fake Client set
	client := fake.NewClientset()

	// 创建cache
	c, err := NewCache(client, "default")
	if err != nil {
		t.Fatalf("Failed to create cache: %v", err)
	}

	// 启动缓存
	done := make(chan struct{})
	go c.Run(done)
	<-done

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
			MaxPoolSize: 5,
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

	// 测试用例
	tests := []struct {
		name          string
		initialTotal  int32
		replicas      int32
		expectedTotal int32
	}{
		{
			name:          "update deployment replicas from 0 to 5",
			initialTotal:  0,
			replicas:      5,
			expectedTotal: 5,
		},
		{
			name:          "update deployment replicas from 5 to 3",
			initialTotal:  5,
			replicas:      3,
			expectedTotal: 3,
		},
		{
			name:          "update deployment replicas from 3 to 0",
			initialTotal:  3,
			replicas:      0,
			expectedTotal: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 设置初始状态
			pool.total.Store(tt.initialTotal)

			// 创建deployment对象
			deployment := &appsv1.Deployment{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-template",
					Namespace: "default",
				},
				Status: appsv1.DeploymentStatus{
					Replicas: tt.replicas,
				},
			}

			// 调用onDeploymentStatusUpdate方法
			pool.onDeploymentStatusUpdate(deployment)

			// 验证total计数器是否正确更新
			if pool.total.Load() != tt.expectedTotal {
				t.Errorf("Expected total to be %d, got %d", tt.expectedTotal, pool.total.Load())
			}
		})
	}

	// 停止缓存
	c.Stop()
}

func TestPool_OnPodUpdate(t *testing.T) {

	// 测试用例
	tests := []struct {
		name               string
		oldPod             *corev1.Pod
		newPod             *corev1.Pod
		expectedPending    int32
		expectedRunning    int32
		expectedPaused     int32
		expectEventTrigger consts.EventType
	}{
		{
			name: "state change from pending to running",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStatePending,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStateRunning,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPending:    0,
			expectedRunning:    1,
			expectedPaused:     0,
			expectEventTrigger: "",
		},
		{
			name: "state change from running to paused",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStateRunning,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStatePaused,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPending:    0,
			expectedRunning:    0,
			expectedPaused:     1,
			expectEventTrigger: "",
		},
		{
			name: "state change from paused to pending",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStatePaused,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStatePending,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPending:    1,
			expectedRunning:    0,
			expectedPaused:     0,
			expectEventTrigger: "",
		},
		{
			name: "no state to pending state",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: "",
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStatePending,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPending:    1,
			expectedRunning:    0,
			expectedPaused:     0,
			expectEventTrigger: "",
		},
		{
			name: "no state to no state with running phase",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: "",
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodPending,
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: "",
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
					Conditions: []corev1.PodCondition{
						{
							Type:   corev1.PodReady,
							Status: corev1.ConditionTrue,
						},
					},
				},
			},
			expectedPending:    0,
			expectedRunning:    0,
			expectedPaused:     0,
			expectEventTrigger: consts.SandboxCreated,
		},
		{
			name: "same state - no change",
			oldPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStateRunning,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			newPod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStateRunning,
					},
				},
				Status: corev1.PodStatus{
					Phase: corev1.PodRunning,
				},
			},
			expectedPending:    0,
			expectedRunning:    0,
			expectedPaused:     0,
			expectEventTrigger: "",
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

			// 启动缓存
			done := make(chan struct{})
			go c.Run(done)
			<-done

			// 创建eventer
			eventer := events.NewEventer()

			// 注册处理器以验证事件是否被触发
			var sandboxCreateTrigger atomic.Bool
			eventer.RegisterHandler(consts.SandboxCreated, &events.Handler{
				Name: "test-pod-ready-handler",
				HandleFunc: func(evt events.Event) error {
					sandboxCreateTrigger.Store(true)
					return nil
				},
			})

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
			_, _ = client.AppsV1().Deployments("default").Create(context.Background(), ParseTemplateAsDeployment(template), metav1.CreateOptions{})

			// 创建测试用的pool
			pool := &Pool{
				template: template,
				client:   client,
				cache:    c,
				eventer:  eventer,
			}

			tt.newPod.Labels[consts.LabelSandboxPool] = template.Name
			SetPodReady(tt.oldPod)
			assert.NoError(t, CreatePodAndReady(client, tt.newPod))

			// 调用onPodUpdate方法
			pool.onPodUpdate(context.Background(), tt.oldPod, tt.newPod)
			c.Refresh()

			check := func() error {
				// 验证计数器是否正确更新
				if pending := pool.pending.Load(); pending != tt.expectedPending {
					return fmt.Errorf("expected pending to be %d, got %d", tt.expectedPending, pending)
				}
				if running := pool.running.Load(); running != tt.expectedRunning {
					return fmt.Errorf("expected running to be %d, got %d", tt.expectedRunning, running)
				}
				if paused := pool.paused.Load(); paused != tt.expectedPaused {
					return fmt.Errorf("expected paused to be %d, got %d", tt.expectedPaused, paused)
				}
				// 验证事件是否被触发
				switch tt.expectEventTrigger {
				case consts.SandboxCreated:
					if !sandboxCreateTrigger.Load() {
						return fmt.Errorf("expected SandboxCreated event not triggered")
					}
				default:
					if sandboxCreateTrigger.Load() {
						return fmt.Errorf("SandboxCreated event triggered")
					}
				}
				return nil
			}

			var checkErr error
			for i := 0; i < 10; i++ {
				checkErr = check()
				if checkErr == nil {
					break
				}
				// wait for cache update
				time.Sleep(500 * time.Millisecond)
			}
			assert.NoError(t, checkErr)
			// 停止缓存
			c.Stop()
		})
	}
}

func CreatePodAndReady(client kubernetes.Interface, pod *corev1.Pod) error {
	_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	SetPodReady(pod)
	_, err = client.CoreV1().Pods("default").UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	return nil
}

func SetPodReady(pod *corev1.Pod) {
	if pod.Status.Phase == "" {
		pod.Status.Phase = corev1.PodRunning
	}
	if pod.Status.Phase == corev1.PodRunning {
		pod.Status.Conditions = []corev1.PodCondition{
			{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			},
		}
	} else {
		pod.Status.Conditions = []corev1.PodCondition{
			{
				Type:   corev1.PodReady,
				Status: corev1.ConditionFalse,
			},
		}
	}
}
