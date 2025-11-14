package microvm

import (
	"context"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra/sandboxcr"
	"github.com/stretchr/testify/assert"
	"gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/api/v1alpha1"
	"gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/client/clientset/versioned/fake"
	informers "gitlab.alibaba-inc.com/serverlessinfra/sandbox-operator/client/informers/externalversions"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	client2 "sigs.k8s.io/controller-runtime/pkg/client"
)

func AsSandbox(sbx *v1alpha1.Sandbox, client *fake.Clientset, cache sandboxcr.Cache[*v1alpha1.Sandbox]) *Sandbox {
	s := &Sandbox{
		BaseSandbox: sandboxcr.BaseSandbox[*v1alpha1.Sandbox]{
			Sandbox:       sbx,
			Cache:         cache,
			SetCondition:  SetSandboxCondition,
			GetConditions: ListSandboxConditions,
			DeepCopy:      DeepCopy,
		},
		Sandbox: sbx,
	}
	if client != nil {
		s.PatchSandbox = client.ApiV1alpha1().Sandboxes("default").Patch
		s.UpdateStatus = client.ApiV1alpha1().Sandboxes("default").UpdateStatus
		s.DeleteFunc = client.ApiV1alpha1().Sandboxes("default").Delete
	}
	return s
}

func CreateSandboxWithStatus(t *testing.T, client *fake.Clientset, sbx *v1alpha1.Sandbox) {
	ctx := context.Background()
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(ctx, sbx, metav1.CreateOptions{})
	assert.NoError(t, err)
	_, err = client.ApiV1alpha1().Sandboxes("default").UpdateStatus(ctx, sbx, metav1.UpdateOptions{})
	assert.NoError(t, err)
}

func ConvertPodToSandboxCR(pod *corev1.Pod) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: pod.ObjectMeta,
		Spec: v1alpha1.SandboxSpec{
			Template: v1alpha1.TemplateSpec{
				TemplateId:          "wcriroew2qf81vxxc30f",
				BuildId:             "78771eac-140f-4024-a3e1-8e356d526962",
				BaseTemplateId:      "dbv8nep93eri7ecgfara",
				KernelVersion:       "vmlinux-6.1.102",
				FirecrackerVersion:  "v1.12.1_d990331",
				HugePages:           true,
				Vcpu:                2,
				RamMb:               1024,
				TotalDiskSizeMb:     5573,
				EnvdVersion:         "0.3.3",
				MaxSandboxLength:    1,
				AllowInternetAccess: true,
			},
		},
		Status: v1alpha1.Status{
			Phase: v1alpha1.Phase(pod.Status.Phase),
			Info: v1alpha1.Info{
				NodeIP: pod.Status.PodIP,
			},
		},
	}
}

