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

func TestInitOptionsQuotaDefaults(t *testing.T) {
	opts := InitOptions(SandboxManagerOptions{})
	assert.Equal(t, consts.DefaultQuotaRedisOperationTimeout, opts.Quota.OperationTimeout)
	assert.Equal(t, consts.DefaultQuotaRedisBreakerN, opts.Quota.BreakerN)
	assert.Equal(t, consts.DefaultQuotaRedisBreakerD, opts.Quota.BreakerD)
	assert.Equal(t, consts.DefaultQuotaAntiDriftInterval, opts.Quota.AntiDriftInterval)
	assert.Equal(t, consts.DefaultQuotaAntiDriftGrace, opts.Quota.AntiDriftGrace)
}

func TestInitOptions(t *testing.T) {
	tests := []struct {
		name                     string
		input                    SandboxManagerOptions
		expectSystemNamespace    string
		expectMaxClaimWorkers    int
		expectExtProcConcurrency uint32
		expectMaxCreateQPS       int
		expectMemberlistBindPort int
		expectEnableShortID      bool
		expectShortIDPrefix      string
		expectTrafficToken       TrafficAccessTokenOptions
	}{
		{
			name:                     "all empty fields should use defaults",
			input:                    SandboxManagerOptions{},
			expectSystemNamespace:    utils.DefaultSandboxDeployNamespace,
			expectMaxClaimWorkers:    consts.DefaultClaimWorkers,
			expectExtProcConcurrency: consts.DefaultExtProcConcurrency,
			expectMaxCreateQPS:       consts.DefaultCreateQPS,
			expectMemberlistBindPort: DefaultMemberlistBindPort,
			expectTrafficToken: TrafficAccessTokenOptions{
				Validity: DefaultTrafficAccessTokenValidity, MinValidity: DefaultTrafficAccessTokenMinValidity, MaxValidity: DefaultTrafficAccessTokenMaxValidity,
			},
		},
		{
			name: "all fields set should preserve values",
			input: SandboxManagerOptions{
				SystemNamespace:       "custom-namespace",
				MaxClaimWorkers:       100,
				ExtProcMaxConcurrency: 500,
				MaxCreateQPS:          20,
				MemberlistBindPort:    9000,
				TrafficAccessToken: TrafficAccessTokenOptions{
					Validity: 2 * time.Hour, MinValidity: time.Hour, MaxValidity: 3 * time.Hour,
				},
			},
			expectSystemNamespace:    "custom-namespace",
			expectMaxClaimWorkers:    100,
			expectExtProcConcurrency: 500,
			expectMaxCreateQPS:       20,
			expectMemberlistBindPort: 9000,
			expectTrafficToken: TrafficAccessTokenOptions{
				Validity: 2 * time.Hour, MinValidity: time.Hour, MaxValidity: 3 * time.Hour,
			},
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
		},
		{
			name: "non-configurable fields should be preserved",
			input: SandboxManagerOptions{
				PeerSelector:         "app=test",
				SandboxNamespace:     "sandbox-ns",
				SandboxLabelSelector: "env=prod",
				EnableShortSandboxID: true,
				ShortSandboxIDPrefix: "prod-",
			},
			expectSystemNamespace:    utils.DefaultSandboxDeployNamespace,
			expectMaxClaimWorkers:    consts.DefaultClaimWorkers,
			expectExtProcConcurrency: consts.DefaultExtProcConcurrency,
			expectMaxCreateQPS:       consts.DefaultCreateQPS,
			expectMemberlistBindPort: DefaultMemberlistBindPort,
			expectEnableShortID:      true,
			expectShortIDPrefix:      "prod-",
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
			assert.Equal(t, tt.expectEnableShortID, result.EnableShortSandboxID)
			assert.Equal(t, tt.expectShortIDPrefix, result.ShortSandboxIDPrefix)
			if tt.expectTrafficToken == (TrafficAccessTokenOptions{}) {
				assert.Equal(t, DefaultTrafficAccessTokenValidity, result.TrafficAccessToken.Validity)
				assert.Equal(t, DefaultTrafficAccessTokenMinValidity, result.TrafficAccessToken.MinValidity)
				assert.Equal(t, DefaultTrafficAccessTokenMaxValidity, result.TrafficAccessToken.MaxValidity)
			} else {
				assert.Equal(t, tt.expectTrafficToken, result.TrafficAccessToken)
			}

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
		})
	}
}

func TestValidateTrafficAccessTokenOptions(t *testing.T) {
	tests := []struct {
		name        string
		opts        TrafficAccessTokenOptions
		expectError string
	}{
		{name: "valid", opts: TrafficAccessTokenOptions{Validity: time.Hour, MinValidity: 5 * time.Minute, MaxValidity: 24 * time.Hour}},
		{name: "non-positive minimum", opts: TrafficAccessTokenOptions{Validity: time.Hour, MaxValidity: 24 * time.Hour}, expectError: "minimum validity"},
		{name: "maximum below minimum", opts: TrafficAccessTokenOptions{Validity: time.Hour, MinValidity: 2 * time.Hour, MaxValidity: time.Hour}, expectError: "maximum validity"},
		{name: "validity below minimum", opts: TrafficAccessTokenOptions{Validity: time.Minute, MinValidity: 5 * time.Minute, MaxValidity: time.Hour}, expectError: "must be between"},
		{name: "validity above maximum", opts: TrafficAccessTokenOptions{Validity: 2 * time.Hour, MinValidity: 5 * time.Minute, MaxValidity: time.Hour}, expectError: "must be between"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateTrafficAccessTokenOptions(tt.opts)
			if tt.expectError == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.expectError)
		})
	}
}
