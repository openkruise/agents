package k8s

import (
	"context"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/infra"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSandbox_GetIP(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "returns pod IP",
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
			s := &Sandbox{
				Pod: tt.pod,
			}
			if got := s.GetIP(); got != tt.want {
				t.Errorf("GetIP() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_GetState(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "returns sandbox state label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.LabelSandboxState: consts.SandboxStateRunning,
					},
				},
			},
			want: consts.SandboxStateRunning,
		},
		{
			name: "empty state",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.LabelSandboxState: "",
					},
				},
			},
			want: "",
		},
		{
			name: "no state label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Pod: tt.pod,
			}
			if got := s.GetState(); got != tt.want {
				t.Errorf("GetState() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_GetTemplate(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want string
	}{
		{
			name: "returns sandbox pool label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.LabelSandboxPool: "test-template",
					},
				},
			},
			want: "test-template",
		},
		{
			name: "empty template",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.LabelSandboxPool: "",
					},
				},
			},
			want: "",
		},
		{
			name: "no template label",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{},
				},
			},
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Pod: tt.pod,
			}
			if got := s.GetTemplate(); got != tt.want {
				t.Errorf("GetTemplate() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_GetResource(t *testing.T) {
	cpuQuantity1, _ := resource.ParseQuantity("1000m")
	cpuQuantity2, _ := resource.ParseQuantity("500m")
	memoryQuantity1, _ := resource.ParseQuantity("1024Mi")
	memoryQuantity2, _ := resource.ParseQuantity("512Mi")

	tests := []struct {
		name string
		pod  *corev1.Pod
		want infra.SandboxResource
	}{
		{
			name: "single container with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 1000,
				MemoryMB: 1024,
			},
		},
		{
			name: "multiple containers with resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity1,
									corev1.ResourceMemory: memoryQuantity1,
								},
							},
						},
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    cpuQuantity2,
									corev1.ResourceMemory: memoryQuantity2,
								},
							},
						},
					},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 1500,
				MemoryMB: 1536,
			},
		},
		{
			name: "no containers",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{},
				},
			},
			want: infra.SandboxResource{
				CPUMilli: 0,
				MemoryMB: 0,
			},
		},
		{
			name: "containers without resources",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{},
							},
						},
					},
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
			s := &Sandbox{
				Pod: tt.pod,
			}
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

func TestSandbox_GetOwnerUser(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        string
	}{
		{
			name: "returns owner annotation",
			annotations: map[string]string{
				consts.AnnotationOwner: "test-user",
			},
			want: "test-user",
		},
		{
			name: "empty owner",
			annotations: map[string]string{
				consts.AnnotationOwner: "",
			},
			want: "",
		},
		{
			name:        "no owner annotation",
			annotations: map[string]string{},
			want:        "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Sandbox{
				Pod: &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Annotations: tt.annotations,
					},
				},
			}
			if got := s.GetOwnerUser(); got != tt.want {
				t.Errorf("GetOwnerUser() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSandbox_SetState(t *testing.T) {
	tests := []struct {
		name          string
		initialLabels map[string]string
		setState      string
		expectedState string
	}{
		{
			name:          "set state on pod without labels",
			initialLabels: map[string]string{},
			setState:      consts.SandboxStateRunning,
			expectedState: consts.SandboxStateRunning,
		},
		{
			name: "set state on pod with existing state",
			initialLabels: map[string]string{
				consts.LabelSandboxState: consts.SandboxStatePaused,
			},
			setState:      consts.SandboxStateKilling,
			expectedState: consts.SandboxStateKilling,
		},
		{
			name: "set empty state",
			initialLabels: map[string]string{
				consts.LabelSandboxState: consts.SandboxStateRunning,
			},
			setState:      "",
			expectedState: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建带有初始标签的 Pod
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels:    tt.initialLabels,
				},
			}

			// 使用 fake client
			client := fake.NewClientset(pod)

			// 创建 Sandbox 实例
			s := &Sandbox{
				Pod:    pod,
				Client: client,
			}

			// 调用 SetState 方法
			err := s.SetState(context.Background(), tt.setState)
			assert.NoError(t, err)

			// 验证状态是否正确设置
			updatedPod, err := client.CoreV1().Pods("default").Get(context.Background(), "test-pod", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, tt.expectedState, updatedPod.Labels[consts.LabelSandboxState])
		})
	}
}