func TestSandbox_GetIP(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "returns sandbox IP",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					PodIP: "192.168.1.1",
				},
			},
			want: "192.168.1.1",
		},
		{
			name: "empty IP",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					PodIP: "",
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandbox(ConvertPodToSandboxCR(tt.pod), nil, nil)
			if got := s.GetIP(); got != tt.want {
				t.Errorf("GetIP() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_GetResource(t *testing.T) {
	tests := []struct {
		name    string
		sandbox *v1alpha1.Sandbox
		want    infra.SandboxResource
	}{
		{
			name: "single container with resources",
			sandbox: &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					Template: v1alpha1.TemplateSpec{
						Vcpu:  1,
						RamMb: 1024,
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 1000,
				MemoryMB: 1024,
			},
		},
		{
			name: "containers without resources",
			sandbox: &v1alpha1.Sandbox{
				Spec: v1alpha1.SandboxSpec{
					Template: v1alpha1.TemplateSpec{},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := AsSandbox(tt.sandbox, nil, nil)
			got := s.GetResource()
			if got.CPUMilli != tt.want.CPUMilli {
				t.Errorf("GetResource().CPUMilli = %v, want %v", got.CPUMilli, tt.want.CPUMilli)
			}
			if got.MemoryMB != tt.want.MemoryMB {
				t.Errorf("GetResource().MemoryMB = %v, want %v", got.MemoryMB, tt.want.MemoryMB)
			}
		})
	}
}

func TestSandbox_SaveTimer(t *testing.T) {
	tests := []struct {
		name              string
		initialConditions []metav1.Condition
		afterSeconds      int
		event             consts.EventType
		triggered         bool
		result            string
		expectedCondition metav1.Condition
	}{
		{
			name:         "save non-triggered timer",
			afterSeconds: 30,
			event:        "TestEvent",
			triggered:    false,
			result:       "",
			expectedCondition: metav1.Condition{
				Type:    "SandboxTimer.TestEvent",
				Status:  metav1.ConditionFalse,
				Message: "This timer will be triggered after 30 seconds",
			},
		},
		{
			name:         "save triggered timer",
			afterSeconds: 0,
			event:        "TestEvent",
			triggered:    true,
			result:       "Test result",
			expectedCondition: metav1.Condition{
				Type:    "SandboxTimer.TestEvent",
				Status:  metav1.ConditionTrue,
				Message: "Test result",
			},
		},
		{
			name: "save non-triggered timer with another conditions",
			initialConditions: []metav1.Condition{
				{
					Type:   "ExistingCondition",
					Status: metav1.ConditionTrue,
				},
			},
			afterSeconds: 15,
			event:        "AnotherEvent",
			triggered:    false,
			result:       "",
			expectedCondition: metav1.Condition{
				Type:    "SandboxTimer.AnotherEvent",
				Status:  metav1.ConditionFalse,
				Message: "This timer will be triggered after 15 seconds",
			},
		},
		{
			name: "save non-triggered timer with existing conditions",
			initialConditions: []metav1.Condition{
				{
					Type:   "SandboxTimer.ExistingCondition",
					Status: metav1.ConditionTrue,
				},
			},
			afterSeconds: 15,
			event:        "ExistingCondition",
			triggered:    false,
			result:       "",
			expectedCondition: metav1.Condition{
				Type:    "SandboxTimer.ExistingCondition",
				Status:  metav1.ConditionFalse,
				Message: "This timer will be triggered after 15 seconds",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 为每个测试用例创建独立的 client 和 cache
			//goland:noinspection GoDeprecation
			client := fake.NewSimpleClientset()

			// 创建 Sandbox
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: v1alpha1.Status{
					Conditions: tt.initialConditions,
				},
			}

			// 将 Pod 添加到 fake client 中
			CreateSandboxWithStatus(t, client, sandbox)

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

			// 创建 Sandbox 实例
			s := AsSandbox(sandbox, client, cache)

			// 调用 SaveTimer 方法
			err = s.SaveTimer(context.Background(), tt.afterSeconds, tt.event, tt.triggered, tt.result)
			assert.NoError(t, err)

			// 验证条件是否正确设置
			updatedPod, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			// 查找我们期望的条件
			var foundCondition *metav1.Condition
			for _, condition := range updatedPod.Status.Conditions {
				if condition.Type == tt.expectedCondition.Type {
					foundCondition = &condition
					break
				}
			}

			assert.NotNil(t, foundCondition, "Expected condition not found")
			if foundCondition != nil {
				assert.Equal(t, tt.expectedCondition.Type, foundCondition.Type)
				assert.Equal(t, tt.expectedCondition.Status, foundCondition.Status)
				assert.Equal(t, tt.expectedCondition.Message, foundCondition.Message)
			}

			// 停止缓存
			cache.Stop()
		})
	}
}

func TestSandbox_LoadTimers(t *testing.T) {
	// Create a sandbox with timer conditions
	now := metav1.Now()
	sandbox := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-sandbox",
		},
		Status: v1alpha1.Status{
			Conditions: []metav1.Condition{
				{
					Type:               "SandboxTimer.TestEvent",
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
					Message:            "This timer will be triggered after 30 seconds",
				},
				{
					Type:   "OtherCondition",
					Status: metav1.ConditionTrue,
				},
				{
					Type:               "SandboxTimer.AnotherEvent",
					Status:             metav1.ConditionFalse,
					LastTransitionTime: now,
					Message:            "This timer will be triggered after 10 seconds",
				},
			},
		},
	}

	callbackCount := 0
	s := AsSandbox(sandbox, nil, nil)

	err := s.LoadTimers(func(after time.Duration, eventType consts.EventType) {
		callbackCount++
		// Verify that the event type is parsed correctly
		if eventType != "TestEvent" && eventType != "AnotherEvent" {
			t.Errorf("Unexpected event type: %s", eventType)
		}
	})

	if err != nil {
		t.Errorf("LoadTimers() error = %v, wantErr nil", err)
	}

	// Should have been called twice for the two SandboxTimer conditions
	if callbackCount != 2 {
		t.Errorf("Callback was called %d times, want 2", callbackCount)
	}
}

