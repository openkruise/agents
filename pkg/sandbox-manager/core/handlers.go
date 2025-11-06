package core

import (
	"fmt"

	"github.com/openkruise/agents/pkg/sandbox-manager/core/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/core/proxy"
	"k8s.io/klog/v2"
)

func (m *SandboxManager) handleSandboxCreated(evt events.Event) error {
	log := klog.FromContext(evt.Context).WithValues("sandbox", klog.KObj(evt.Sandbox))
	_, ok := m.infra.GetPoolByObject(evt.Sandbox)
	if !ok {
		return fmt.Errorf("pool not found for sandbox %s", evt.Sandbox.GetName())
	}
	route := proxy.Route{
		ID:           evt.Sandbox.GetName(),
		IP:           evt.Sandbox.GetIP(),
		Owner:        evt.Sandbox.GetOwnerUser(),
		ExtraHeaders: evt.Sandbox.GetRouteHeader(),
	}
	m.proxy.SetRoute(evt.Sandbox.GetName(), route)
	state := evt.Sandbox.GetState()
	if state == "" {
		state = consts.SandboxStatePending
	}
	log.Info("sandbox ready", "route", route, "state", state)
	return evt.Sandbox.PatchLabels(evt.Context, map[string]string{
		consts.LabelSandboxID:    evt.Sandbox.GetName(),
		consts.LabelSandboxState: state,
	})
}

func (m *SandboxManager) handleSandboxKill(evt events.Event) error {
	log := klog.FromContext(evt.Context).WithValues("pod", klog.KObj(evt.Sandbox))
	log.Info("will kill sandbox for SandboxKill event")
	return m.killSandbox(evt.Context, evt.Sandbox)
}
