package sandbox_manager

import (
	"context"
	errors2 "errors"
	"fmt"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/sandbox-manager/proxy"
	"k8s.io/klog/v2"
)

func (m *SandboxManager) SetTimer(ctx context.Context, sbx infra.Sandbox, afterSeconds int, event consts.EventType) error {
	if event == "" {
		return errors.NewError(errors.ErrorBadRequest, "event name can not be empty")
	}
	if afterSeconds <= 0 {
		return errors.NewError(errors.ErrorBadRequest, "afterSeconds must be greater than 0")
	}
	if err := sbx.SaveTimer(ctx, afterSeconds, event, false, ""); err != nil {
		return errors.NewError(errors.ErrorInternal, fmt.Sprintf("failed to persist timer to sandbox: %s", err.Error()))
	}
	m.setTimer(sbx, time.Duration(afterSeconds)*time.Second, event)
	return nil
}

func (m *SandboxManager) setTimer(sbx infra.Sandbox, after time.Duration, event consts.EventType) {
	key := timerKey(sbx, event)

	newTimer := time.AfterFunc(after, func() {
		ctx := logs.NewContext()
		m.handleTimer(ctx, sbx, event)
		// 定时器触发后从映射中删除
		m.timers.Delete(key)
	})

	if value, exists := m.timers.Swap(key, newTimer); exists {
		oldTimer := value.(*time.Timer)
		oldTimer.Stop()
	}
}

func (m *SandboxManager) handleTimer(ctx context.Context, sbx infra.Sandbox, eventType consts.EventType) {
	var err error
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "event", eventType)
	err = sbx.InplaceRefresh(true)
	if err != nil {
		log.Error(err, "Cannot trigger custom timer: failed to refresh sandbox")
		return
	}
	failures := m.eventer.Trigger(events.Event{
		Type:    eventType,
		Sandbox: sbx,
		Source:  "Timer",
		Message: "custom timer event triggered",
	})
	if failures > 0 {
		err = fmt.Errorf("%d custom timer event handler failed", failures)
		log.Error(err, "Not all handlers succeed")
	}
	var message string
	if err != nil {
		message = "This timer is triggered but failed to be handled"
	} else {
		message = "This timer is triggered and handled properly"
	}
	if err = sbx.SaveTimer(ctx, 0, eventType, true, message); err != nil {
		log.Error(err, "Failed to persist timer after triggered")
	}
}

func (m *SandboxManager) recoverFromCluster(ctx context.Context) error {
	log := klog.FromContext(ctx).V(DebugLevel)
	log.Info("recovering timers")
	sandboxes, err := m.infra.SelectSandboxes(infra.SandboxSelectorOptions{
		WantRunning:   true,
		WantAvailable: true,
		WantPaused:    true,
	})
	if err != nil {
		return err
	}
	var allErrors error
	for _, sbx := range sandboxes {
		// recover timers
		if err := sbx.LoadTimers(func(after time.Duration, eventType consts.EventType) {
			m.setTimer(sbx, after, eventType)
			log.Info("recovered timer", "sandbox", klog.KObj(sbx), "event", eventType, "after", after)
		}); err != nil {
			log.Error(err, "failed to recover timer on sandbox", "sandbox", klog.KObj(sbx))
			allErrors = errors2.Join(allErrors, err)
		}
		// recover routes
		if state := sbx.GetState(); state == agentsv1alpha1.SandboxStateRunning || state == agentsv1alpha1.SandboxStatePaused {
			route := proxy.Route{
				ID:           sbx.GetName(),
				IP:           sbx.GetIP(),
				Owner:        sbx.GetOwnerUser(),
				ExtraHeaders: sbx.GetRouteHeader(),
			}
			m.proxy.SetRoute(sbx.GetName(), route)
		}
	}
	return allErrors
}

func timerKey(sbx infra.Sandbox, event consts.EventType) string {
	return fmt.Sprintf("%s/%s/%s", sbx.GetNamespace(), sbx.GetName(), event)
}
