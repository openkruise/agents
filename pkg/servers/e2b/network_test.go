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

package e2b

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestValidateDenyOut(t *testing.T) {
	tests := []struct {
		name        string
		denyOut     []string
		expectError string
	}{
		{
			name:        "valid CIDR entries",
			denyOut:     []string{"10.0.0.0/8", "192.168.1.0/24"},
			expectError: "",
		},
		{
			name:        "valid bare IP entries",
			denyOut:     []string{"8.8.8.8", "1.1.1.1"},
			expectError: "",
		},
		{
			name:        "valid mixed CIDR and IP",
			denyOut:     []string{"10.0.0.0/8", "8.8.8.8"},
			expectError: "",
		},
		{
			name:        "valid IPv6 CIDR",
			denyOut:     []string{"::1/128", "2001:db8::/32"},
			expectError: "",
		},
		{
			name:        "empty list is valid",
			denyOut:     []string{},
			expectError: "",
		},
		{
			name:        "nil list is valid",
			denyOut:     nil,
			expectError: "",
		},
		{
			name:        "plain domain rejected",
			denyOut:     []string{"example.com"},
			expectError: "domains are not supported in denyOut",
		},
		{
			name:        "wildcard domain rejected",
			denyOut:     []string{"*.example.com"},
			expectError: "domains are not supported in denyOut",
		},
		{
			name:        "multi-level domain rejected",
			denyOut:     []string{"api.openai.com"},
			expectError: "domains are not supported in denyOut",
		},
		{
			name:        "domain mixed with valid CIDR rejected",
			denyOut:     []string{"10.0.0.0/8", "evil.com"},
			expectError: "domains are not supported in denyOut",
		},
		{
			name:        "all-traffic CIDR is valid",
			denyOut:     []string{"0.0.0.0/0"},
			expectError: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDenyOut(tt.denyOut)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestValidateAndBuildNetworkConfig_DenyOutDomainError(t *testing.T) {
	// Validation is centralized in validateAndBuildNetworkConfig.
	_, err := validateAndBuildNetworkConfig(nil, &models.SandboxNetworkConfig{
		DenyOut: []string{"example.com"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "domains are not supported in denyOut")
}

func TestApplyAllowInternetAccess(t *testing.T) {
	falseVal := false
	trueVal := true

	tests := []struct {
		name                string
		allowInternetAccess *bool
		denyOut             []string
		wantDenyOut         []string
	}{
		{
			name:                "nil pointer: no change",
			allowInternetAccess: nil,
			denyOut:             []string{"10.0.0.0/8"},
			wantDenyOut:         []string{"10.0.0.0/8"},
		},
		{
			name:                "true: no change",
			allowInternetAccess: &trueVal,
			denyOut:             []string{"10.0.0.0/8"},
			wantDenyOut:         []string{"10.0.0.0/8"},
		},
		{
			name:                "false: adds 0.0.0.0/0",
			allowInternetAccess: &falseVal,
			denyOut:             []string{"10.0.0.0/8"},
			wantDenyOut:         []string{"10.0.0.0/8", "0.0.0.0/0"},
		},
		{
			name:                "false with empty denyOut: adds 0.0.0.0/0",
			allowInternetAccess: &falseVal,
			denyOut:             nil,
			wantDenyOut:         []string{"0.0.0.0/0"},
		},
		{
			name:                "false with existing 0.0.0.0/0: not duplicated",
			allowInternetAccess: &falseVal,
			denyOut:             []string{"0.0.0.0/0"},
			wantDenyOut:         []string{"0.0.0.0/0"},
		},
		{
			name:                "false with existing 0.0.0.0/0 among others: not duplicated",
			allowInternetAccess: &falseVal,
			denyOut:             []string{"10.0.0.0/8", "0.0.0.0/0", "8.8.8.8"},
			wantDenyOut:         []string{"10.0.0.0/8", "0.0.0.0/0", "8.8.8.8"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := applyAllowInternetAccess(tt.allowInternetAccess, tt.denyOut)
			assert.Equal(t, tt.wantDenyOut, got)
		})
	}
}

func TestValidateAndBuildNetworkConfig(t *testing.T) {
	falseVal := false
	trueVal := true

	tests := []struct {
		name                string
		allowInternetAccess *bool
		network             *models.SandboxNetworkConfig
		wantNil             bool
		wantAllow           []string
		wantDeny            []string
		expectError         string
	}{
		{
			name:                "nil allowInternetAccess, nil network: returns nil",
			allowInternetAccess: nil,
			network:             nil,
			wantNil:             true,
			expectError:         "",
		},
		{
			name:                "true allowInternetAccess, nil network: returns nil",
			allowInternetAccess: &trueVal,
			network:             nil,
			wantNil:             true,
			expectError:         "",
		},
		{
			name:                "nil allowInternetAccess, network with allowOut: returns as-is",
			allowInternetAccess: nil,
			network: &models.SandboxNetworkConfig{
				AllowOut: []string{"10.0.0.0/8"},
			},
			wantNil:     false,
			wantAllow:   []string{"10.0.0.0/8"},
			wantDeny:    nil,
			expectError: "",
		},
		{
			name:                "nil allowInternetAccess, network with denyOut: returns as-is",
			allowInternetAccess: nil,
			network: &models.SandboxNetworkConfig{
				DenyOut: []string{"10.0.0.0/8"},
			},
			wantNil:     false,
			wantAllow:   nil,
			wantDeny:    []string{"10.0.0.0/8"},
			expectError: "",
		},
		{
			name:                "false allowInternetAccess, nil network: creates config with 0.0.0.0/0",
			allowInternetAccess: &falseVal,
			network:             nil,
			wantNil:             false,
			wantDeny:            []string{"0.0.0.0/0"},
			expectError:         "",
		},
		{
			name:                "false allowInternetAccess, network with allowOut: merges 0.0.0.0/0 into denyOut",
			allowInternetAccess: &falseVal,
			network: &models.SandboxNetworkConfig{
				AllowOut: []string{"10.0.0.0/8"},
				DenyOut:  []string{"8.8.4.4"},
			},
			wantNil:     false,
			wantAllow:   []string{"10.0.0.0/8"},
			wantDeny:    []string{"8.8.4.4", "0.0.0.0/0"},
			expectError: "",
		},
		{
			name:                "false allowInternetAccess, network with existing 0.0.0.0/0: no duplicate",
			allowInternetAccess: &falseVal,
			network: &models.SandboxNetworkConfig{
				DenyOut: []string{"0.0.0.0/0"},
			},
			wantNil:     false,
			wantDeny:    []string{"0.0.0.0/0"},
			expectError: "",
		},
		{
			name:                "domain in denyOut rejected",
			allowInternetAccess: nil,
			network: &models.SandboxNetworkConfig{
				DenyOut: []string{"example.com"},
			},
			wantNil:     true,
			expectError: "domains are not supported in denyOut",
		},
		{
			name:                "wildcard domain in denyOut rejected",
			allowInternetAccess: nil,
			network: &models.SandboxNetworkConfig{
				DenyOut: []string{"*.evil.com"},
			},
			wantNil:     true,
			expectError: "domains are not supported in denyOut",
		},
		{
			name:                "false allowInternetAccess, domain in denyOut rejected",
			allowInternetAccess: &falseVal,
			network: &models.SandboxNetworkConfig{
				DenyOut: []string{"bad.com"},
			},
			wantNil:     true,
			expectError: "domains are not supported in denyOut",
		},
		{
			name:                "empty allowOut and denyOut with nil allowInternetAccess: returns nil",
			allowInternetAccess: nil,
			network: &models.SandboxNetworkConfig{
				AllowOut: []string{},
				DenyOut:  []string{},
			},
			wantNil:     true,
			expectError: "",
		},
		{
			name:                "mixed allowOut (bare IP + domain) and mixed denyOut (CIDR + bare IP): valid",
			allowInternetAccess: nil,
			network: &models.SandboxNetworkConfig{
				AllowOut: []string{"1.2.3.4", "api.example.com"},
				DenyOut:  []string{"10.0.0.0/8", "8.8.8.8"},
			},
			wantNil:     false,
			wantAllow:   []string{"1.2.3.4", "api.example.com"},
			wantDeny:    []string{"10.0.0.0/8", "8.8.8.8"},
			expectError: "",
		},
		{
			name:                "mixed allowOut (CIDR + wildcard domain) and mixed denyOut (CIDR + bare IP): valid",
			allowInternetAccess: nil,
			network: &models.SandboxNetworkConfig{
				AllowOut: []string{"192.168.1.0/24", "*.openai.com"},
				DenyOut:  []string{"172.16.0.0/12", "1.1.1.1"},
			},
			wantNil:     false,
			wantAllow:   []string{"192.168.1.0/24", "*.openai.com"},
			wantDeny:    []string{"172.16.0.0/12", "1.1.1.1"},
			expectError: "",
		},
		{
			name:                "mixed allowOut (IP + domain) and denyOut with domain: rejected",
			allowInternetAccess: nil,
			network: &models.SandboxNetworkConfig{
				AllowOut: []string{"1.2.3.4", "api.example.com"},
				DenyOut:  []string{"10.0.0.0/8", "evil.com"},
			},
			wantNil:     true,
			expectError: "domains are not supported in denyOut",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := validateAndBuildNetworkConfig(tt.allowInternetAccess, tt.network)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Nil(t, got)
				return
			}
			require.NoError(t, err)
			if tt.wantNil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.wantAllow, got.AllowOut)
			assert.Equal(t, tt.wantDeny, got.DenyOut)
		})
	}
}
