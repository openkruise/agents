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

package validation

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// generateCIDREntries returns n unique valid CIDR entries for testing
// entry-count limits.
func generateCIDREntries(n int) []string {
	entries := make([]string, n)
	for i := range entries {
		entries[i] = fmt.Sprintf("10.%d.%d.0/24", i/256, i%256)
	}
	return entries
}

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
		{
			name:        "denyOut exceeds max entries",
			denyOut:     generateCIDREntries(maxNetworkEntriesPerList + 1),
			expectError: "denyOut list exceeds maximum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateDenyOut(tt.denyOut)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestValidateAllowOut(t *testing.T) {
	tests := []struct {
		name        string
		allowOut    []string
		expectError string
	}{
		{
			name:        "valid CIDR entries",
			allowOut:    []string{"10.0.0.0/8", "192.168.1.0/24"},
			expectError: "",
		},
		{
			name:        "valid bare IP entries",
			allowOut:    []string{"8.8.8.8", "1.1.1.1"},
			expectError: "",
		},
		{
			name:        "valid domain entries",
			allowOut:    []string{"example.com", "api.openai.com"},
			expectError: "",
		},
		{
			name:        "wildcard prefix rejected",
			allowOut:    []string{"*.example.com", "*.openai.com"},
			expectError: "wildcard domains are not supported",
		},
		{
			name:        "wildcard in middle rejected",
			allowOut:    []string{"api.*.github.com"},
			expectError: "wildcard domains are not supported",
		},
		{
			name:        "wildcard at end rejected",
			allowOut:    []string{"example.com.*"},
			expectError: "wildcard domains are not supported",
		},
		{
			name:        "wildcard without dot rejected",
			allowOut:    []string{"*example.com"},
			expectError: "wildcard domains are not supported",
		},
		{
			name:        "wildcard mixed with valid entries rejected",
			allowOut:    []string{"10.0.0.0/8", "8.8.8.8", "api.example.com", "*.github.com"},
			expectError: "wildcard domains are not supported",
		},
		{
			name:        "valid mixed CIDR IP and domain",
			allowOut:    []string{"10.0.0.0/8", "8.8.8.8", "api.example.com", "github.com"},
			expectError: "",
		},
		{
			name:        "empty list is valid",
			allowOut:    []string{},
			expectError: "",
		},
		{
			name:        "nil list is valid",
			allowOut:    nil,
			expectError: "",
		},
		{
			name:        "garbage string rejected",
			allowOut:    []string{">>>invalid"},
			expectError: "invalid allowOut entry",
		},
		{
			name:        "single label rejected",
			allowOut:    []string{"localhost"},
			expectError: "invalid allowOut entry",
		},
		{
			name:        "empty string rejected",
			allowOut:    []string{""},
			expectError: "invalid allowOut entry",
		},
		{
			name:        "TLD too short rejected",
			allowOut:    []string{"example.a"},
			expectError: "invalid allowOut entry",
		},
		{
			name:        "invalid entry mixed with valid rejected",
			allowOut:    []string{"10.0.0.0/8", ">>>bad"},
			expectError: "invalid allowOut entry",
		},
		{
			name:        "allowOut exceeds max entries",
			allowOut:    generateCIDREntries(maxNetworkEntriesPerList + 1),
			expectError: "allowOut list exceeds maximum",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAllowOut(tt.allowOut)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}
