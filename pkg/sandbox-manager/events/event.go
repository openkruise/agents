package events

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"k8s.io/klog/v2"
)

type HandleFunc func(Event) error
type OnErrorFunc func(Event, error)

var DebugLevel = 5

type Handler struct {
	Name string
	HandleFunc
	OnErrorFunc
}

type Event struct {
	Type    consts.EventType
	Sandbox infra.Sandbox
	Source  string
	Message string
	Context context.Context
	Cancel  chan struct{}
}

type channelSuite struct {
	start  chan struct{}
	cancel chan struct{}
}

func newChannelSuite() channelSuite {
	return channelSuite{
		start:  make(chan struct{}, 1),
		cancel: make(chan struct{}),
	}
}

type Eventer struct {
	handlers map[consts.EventType][]*Handler
	channels sync.Map
}

func NewEventer() *Eventer {
	return &Eventer{
		handlers: make(map[consts.EventType][]*Handler),
	}
}

// Trigger triggers an event, all handler funcs will be called parallelly and wait all handlers succeed.
//
// Use go `eventer.Trigger(evt)` to async call.
//
// NOTE: a Sandbox is required for the event.
//
// returns: number of failed handlers. -1 is returned if cancelled.
func (e *Eventer) Trigger(evt Event) int32 {
	if evt.Context == nil {
		evt.Context = logs.NewContext()
	}
	log := klog.FromContext(evt.Context).WithValues("sandbox", klog.KObj(evt.Sandbox), "type", evt.Type,
		"source", evt.Source, "message", evt.Message).V(DebugLevel)
	if evt.Sandbox == nil {
		log.Info("event ignored for Sandbox is nil")
		return 0
	}
	if evt.Sandbox.GetDeletionTimestamp() != nil && evt.Type != consts.SandboxKill {
		log.Info("non SandboxKill event ignored for sandbox is being deleted", "type", evt.Type)
		return 0
	}
	chSuiteValue, _ := e.channels.LoadOrStore(evt.Sandbox.GetUID(), newChannelSuite())
	ch := chSuiteValue.(channelSuite)
	select {
	case ch.start <- struct{}{}:
		break
	case <-ch.cancel:
		log.Info("event canceled")
		return -1
	}
	evt.Cancel = ch.cancel
	wg := sync.WaitGroup{}
	failures := atomic.Int32{}
	for _, handler := range e.handlers[evt.Type] {
		wg.Add(1)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Error(fmt.Errorf("event panic recovered: %v", r), "Panic occurred in handler")
				}
				wg.Done()
			}()
			log.Info("event will be handled by handler", "handler", handler.Name)
			if err := handler.HandleFunc(evt); err == nil {
				log.Info("event handler succeeded", "handler", handler.Name)
			} else {
				log.Error(err, "event handler failed", "handler", handler.Name)
				failures.Add(1)
				if handler.OnErrorFunc != nil {
					log.Info("on-error func will be called", "handler", handler.Name)
					handler.OnErrorFunc(evt, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	<-ch.start
	log.Info("event handled")
	e.next(evt)
	return failures.Load()
}

func (e *Eventer) RegisterHandler(evt consts.EventType, handler *Handler) {
	e.handlers[evt] = append(e.handlers[evt], handler)
}

func (e *Eventer) next(evt Event) {
	var autoTrigger bool
	switch evt.Type {
	case consts.SandboxCreated:
		evt.Type = consts.SandboxReady
		evt.Message = fmt.Sprintf("auto triggered for Sandbox %s/%s is ready to be claimed as a sandbox",
			evt.Sandbox.GetNamespace(), evt.Sandbox.GetName())
		evt.Source = "Eventer"
		autoTrigger = true
	default:
	}
	if autoTrigger {
		go e.Trigger(evt)
	}
}

func (e *Eventer) OnSandboxDelete(sbx infra.Sandbox) {
	defer func() {
		if r := recover(); r != nil {
			klog.InfoS("Panic occurred in event handler", "sandbox", klog.KObj(sbx), "reason", r)
		}
	}()
	value, loaded := e.channels.LoadAndDelete(sbx.GetUID())
	if loaded {
		ch := value.(channelSuite)
		close(ch.cancel)
		close(ch.start)
	}
}
