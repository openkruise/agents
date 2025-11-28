package events

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestEventer_Trigger(t *testing.T) {
	// 创建测试用的pod
	sbx := utils.FakeSandbox{}

	// 创建eventer
	eventer := NewEventer()

	// 测试用例
	tests := []struct {
		name             string
		eventType        consts.EventType
		expectedNextType consts.EventType
		expectNext       bool
	}{
		{
			name:             "SandboxCreated event triggers SandboxReady",
			eventType:        consts.SandboxCreated,
			expectedNextType: consts.SandboxReady,
			expectNext:       true,
		},
		{
			name:             "SandboxReady event",
			eventType:        consts.SandboxReady,
			expectedNextType: "",
			expectNext:       false,
		},
		{
			name:             "SandboxKill event",
			eventType:        consts.SandboxKill,
			expectedNextType: "",
			expectNext:       false,
		},
	}

	// 为每个事件类型注册处理器
	handlerCalled := make(map[consts.EventType]bool)
	nextHandlerCalled := make(map[consts.EventType]bool)

	for _, eventType := range []consts.EventType{
		consts.SandboxCreated, consts.SandboxReady, consts.SandboxKill,
	} {
		// 注册主处理器
		eventTypeCopy := eventType
		eventer.RegisterHandler(eventTypeCopy, &Handler{
			Name: "test-handler-" + string(eventTypeCopy),
			HandleFunc: func(evt Event) error {
				handlerCalled[eventTypeCopy] = true
				return nil
			},
			OnErrorFunc: nil,
		})

		// 注册级联事件处理器
		eventer.RegisterHandler(eventTypeCopy, &Handler{
			Name: "test-next-handler-" + string(eventTypeCopy),
			HandleFunc: func(evt Event) error {
				nextHandlerCalled[eventTypeCopy] = true
				return nil
			},
			OnErrorFunc: nil,
		})
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 重置调用标记
			handlerCalled[tt.eventType] = false
			if tt.expectNext {
				nextHandlerCalled[tt.expectedNextType] = false
			}

			// 触发事件
			event := Event{
				Type:    tt.eventType,
				Sandbox: sbx,
				Source:  "test",
				Message: "test message",
			}
			failures := eventer.Trigger(event)

			// 验证没有处理器失败
			if failures != 0 {
				t.Errorf("Expected 0 failures, got %d", failures)
			}

			// 验证主处理器被调用
			if !handlerCalled[tt.eventType] {
				t.Errorf("Expected handler for %s to be called", tt.eventType)
			}

			// 等待一段时间以允许级联事件触发
			if tt.expectNext {
				time.Sleep(100 * time.Millisecond)
				if !nextHandlerCalled[tt.expectedNextType] {
					t.Errorf("Expected next handler for %s to be called", tt.expectedNextType)
				}
			}
		})
	}
}

func TestEventer_TriggerWithNilPod(t *testing.T) {
	// 创建eventer
	eventer := NewEventer()

	// 测试使用nil pod触发事件
	event := Event{
		Type:    consts.SandboxCreated,
		Sandbox: nil,
		Source:  "test",
		Message: "test message",
	}

	// 触发事件
	failures := eventer.Trigger(event)

	// 验证没有失败（因为事件被忽略）
	if failures != 0 {
		t.Errorf("Expected 0 failures for nil pod event, got %d", failures)
	}
}

func TestEventer_TriggerWithErrorHandler(t *testing.T) {
	// 创建测试用的pod
	sbx := utils.FakeSandbox{}

	// 创建eventer
	eventer := NewEventer()

	// 记录是否调用了错误处理函数
	errorHandlerCalled := false

	// 注册一个会出错的处理器
	eventer.RegisterHandler(consts.SandboxCreated, &Handler{
		Name: "error-handler",
		HandleFunc: func(evt Event) error {
			return &testError{}
		},
		OnErrorFunc: func(evt Event, err error) {
			errorHandlerCalled = true
		},
	})

	// 触发事件
	event := Event{
		Type:    consts.SandboxCreated,
		Sandbox: sbx,
		Source:  "test",
		Message: "test message",
	}
	failures := eventer.Trigger(event)

	// 验证有一个失败
	if failures != 1 {
		t.Errorf("Expected 1 failure, got %d", failures)
	}

	// 验证错误处理函数被调用
	if !errorHandlerCalled {
		t.Error("Expected error handler to be called")
	}
}

