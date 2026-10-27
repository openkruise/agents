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
	"context"
	"fmt"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// SelectSandboxNetworkPolicy reads the configured egress rules of a sandbox
// already admitted by GetSandbox. An absent instance policy has no rules.
func (m *SandboxManager) SelectSandboxNetworkPolicy(ctx context.Context, sbx infra.Sandbox) (infra.SandboxNetworkConfig, error) {
	config, err := sbx.SelectNetworkPolicy(ctx)
	if err != nil {
		return infra.SandboxNetworkConfig{}, fmt.Errorf("read sandbox network policy: %w", err)
	}
	if config == nil {
		return infra.SandboxNetworkConfig{}, nil
	}
	return *config, nil
}

// ReplaceSandboxNetworkPolicy replaces the rules, then reads the native
// configuration. A readback failure leaves any completed write in place.
func (m *SandboxManager) ReplaceSandboxNetworkPolicy(ctx context.Context, sbx infra.Sandbox, config infra.SandboxNetworkConfig) (infra.SandboxNetworkConfig, error) {
	if err := sbx.UpdateNetworkPolicy(ctx, config); err != nil {
		return infra.SandboxNetworkConfig{}, fmt.Errorf("replace sandbox network policy: %w", err)
	}
	return m.SelectSandboxNetworkPolicy(ctx, sbx)
}
