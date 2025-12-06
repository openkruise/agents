package sandbox_manager

import (
	"github.com/openkruise/agents/pkg/proxy"
)

type DebugInfo struct {
	Routes []proxy.Route
	Peers  []proxy.Peer
	Pools  map[string]any
}

func (m *SandboxManager) GetDebugInfo() DebugInfo {
	info := DebugInfo{
		Routes: m.proxy.ListRoutes(),
		Peers:  m.proxy.ListPeers(),
		Pools:  m.infra.LoadDebugInfo(),
	}
	return info
}
