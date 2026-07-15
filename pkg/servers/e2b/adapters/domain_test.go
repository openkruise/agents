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

package adapters

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNativeE2BAdapter_GetDomain(t *testing.T) {
	tests := []struct {
		name        string
		authority   string
		expect      string
		expectError string
	}{
		{
			name:      "strips api prefix and preserves port",
			authority: "api.example.com:8443",
			expect:    "example.com:8443",
		},
		{
			name:      "strips api prefix without port",
			authority: "api.example.com",
			expect:    "example.com",
		},
		{
			name:      "normalizes uppercase api prefix",
			authority: "API.example.com",
			expect:    "example.com",
		},
		{
			name:      "normalizes uppercase api prefix with port",
			authority: "API.example.com:8443",
			expect:    "example.com:8443",
		},
		{
			name:      "preserves host without api prefix",
			authority: "example.com",
			expect:    "example.com",
		},
		{
			name:      "removes trailing dot",
			authority: "api.example.com.",
			expect:    "example.com",
		},
		{
			name:      "removes trailing dot and preserves port",
			authority: "api.example.com.:8443",
			expect:    "example.com:8443",
		},
		{
			name:      "preserves bracketed ipv6 without port",
			authority: "[::1]",
			expect:    "[::1]",
		},
		{
			name:      "preserves bracketed ipv6 with port",
			authority: "[::1]:8443",
			expect:    "[::1]:8443",
		},
		{
			name:      "preserves localhost with port",
			authority: "localhost:7788",
			expect:    "localhost:7788",
		},
		{
			name:      "does not strip apiserver prefix",
			authority: "apiserver.example.com",
			expect:    "apiserver.example.com",
		},
		{
			name:        "empty host is rejected",
			expectError: "cannot resolve sandbox domain: empty host",
		},
		{
			name:        "api dot is rejected",
			authority:   "api.",
			expectError: "cannot resolve sandbox domain: empty host",
		},
		{
			name:        "api dot with port is rejected",
			authority:   "api.:8443",
			expectError: "cannot resolve sandbox domain: empty host",
		},
	}

	adapter := &NativeE2BAdapter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adapter.GetDomain(tt.authority)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestCustomizedE2BAdapter_GetDomain(t *testing.T) {
	tests := []struct {
		name        string
		authority   string
		expect      string
		expectError string
	}{
		{
			name:      "preserves host",
			authority: "gateway.example.com",
			expect:    "gateway.example.com",
		},
		{
			name:      "preserves host and port",
			authority: "gateway.example.com:8443",
			expect:    "gateway.example.com:8443",
		},
		{
			name:      "preserves api prefix",
			authority: "api.gateway.example.com",
			expect:    "api.gateway.example.com",
		},
		{
			name:      "preserves api prefix and port",
			authority: "api.gateway.example.com:8443",
			expect:    "api.gateway.example.com:8443",
		},
		{
			name:      "preserves case",
			authority: "Gateway.example.com",
			expect:    "Gateway.example.com",
		},
		{
			name:      "removes trailing dot",
			authority: "gateway.example.com.",
			expect:    "gateway.example.com",
		},
		{
			name:      "preserves case and port while removing trailing dot",
			authority: "Gateway.example.com.:8443",
			expect:    "Gateway.example.com:8443",
		},
		{
			name:        "empty host is rejected",
			expectError: "cannot resolve sandbox domain: empty host",
		},
		{
			name:        "empty host with port is rejected",
			authority:   ":8443",
			expectError: "cannot resolve sandbox domain: empty host",
		},
	}

	adapter := &CustomizedE2BAdapter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adapter.GetDomain(tt.authority)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestNativeE2BAdapter_GetSandboxAddress(t *testing.T) {
	tests := []struct {
		name      string
		domain    string
		sandboxID string
		port      int32
		expect    string
	}{
		{
			name:      "formats resolved domain as subdomain address",
			domain:    "example.com",
			sandboxID: "sid",
			port:      9222,
			expect:    "9222-sid.example.com",
		},
		{
			name:      "preserves resolved domain as-is",
			domain:    "API.Static.example.com.",
			sandboxID: "sid",
			port:      9222,
			expect:    "9222-sid.API.Static.example.com.",
		},
	}

	adapter := &NativeE2BAdapter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adapter.GetSandboxAddress(tt.domain, tt.sandboxID, tt.port)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestCustomizedE2BAdapter_GetSandboxAddress(t *testing.T) {
	tests := []struct {
		name      string
		domain    string
		sandboxID string
		port      int32
		expect    string
	}{
		{
			name:      "formats resolved domain as path address",
			domain:    "gateway.example.com",
			sandboxID: "sid",
			port:      9222,
			expect:    "gateway.example.com/kruise/sid/9222",
		},
		{
			name:      "preserves resolved domain as-is",
			domain:    "Gateway.example.com.",
			sandboxID: "sid",
			port:      9222,
			expect:    "Gateway.example.com./kruise/sid/9222",
		},
	}

	adapter := &CustomizedE2BAdapter{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adapter.GetSandboxAddress(tt.domain, tt.sandboxID, tt.port)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestE2BAdapter_GetDomain(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		path      string
		expect    string
	}{
		{
			name:      "native request without port returns base domain",
			authority: "api.example.com",
			path:      "/xxxx",
			expect:    "example.com",
		},
		{
			name:      "native request with port returns base domain and port",
			authority: "api.example.com:8443",
			path:      "/xxxx",
			expect:    "example.com:8443",
		},
		{
			name:      "customized request without port preserves authority",
			authority: "example.com",
			path:      "/kruise/api/xxxx",
			expect:    "example.com",
		},
		{
			name:      "customized request with port preserves authority",
			authority: "example.com:8443",
			path:      "/kruise/api/xxxx",
			expect:    "example.com:8443",
		},
	}

	adapter := NewE2BAdapter(8080)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := adapter.GetDomain(tt.authority, tt.path)
			require.NoError(t, err)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestE2BAdapter_GetSandboxAddress(t *testing.T) {
	tests := []struct {
		name   string
		domain string
		path   string
		expect string
	}{
		{
			name:   "native path selects native address formatter",
			domain: "example.com",
			path:   "/sandboxes/sid/connect",
			expect: "9222-sid.example.com",
		},
		{
			name:   "customized path selects customized address formatter",
			domain: "gateway.example.com",
			path:   "/kruise/api/sandboxes/sid/connect",
			expect: "gateway.example.com/kruise/sid/9222",
		},
	}

	adapter := NewE2BAdapter(8080)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := adapter.GetSandboxAddress(tt.domain, tt.path, "sid", 9222)
			assert.Equal(t, tt.expect, got)
		})
	}
}