//goland:noinspection GoDeprecation
func TestSandbox_SetPause(t *testing.T) {
	tests := []struct {
		name            string
		phase           v1alpha1.Phase
		initialState    string
		pause           bool
		expectedState   string
		expectedNoPatch bool // 期望不进行patch操作
		expectError     bool
	}{
		{
			name:            "pause running sandbox",
			phase:           v1alpha1.SandboxRunning,
			initialState:    consts.SandboxStateRunning,
			pause:           true,
			expectedState:   consts.SandboxStatePaused,
			expectedNoPatch: false,
			expectError:     false,
		},
		{
			name:            "resume paused sandbox",
			phase:           v1alpha1.SandboxPaused,
			initialState:    consts.SandboxStatePaused,
			pause:           false,
			expectedState:   consts.SandboxStateRunning,
			expectedNoPatch: false,
			expectError:     false,
		},
		{
			name:            "pause already paused sandbox",
			phase:           v1alpha1.SandboxPaused,
			initialState:    consts.SandboxStatePaused,
			pause:           true,
			expectedState:   consts.SandboxStatePaused,
			expectedNoPatch: true,
			expectError:     true,
		},
		{
			name:            "resume already running sandbox",
			phase:           v1alpha1.SandboxRunning,
			initialState:    consts.SandboxStateRunning,
			pause:           false,
			expectedState:   consts.SandboxStateRunning,
			expectedNoPatch: true,
			expectError:     true,
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

			sandbox := ConvertPodToSandboxCR(pod)
			sandbox.Status.Phase = tt.phase
			// 使用 fake client
			client := fake.NewSimpleClientset()
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

			CreateSandboxWithStatus(t, client, sandbox)
			assert.NoError(t, err)
			// 创建 Sandbox 实例
			s := AsSandbox(sandbox, client, cache)

			// 调用 SetPause 方法
			if tt.pause {
				err = s.Pause(context.Background())
			} else {
				time.AfterFunc(20*time.Millisecond, func() {
					patch := client2.MergeFrom(s.Sandbox.DeepCopy())
					s.Status.Phase = v1alpha1.SandboxRunning
					SetSandboxCondition(s.Sandbox, string(v1alpha1.SandboxConditionReady), metav1.ConditionTrue, "Resume", "")
					data, err := patch.Data(s.Sandbox)
					assert.NoError(t, err)
					_, err = client.ApiV1alpha1().Sandboxes("default").Patch(
						context.Background(), s.Name, types.MergePatchType, data, metav1.PatchOptions{})
					assert.NoError(t, err)
				})
				err = s.Resume(context.Background())
			}
			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}

			// 获取更新后的 Pod
			updatedSbx, err := client.ApiV1alpha1().Sandboxes("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			// 验证 Pod 状态是否正确更新
			if !tt.expectedNoPatch {
				// 应该进行了 patch 操作
				assert.Equal(t, tt.expectedState, updatedSbx.Labels[consts.LabelSandboxState])
				assert.Equal(t, tt.pause, updatedSbx.Spec.Paused)
			} else {
				// 不应该进行 patch 操作，状态应该保持不变
				assert.Equal(t, tt.initialState, updatedSbx.Labels[consts.LabelSandboxState])
			}
		})
	}
}