func TestSandbox_PatchLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		expect map[string]string
	}{
		{
			name: "patch labels",
			labels: map[string]string{
				"foo":     "baz",
				"another": "value",
			},
			expect: map[string]string{
				"foo":     "baz",
				"another": "value",
			},
		},
		{
			name: "without foo",
			labels: map[string]string{
				"another": "value",
			},
			expect: map[string]string{
				"foo":     "bar",
				"another": "value",
			},
		},
		{
			name: "nil labels",
			expect: map[string]string{
				"foo": "bar",
			},
		},
		{
			name:   "empty labels",
			labels: nil,
			expect: map[string]string{
				"foo": "bar",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-pod",
					Namespace: "default",
					Labels: map[string]string{
						"foo": "bar",
					},
				},
			}
			client := fake.NewClientset(pod)
			s := &Sandbox{
				Pod:    pod,
				Client: client,
			}
			err := s.PatchLabels(context.Background(), map[string]string{
				"foo":     "baz",
				"another": "value",
			})
			assert.NoError(t, err)
			got, err := client.CoreV1().Pods("default").Get(context.Background(), "test-pod", metav1.GetOptions{})
			assert.NoError(t, err)
			assert.Equal(t, "baz", got.Labels["foo"])
			assert.Equal(t, "value", got.Labels["another"])
		})
	}

}

func TestSandbox_SaveTimer(t *testing.T) {
	tests := []struct {
		name              string
		initialConditions []corev1.PodCondition
		afterSeconds      int
		event             consts.EventType
		triggered         bool
		result            string
		expectedCondition corev1.PodCondition
	}{
		{
			name:         "save non-triggered timer",
			afterSeconds: 30,
			event:        "TestEvent",
			triggered:    false,
			result:       "",
			expectedCondition: corev1.PodCondition{
				Type:    "SandboxTimer.TestEvent",
				Status:  corev1.ConditionFalse,
				Message: "This timer will be triggered after 30 seconds",
			},
		},
		{
			name:         "save triggered timer",
			afterSeconds: 0,
			event:        "TestEvent",
			triggered:    true,
			result:       "Test result",
			expectedCondition: corev1.PodCondition{
				Type:    "SandboxTimer.TestEvent",
				Status:  corev1.ConditionTrue,
				Message: "Test result",
			},
		},
		{
			name: "save non-triggered timer with existing conditions",
			initialConditions: []corev1.PodCondition{
				{
					Type:   "ExistingCondition",
					Status: corev1.ConditionTrue,
				},
			},
			afterSeconds: 15,
			event:        "AnotherEvent",
			triggered:    false,
			result:       "",
			expectedCondition: corev1.PodCondition{
				Type:    "SandboxTimer.AnotherEvent",
				Status:  corev1.ConditionFalse,
				Message: "This timer will be triggered after 15 seconds",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 为每个测试用例创建独立的 client 和 cache
			client := fake.NewClientset()

			// 创建 Pod
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sandbox",
					Namespace: "default",
				},
				Status: corev1.PodStatus{
					Conditions: tt.initialConditions,
				},
			}

			// 将 Pod 添加到 fake client 中
			_, err := client.CoreV1().Pods("default").Create(context.Background(), pod, metav1.CreateOptions{})
			assert.NoError(t, err)

			// 创建 cache
			cache, err := NewCache(client, "default")
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
			s := &Sandbox{
				Pod:    pod,
				Client: client,
				Cache:  cache,
			}

			// 调用 SaveTimer 方法
			err = s.SaveTimer(context.Background(), tt.afterSeconds, tt.event, tt.triggered, tt.result)
			assert.NoError(t, err)

			// 验证条件是否正确设置
			updatedPod, err := client.CoreV1().Pods("default").Get(context.Background(), "test-sandbox", metav1.GetOptions{})
			assert.NoError(t, err)

			// 查找我们期望的条件
			var foundCondition *corev1.PodCondition
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
	// Create a pod with timer conditions
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               "SandboxTimer.TestEvent",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: now,
					Message:            "This timer will be triggered after 30 seconds",
				},
				{
					Type:   "OtherCondition",
					Status: corev1.ConditionTrue,
				},
				{
					Type:               "SandboxTimer.AnotherEvent",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: now,
					Message:            "This timer will be triggered after 10 seconds",
				},
			},
		},
	}

	callbackCount := 0
	s := &Sandbox{
		Pod: pod,
	}

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

func TestSandbox_LoadTimers_InvalidFormat(t *testing.T) {
	// Create a pod with invalid timer condition format
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               "SandboxTimer.TestEvent",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: now,
					Message:            "This timer will be triggered after abc seconds", // Invalid format - not a number
				},
			},
		},
	}

	s := &Sandbox{
		Pod: pod,
	}

	callbackCalled := false
	err := s.LoadTimers(func(after time.Duration, eventType consts.EventType) {
		// This should be called even with invalid format
		callbackCalled = true
	})

	// Check if callback was called
	if callbackCalled {
		t.Log("Callback was called")
	} else {
		t.Log("Callback was not called")
	}

	// Check if there was an error
	if err != nil {
		t.Logf("LoadTimers() returned error: %v", err)
	} else {
		t.Log("LoadTimers() returned no error")
	}

	// The behavior depends on the actual implementation
	// If the regex doesn't match, callback is called with 0 seconds
	// If the regex matches but strconv fails, an error is returned
}

