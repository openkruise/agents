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
	Peers []proxy.Peer
	Pools map[string]any
}

// DebugScope limits what GetDebugInfo reports. The caller decides what the
// requester is allowed to see; this package only applies the filter.
type DebugScope struct {
	// IncludeControlPlane adds Peers and Pools. Both describe the
	// sandbox-manager deployment rather than any caller's own sandboxes.
	IncludeControlPlane bool
}

func (m *SandboxManager) GetDebugInfo(scope DebugScope) DebugInfo {
	info := DebugInfo{}
	if scope.IncludeControlPlane {
		info.Peers = m.proxy.ListPeers()
		info.Pools = m.infra.LoadDebugInfo()
	}
	return info
}
