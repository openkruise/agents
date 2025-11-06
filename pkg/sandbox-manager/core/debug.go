package core

import (
	"github.com/openkruise/agents/pkg/sandbox-manager/core/proxy"
)

type DebugInfo struct {
	Routes []proxy.Route
	Pools  map[string]any
}

func (m *SandboxManager) GetDebugInfo() DebugInfo {
	info := DebugInfo{
		Routes: m.proxy.ListRoutes(),
		Pools:  m.infra.LoadDebugInfo(),
	}
	return info
}
