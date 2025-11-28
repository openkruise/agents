package sandbox_manager

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	sandboxcr2 "github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTimer_SetTimer(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client.SandboxClient

	// 创建测试用的pod
	testPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{},
		},
	}

	// 添加pod到fake client
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), ConvertPodToSandboxCR(testPod), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test pod: %v", err)
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	// 测试用例
	tests := []struct {
		name          string
		pod           *corev1.Pod
		afterSeconds  int
		event         consts.EventType
		expectError   bool
		errorContains string
	}{
		{
			name:          "Valid timer setup",
			pod:           testPod,
			afterSeconds:  1,
			event:         consts.SandboxKill,
			expectError:   false,
			errorContains: "",
		},
		{
			name:          "Zero seconds should return error",
			pod:           testPod,
			afterSeconds:  0,
			event:         consts.SandboxKill,
			expectError:   true,
			errorContains: "afterSeconds must be greater than 0",
		},
		{
			name:          "Negative seconds should return error",
			pod:           testPod,
			afterSeconds:  -1,
			event:         consts.SandboxKill,
			expectError:   true,
			errorContains: "afterSeconds must be greater than 0",
		},
		{
			name:          "Empty event should return error",
			pod:           testPod,
			afterSeconds:  1,
			event:         "",
			expectError:   true,
			errorContains: "event name can not be empty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := manager.infra.(*sandboxcr2.Infra).AsSandbox(ConvertPodToSandboxCR(tt.pod))
			err := manager.SetTimer(context.Background(), sbx, tt.afterSeconds, tt.event)
			if tt.expectError {
				if err == nil {
					t.Errorf("Expected error but got none")
				} else if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("Expected error to contain '%s' but got '%s'", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("Unexpected error: %v", err)
				}
				// 验证定时器是否设置
				key := timerKey(sbx, tt.event)
				if _, exists := manager.timers.Load(key); !exists {
					t.Errorf("Timer was not set")
				}
			}
		})
	}
}

func TestTimer_HandleTimer(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client.SandboxClient

	// 创建测试用的pod
	testPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxID:    "test-pod",
				agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateRunning,
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{},
		},
	}

	// 添加pod到fake client
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), ConvertPodToSandboxCR(testPod), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test pod: %v", err)
	}

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	// 创建处理函数被调用的标记
	handlerCalled := false
	handlerMutex := sync.Mutex{}

	// 注册事件处理器
	manager.eventer.RegisterHandler(consts.SandboxKill, &events.Handler{
		Name: "test-handler",
		HandleFunc: func(evt events.Event) error {
			handlerMutex.Lock()
			handlerCalled = true
			handlerMutex.Unlock()
			return nil
		},
		OnErrorFunc: nil,
	})

	t.Run("Handle timer successfully", func(t *testing.T) {
		// 重置调用标记
		handlerMutex.Lock()
		handlerCalled = false
		handlerMutex.Unlock()
		sbx := manager.infra.(*sandboxcr2.Infra).AsSandbox(ConvertPodToSandboxCR(testPod))
		// 直接调用handleTimer方法
		manager.handleTimer(context.Background(), sbx, consts.SandboxKill)

		// 等待事件处理器被调用
		time.Sleep(100 * time.Millisecond)

		// 验证事件处理器被调用
		handlerMutex.Lock()
		if !handlerCalled {
			t.Errorf("Event handler was not called")
		}
		handlerMutex.Unlock()

		// 验证Pod Condition是否正确设置
		updatedSbx, err := manager.GetClaimedSandbox(testPod.Name)
		if err != nil {
			t.Fatalf("Failed to get updated sandbox: %v", err)
		}
		updatedCR := updatedSbx.(*sandboxcr2.Sandbox).Sandbox

		condition, found := sandboxcr2.GetSandboxCondition(updatedCR, "SandboxTimer.SandboxKill")
		if !found {
			t.Errorf("Timer condition was not found")
		} else {
			if condition.Status != metav1.ConditionTrue {
				t.Errorf("Expected condition status to be True, got %v", condition.Status)
			}
			if condition.Reason != "Triggered" {
				t.Errorf("Expected condition reason to be Triggered, got %s", condition.Reason)
			}
		}
	})
}

func TestTimer_ConcurrentTimers(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client.SandboxClient

	// 创建多个测试用的pods
	var testPods []*corev1.Pod
	for i := 0; i < 3; i++ {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-pod-%d", i),
				Namespace: "default",
			},
			Status: corev1.PodStatus{
				Conditions: []corev1.PodCondition{},
			},
		}
		testPods = append(testPods, pod)

		// 添加pod到fake client
		_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), ConvertPodToSandboxCR(pod), metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("Failed to create test pod: %v", err)
		}
	}

	// 等待informer同步
	time.Sleep(1000 * time.Millisecond)

	// 创建调用计数器和互斥锁
	callCount := 0
	callMutex := sync.Mutex{}

	// 注册事件处理器
	manager.eventer.RegisterHandler(consts.SandboxKill, &events.Handler{
		Name: "test-handler",
		HandleFunc: func(evt events.Event) error {
			callMutex.Lock()
			callCount++
			callMutex.Unlock()
			return nil
		},
		OnErrorFunc: nil,
	})

	// 并发设置定时器
	var wg sync.WaitGroup
	for i, pod := range testPods {
		wg.Add(1)
		go func(index int, p *corev1.Pod) {
			defer wg.Done()
			sbx := manager.infra.(*sandboxcr2.Infra).AsSandbox(ConvertPodToSandboxCR(p))
			err := manager.SetTimer(context.Background(), sbx, 1+index, consts.SandboxKill)
			if err != nil {
				t.Errorf("Failed to set timer for pod %s: %v", p.Name, err)
			}
		}(i, pod)
	}

	// 等待所有定时器设置完成
	wg.Wait()

	// 验证所有定时器都已设置
	var cnt int
	manager.timers.Range(func(key, value any) bool {
		cnt++
		return true
	})
	if cnt != len(testPods) {
		t.Errorf("Expected %d timers to be set, got %d", len(testPods), cnt)
	}

	// 等待所有定时器触发
	time.Sleep(5 * time.Second)

	// 验证所有事件处理器都被调用
	callMutex.Lock()
	if callCount != len(testPods) {
		t.Errorf("Expected event handler to be called %d times, got %d", len(testPods), callCount)
	}
	callMutex.Unlock()

	cnt = 0
	manager.timers.Range(func(key, value any) bool {
		cnt++
		return true
	})

	// 验证所有定时器都已从映射中移除
	if cnt != 0 {
		t.Errorf("Expected all timers to be removed, got %d remaining", cnt)
	}
}