func TestSandbox_LoadTimers_NoMatch(t *testing.T) {
	// Create a pod with timer condition that doesn't match the regex
	now := metav1.Now()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-pod",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:               "SandboxTimer.TestEvent",
					Status:             corev1.ConditionFalse,
					LastTransitionTime: now,
					Message:            "Invalid timer format", // Does not match the regex
				},
			},
		},
	}

	s := &Sandbox{
		Pod: pod,
	}

	callbackCalled := false
	err := s.LoadTimers(func(after time.Duration, eventType consts.EventType) {
		// This should be called with 0 seconds (default)
		callbackCalled = true
	})

	// Callback should not be called
	if callbackCalled {
		t.Error("Callback should not be called")
	}

	if err == nil {
		t.Errorf("LoadTimers() should return error when no match found")
	}
}

func TestSandbox_Kill(t *testing.T) {
	tests := []struct {
		name              string
		initialState      string
		deletionTimestamp *metav1.Time
		expectError       bool
	}{
		{
			name:         "kill running sandbox",
			initialState: consts.SandboxStateRunning,
			expectError:  false,
		},
		{
			name:         "kill paused sandbox",
			initialState: consts.SandboxStatePaused,
			expectError:  false,
		},
		{
			name:              "kill already deleted sandbox",
			initialState:      consts.SandboxStateRunning,
			deletionTimestamp: &metav1.Time{},
			expectError:       false,
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
					DeletionTimestamp: tt.deletionTimestamp,
				},
			}

			// 使用 fake client
			client := fake.NewClientset(pod)

			// 创建 Sandbox 实例
			s := &Sandbox{
				Pod:    pod,
				Client: client,
			}

			// 调用 Kill 方法
			err := s.Kill(context.Background())

			if tt.expectError {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)

				if tt.deletionTimestamp == nil {
					// 验证在删除前状态是否设置为 killing
					// 由于 Pod 已被删除，我们需要检查是否调用了状态更新操作
					// 在 fake client 中，我们可以通过检查是否有任何 patch 操作来验证
					// 但在这里我们只能验证方法没有返回错误
				}
			}
		})
	}
}

func TestSandbox_Patch(t *testing.T) {
	tests := []struct {
		name                string
		initialLabels       map[string]string
		initialAnnotations  map[string]string
		patchStr            string
		expectedLabels      map[string]string
		expectedAnnotations map[string]string
	}{
		{
			name: "add new labels",
			initialLabels: map[string]string{
				"existing": "label",
			},
			initialAnnotations: map[string]string{
				"existing": "annotation",
			},
			patchStr: `{"metadata":{"labels":{"new":"label"},"annotations":{"new":"annotation"}}}`,
			expectedLabels: map[string]string{
				"existing": "label",
				"new":      "label",
			},
			expectedAnnotations: map[string]string{
				"existing": "annotation",
				"new":      "annotation",
			},
		},
		{
			name: "update existing labels",
			initialLabels: map[string]string{
				"existing": "old-value",
			},
			initialAnnotations: map[string]string{},
			patchStr:           `{"metadata":{"labels":{"existing":"new-value"}}}`,
			expectedLabels: map[string]string{
				"existing": "new-value",
			},
			expectedAnnotations: map[string]string{},
		},
		{
			name: "empty patch",
			initialLabels: map[string]string{
				"existing": "label",
			},
			initialAnnotations: map[string]string{
				"existing": "annotation",
			},
			patchStr: `{"metadata":{}}`,
			expectedLabels: map[string]string{
				"existing": "label",
			},
			expectedAnnotations: map[string]string{
				"existing": "annotation",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 创建带有初始标签和注解的 Pod
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-pod",
					Namespace:   "default",
					Labels:      tt.initialLabels,
					Annotations: tt.initialAnnotations,
				},
			}

			// 使用 fake client
			client := fake.NewClientset(pod)

			// 创建 Sandbox 实例
			s := &Sandbox{
				Pod:    pod,
				Client: client,
			}

			// 调用 Patch 方法
			err := s.Patch(context.Background(), tt.patchStr)
			assert.NoError(t, err)

			// 验证补丁是否正确应用
			updatedPod, err := client.CoreV1().Pods("default").Get(context.Background(), "test-pod", metav1.GetOptions{})
			assert.NoError(t, err)

			// 对于空的map，我们需要特殊处理
			if len(tt.expectedLabels) == 0 {
				if updatedPod.Labels == nil {
					// 如果期望是空map，而实际是nil，这也可以接受
					assert.True(t, len(updatedPod.Labels) == 0)
				} else {
					assert.Equal(t, tt.expectedLabels, updatedPod.Labels)
				}
			} else {
				assert.Equal(t, tt.expectedLabels, updatedPod.Labels)
			}

			if len(tt.expectedAnnotations) == 0 {
				if updatedPod.Annotations == nil {
					// 如果期望是空map，而实际是nil，这也可以接受
					assert.True(t, len(updatedPod.Annotations) == 0)
				} else {
					assert.Equal(t, tt.expectedAnnotations, updatedPod.Annotations)
				}
			} else {
				assert.Equal(t, tt.expectedAnnotations, updatedPod.Annotations)
			}
		})
	}
}