func TestEventer_TriggerWithPanicHandler(t *testing.T) {
	// 创建测试用的pod
	sbx := utils.FakeSandbox{}

	// 创建eventer
	eventer := NewEventer()

	// 注册一个会panic的处理器
	eventer.RegisterHandler(consts.SandboxReady, &Handler{
		Name: "panic-handler",
		HandleFunc: func(evt Event) error {
			panic("test panic in handler")
		},
		OnErrorFunc: nil,
	})

	// 触发事件
	event := Event{
		Type:    consts.SandboxReady,
		Sandbox: sbx,
		Source:  "test",
		Message: "test message",
	}

	// 测试即使handler发生panic，程序也不会panic（触发器会捕获并处理panic）
	// 如果没有panic，测试通过
	failures := eventer.Trigger(event)

	// 验证没有失败计数（因为panic被recover捕获，不会增加failures计数）
	if failures != 0 {
		t.Errorf("Expected 0 failures as panic is recovered, got %d", failures)
	}

	// 关键是测试没有panic导致程序崩溃，这在测试运行完成时自动验证
	// 如果测试能运行到这里而不崩溃，说明panic被成功处理
}

func TestEventer_TriggerWithDeletedPod(t *testing.T) {
	// 创建一个有deletion timestamp的测试用pod
	now := metav1.Now()
	sbx := utils.FakeSandbox{
		DeletionTimestamp: &now,
	}

	// 创建eventer
	eventer := NewEventer()

	// 注册处理器
	handlerCalled := false
	eventer.RegisterHandler(consts.SandboxReady, &Handler{
		Name: "test-handler",
		HandleFunc: func(evt Event) error {
			handlerCalled = true
			return errors.New("some error")
		},
		OnErrorFunc: nil,
	})

	// 触发SandboxReady事件（对于已删除的Pod应该被忽略）
	event := Event{
		Type:    consts.SandboxReady,
		Sandbox: sbx,
		Source:  "test",
		Message: "test message",
	}
	failures := eventer.Trigger(event)

	// 验证没有失败（因为事件被忽略）
	if failures != 0 {
		t.Errorf("Expected 0 failures for deleted pod event, got %d", failures)
	}

	// 验证处理器没有被调用
	if handlerCalled {
		t.Error("Expected handler not to be called for deleted pod")
	}
}

// testError 是一个用于测试的错误类型
type testError struct{}

func (e *testError) Error() string {
	return "test error"
}

func TestEventer_OnSandboxDelete(t *testing.T) {
	// 创建测试用的pod
	sbx := utils.FakeSandbox{}

	// 创建eventer
	eventer := NewEventer()

	var startedHandlers atomic.Int32

	eventer.RegisterHandler(consts.SandboxReady, &Handler{
		Name: "sleep-handler",
		HandleFunc: func(evt Event) error {
			// 模拟长时间运行的任务
			startedHandlers.Add(1)
			t.Logf("starting processing %s", evt.Message)
			select {
			case <-evt.Cancel:
				t.Logf("canceled processing %s", evt.Message)
				return nil
			}
		},
		OnErrorFunc: nil,
	})

	// 启动两个并发的事件处理
	var firstEventResult int32 = 9999
	go func() {
		firstEventResult = eventer.Trigger(Event{
			Type:    consts.SandboxReady,
			Sandbox: sbx,
			Source:  "test",
			Message: "first event",
		})
	}()

	var secondEventResult int32 = 9999
	go func() {
		// 稍微延迟发送第二个事件，确保第一个事件已经开始处理
		time.Sleep(50 * time.Millisecond)
		secondEventResult = eventer.Trigger(Event{
			Type:    consts.SandboxReady,
			Sandbox: sbx,
			Source:  "test",
			Message: "second event",
		})
	}()

	// 等待第一个处理器开始执行
	time.Sleep(time.Second)
	if started := startedHandlers.Load(); started != 1 {
		t.Errorf("expect 1 handler started, but got %d", started)
	}
	if firstEventResult != 9999 {
		t.Errorf("first event returned")
	}
	if secondEventResult != 9999 {
		t.Errorf("second event returned")
	}

	// 调用OnSandboxDelete，不发生 panic
	eventer.OnSandboxDelete(sbx)
	time.Sleep(time.Millisecond * 50)
	if firstEventResult != 0 {
		t.Errorf("first event not returned")
	}
	if secondEventResult != -1 {
		t.Errorf("second event not cancelled")
	}
}