func TestTimer_RecoverTimers(t *testing.T) {
	manager := setupTestManager(t)
	client := manager.client.SandboxClient

	// 创建测试用的pod，带有一个定时器条件
	testSbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxState: agentsv1alpha1.SandboxStateRunning,
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Conditions: []metav1.Condition{
				{
					Type:               "SandboxTimer.SandboxKill",
					Status:             metav1.ConditionFalse,
					Reason:             "SetTimer",
					Message:            "This timer will be triggered after 1 seconds",
					LastTransitionTime: metav1.Now(),
				},
			},
		},
	}

	CreateSandboxWithStatus(t, client, testSbx)

	// 等待informer同步
	time.Sleep(100 * time.Millisecond)

	// 创建处理函数被调用的标记
	handlerCalled := false
	handlerMutex := sync.Mutex{}

	// 注册事件处理器
	manager.eventer.RegisterHandler(consts.SandboxKill, &events.Handler{
		Name: "test-handler",
		HandleFunc: func(evt events.Event) error {
			handlerMutex.Lock()
			handlerCalled = true
			handlerMutex.Unlock()
			return nil
		},
		OnErrorFunc: nil,
	})

	// 调用recoverTimers
	err := manager.recoverFromCluster(context.Background())
	if err != nil {
		t.Fatalf("Failed to recover timers: %v", err)
	}

	// 验证定时器是否恢复
	var cnt int
	manager.timers.Range(func(key, value any) bool {
		cnt++
		return true
	})

	if cnt != 1 {
		t.Errorf("Expected 1 timer to be recovered, got %d", cnt)
	}

	// 等待定时器触发
	time.Sleep(2 * time.Second)

	// 验证事件处理器被调用
	handlerMutex.Lock()
	if !handlerCalled {
		t.Errorf("Event handler was not called after timer recovery")
	}
	handlerMutex.Unlock()
	err = client.ApiV1alpha1().Sandboxes(testSbx.Namespace).Delete(context.Background(), testSbx.Name, metav1.DeleteOptions{})
	if err != nil {
		t.Fatalf("Failed to delete test pod: %v", err)
	}
	badPod := testSbx.DeepCopy()
	badPod.Name = "bad-pod"
	badPod.Status = agentsv1alpha1.SandboxStatus{
		Conditions: []metav1.Condition{
			{
				Type:               "SandboxTimer.SandboxKill",
				Status:             metav1.ConditionFalse,
				Reason:             "SetTimer",
				Message:            "This timer will be triggered after x seconds",
				LastTransitionTime: metav1.Now(),
			},
		},
	}
	_, err = client.ApiV1alpha1().Sandboxes(badPod.Namespace).Create(context.Background(), badPod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create bad pod: %v", err)
	}
	// 等待informer同步
	time.Sleep(100 * time.Millisecond)
	err = manager.recoverFromCluster(context.Background())
	if err == nil {
		t.Fatalf("Should failed to recover timers")
	}
}

func TestTimer_ResetTimers(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: "default",
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{},
		},
	}
	manager := setupTestManager(t)
	cr := ConvertPodToSandboxCR(pod)
	sbx := manager.infra.(*sandboxcr2.Infra).AsSandbox(cr)
	triggered := make(chan time.Time)
	manager.RegisterHandler(consts.SandboxKill, "test-handler", func(event events.Event) error {
		triggered <- time.Now()
		return nil
	}, nil)
	client := manager.client.SandboxClient
	_, err := client.ApiV1alpha1().Sandboxes("default").Create(context.Background(), cr, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("Failed to create test pod: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	start := time.Now()
	err = manager.SetTimer(context.Background(), sbx, 1, consts.SandboxKill)
	if err != nil {
		t.Fatalf("Failed to set timer: %v", err)
	}
	// 0.5, 1.0, 1.5, 2.0 秒分别 reset timer，期望在第 3 秒之后触发
	for i := 0; i < 4; i++ {
		time.Sleep(500 * time.Millisecond)
		err = manager.SetTimer(context.Background(), sbx, 1, consts.SandboxKill)
		if err != nil {
			t.Fatalf("Failed to set timer: %v", err)
		}
	}
	end := <-triggered
	since := end.Sub(start)
	t.Logf("Timer triggered after %v", since)
	if since < 3*time.Second {
		t.Errorf("Expected timer to be triggered after 1 second, got %v", end.Sub(start))
	}
}
