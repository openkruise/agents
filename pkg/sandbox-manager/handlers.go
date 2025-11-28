package sandbox_manager

import (
	"fmt"

	"github.com/openkruise/agents/pkg/sandbox-manager/events"
	"github.com/openkruise/agents/pkg/sandbox-manager/proxy"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/klog/v2"
)

// Deprecated: eventer will be removed recently for HA.
func (m *SandboxManager) handleSandboxCreated(evt events.Event) error {
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
	// TODO: remove SetRoute
	m.proxy.SetRoute(evt.Sandbox.GetName(), route)
	return utils.InitCreatedSandbox(evt.Context, evt.Sandbox)
}

func (m *SandboxManager) handleSandboxKill(evt events.Event) error {
	log := klog.FromContext(evt.Context).WithValues("pod", klog.KObj(evt.Sandbox))
	log.Info("will kill sandbox for SandboxKill event")
	return m.killSandbox(evt.Context, evt.Sandbox)
}
