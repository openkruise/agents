/*
Copyright 2025.

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

package config

import (
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/stretchr/testify/assert"
)

func TestInitOptions(t *testing.T) {
	tests := []struct {
		name                     string
		input                    SandboxManagerOptions
		expectSystemNamespace    string
		expectMaxClaimWorkers    int
		expectExtProcConcurrency uint32
		expectMaxCreateQPS       int
		expectMemberlistBindPort int
		expectSingleflightTTL    time.Duration
	}{
		{
			name:                     "all empty fields should use defaults",
			input:                    SandboxManagerOptions{},
			expectSystemNamespace:    utils.DefaultSandboxDeployNamespace,
			expectMaxClaimWorkers:    consts.DefaultClaimWorkers,
			expectExtProcConcurrency: consts.DefaultExtProcConcurrency,
			expectMaxCreateQPS:       consts.DefaultCreateQPS,
			expectMemberlistBindPort: DefaultMemberlistBindPort,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "all fields set should preserve values",
			input: SandboxManagerOptions{
				SystemNamespace:                 "custom-namespace",
				MaxClaimWorkers:                 100,
				ExtProcMaxConcurrency:           500,
				MaxCreateQPS:                    20,
				MemberlistBindPort:              9000,
				SingleflightPreemptionThreshold: 2 * time.Minute,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    100,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       20,
			expectMemberlistBindPort: 9000,
			expectSingleflightTTL:    2 * time.Minute,
		},
		{
			name: "empty SystemNamespace should use default",
			input: SandboxManagerOptions{
				SystemNamespace:       "",
				MaxClaimWorkers:       100,
				ExtProcMaxConcurrency: 500,
				MaxCreateQPS:          20,
				MemberlistBindPort:    9000,
			},
			expectSystemNamespace:    utils.DefaultSandboxDeployNamespace,
			expectMaxClaimWorkers:    100,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       20,
			expectMemberlistBindPort: 9000,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "zero MaxClaimWorkers should use default",
			input: SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       0,
				ExtProcMaxConcurrency: 500,
				MaxCreateQPS:          20,
				MemberlistBindPort:    9000,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    consts.DefaultClaimWorkers,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       20,
			expectMemberlistBindPort: 9000,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "negative MaxClaimWorkers should use default",
			input: SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       -1,
				ExtProcMaxConcurrency: 500,
				MaxCreateQPS:          20,
				MemberlistBindPort:    9000,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    consts.DefaultClaimWorkers,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       20,
			expectMemberlistBindPort: 9000,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "zero ExtProcMaxConcurrency should use default",
			input: SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       100,
				ExtProcMaxConcurrency: 0,
				MaxCreateQPS:          20,
				MemberlistBindPort:    9000,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    100,
			expectExtProcConcurrency: consts.DefaultExtProcConcurrency,
			expectMaxCreateQPS:       20,
			expectMemberlistBindPort: 9000,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "zero MaxCreateQPS should use default",
			input: SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       100,
				ExtProcMaxConcurrency: 500,
				MaxCreateQPS:          0,
				MemberlistBindPort:    9000,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    100,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       consts.DefaultCreateQPS,
			expectMemberlistBindPort: 9000,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "negative MaxCreateQPS should use default",
			input: SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       100,
				ExtProcMaxConcurrency: 500,
				MaxCreateQPS:          -5,
				MemberlistBindPort:    9000,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    100,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       consts.DefaultCreateQPS,
			expectMemberlistBindPort: 9000,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "zero MemberlistBindPort should use default",
			input: SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       100,
				ExtProcMaxConcurrency: 500,
				MaxCreateQPS:          20,
				MemberlistBindPort:    0,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    100,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       20,
			expectMemberlistBindPort: DefaultMemberlistBindPort,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "negative MemberlistBindPort should use default",
			input: SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       100,
				ExtProcMaxConcurrency: 500,
				MaxCreateQPS:          20,
				MemberlistBindPort:    -1,
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    100,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       20,
			expectMemberlistBindPort: DefaultMemberlistBindPort,
			expectSingleflightTTL:    5 * time.Minute,
		},
		{
			name: "non-configurable fields should be preserved",
			input: SandboxManagerOptions{
				PeerSelector:               "app=test",
				SandboxNamespace:           "sandbox-ns",
				SandboxLabelSelector:       "env=prod",
				DisableRouteReconciliation: true,
			},
			expectSystemNamespace:    utils.DefaultSandboxDeployNamespace,
			expectMaxClaimWorkers:    consts.DefaultClaimWorkers,
			expectExtProcConcurrency: consts.DefaultExtProcConcurrency,
			expectMaxCreateQPS:       consts.DefaultCreateQPS,
			expectMemberlistBindPort: DefaultMemberlistBindPort,
			expectSingleflightTTL:    5 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := InitOptions(tt.input)

			assert.Equal(t, tt.expectSystemNamespace, result.SystemNamespace)
			assert.Equal(t, tt.expectMaxClaimWorkers, result.MaxClaimWorkers)
			assert.Equal(t, tt.expectExtProcConcurrency, result.ExtProcMaxConcurrency)
			assert.Equal(t, tt.expectMaxCreateQPS, result.MaxCreateQPS)
			assert.Equal(t, tt.expectMemberlistBindPort, result.MemberlistBindPort)
			assert.Equal(t, tt.expectSingleflightTTL, result.SingleflightPreemptionThreshold)

			// Verify non-configurable fields are preserved
			if tt.input.PeerSelector != "" {
				assert.Equal(t, tt.input.PeerSelector, result.PeerSelector)
			}
			if tt.input.SandboxNamespace != "" {
				assert.Equal(t, tt.input.SandboxNamespace, result.SandboxNamespace)
			}
			if tt.input.SandboxLabelSelector != "" {
				assert.Equal(t, tt.input.SandboxLabelSelector, result.SandboxLabelSelector)
			}
			if tt.input.DisableRouteReconciliation {
				assert.True(t, result.DisableRouteReconciliation)
			}
		})
	}
}
