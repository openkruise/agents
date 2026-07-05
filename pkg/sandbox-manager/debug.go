/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

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
	routes := m.proxy.ListRoutes()
	// Mask AccessToken to prevent credential leakage via the debug endpoint.
	// Always set to "***" to avoid revealing which sandboxes have tokens configured.
	for i := range routes {
		routes[i].AccessToken = "***"
	}
	info := DebugInfo{
		Routes: routes,
		Peers:  m.proxy.ListPeers(),
		Pools:  m.infra.LoadDebugInfo(),
	}
	return info
}
