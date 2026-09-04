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

package network

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListenAddress(t *testing.T) {
	tests := []struct {
		name string
		host string
		port int
		want string
	}{
		{name: "empty host listens on all addresses", port: 8080, want: ":8080"},
		{name: "configured host", host: "10.0.0.2", port: 8080, want: "10.0.0.2:8080"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, ListenAddress(tt.host, tt.port))
		})
	}
}

func TestResolveNetworkInterfaceAddress(t *testing.T) {
	tests := []struct {
		name    string
		iface   string
		wantErr string
	}{
		{name: "empty interface name resolves to empty address"},
		{name: "missing interface fails", iface: "definitely-missing-o1-interface", wantErr: "find network interface"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			address, err := ResolveNetworkInterfaceAddress(tt.iface)
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Empty(t, address)
		})
	}
}

func TestChooseInterfaceAddress(t *testing.T) {
	addr := func(value string) net.Addr {
		ip, ipNet, err := net.ParseCIDR(value)
		require.NoError(t, err)
		ipNet.IP = ip
		return ipNet
	}
	tests := []struct {
		name      string
		flags     net.Flags
		addrs     []net.Addr
		want      string
		wantError string
	}{
		{name: "down interface", addrs: []net.Addr{addr("10.0.0.2/24")}, wantError: "is down"},
		{name: "no address", flags: net.FlagUp, wantError: "has no global-unicast IPv4"},
		{name: "loopback and IPv6 are excluded", flags: net.FlagUp, addrs: []net.Addr{addr("127.0.0.1/8"), addr("2001:db8::1/64")}, wantError: "has no global-unicast IPv4"},
		{name: "multiple IPv4 addresses", flags: net.FlagUp, addrs: []net.Addr{addr("10.0.0.2/24"), addr("192.168.1.2/24")}, wantError: "has multiple global-unicast IPv4"},
		{name: "one IPv4 among ignored addresses", flags: net.FlagUp, addrs: []net.Addr{addr("127.0.0.1/8"), addr("10.0.0.2/24"), addr("2001:db8::1/64")}, want: "10.0.0.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := chooseInterfaceAddress("user0", tt.flags, tt.addrs)
			if tt.wantError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsCIDROrIP(t *testing.T) {
	tests := []struct {
		name   string
		entry  string
		expect bool
	}{
		{name: "IPv4 bare address", entry: "1.2.3.4", expect: true},
		{name: "IPv4 CIDR", entry: "10.0.0.0/8", expect: true},
		{name: "IPv6 bare address", entry: "::1", expect: true},
		{name: "IPv6 CIDR", entry: "fe80::/64", expect: true},
		{name: "domain is not CIDR or IP", entry: "api.example.com", expect: false},
		{name: "wildcard domain is not CIDR or IP", entry: "*.github.com", expect: false},
		{name: "empty string", entry: "", expect: false},
		{name: "invalid string", entry: "not-an-ip", expect: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, IsCIDROrIP(tt.entry))
		})
	}
}

func TestSplitAllowOut(t *testing.T) {
	tests := []struct {
		name         string
		allowOut     []string
		expectCIDRs  []string
		expectDomain []string
	}{
		{
			name:         "mixed CIDRs IPs and domains",
			allowOut:     []string{"1.2.3.4", "10.0.0.0/8", "api.example.com", "*.github.com"},
			expectCIDRs:  []string{"1.2.3.4/32", "10.0.0.0/8"},
			expectDomain: []string{"api.example.com", "*.github.com"},
		},
		{
			name:         "only domains",
			allowOut:     []string{"api.example.com", "*.github.com"},
			expectCIDRs:  nil,
			expectDomain: []string{"api.example.com", "*.github.com"},
		},
		{
			name:         "only CIDRs and IPs",
			allowOut:     []string{"1.2.3.4", "10.0.0.0/8"},
			expectCIDRs:  []string{"1.2.3.4/32", "10.0.0.0/8"},
			expectDomain: nil,
		},
		{
			name:         "empty input",
			allowOut:     nil,
			expectCIDRs:  nil,
			expectDomain: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cidrs, domains := SplitAllowOut(tt.allowOut)
			assert.Equal(t, tt.expectCIDRs, cidrs)
			assert.Equal(t, tt.expectDomain, domains)
		})
	}
}

func TestIsFQDN(t *testing.T) {
	tests := []struct {
		name   string
		entry  string
		expect bool
	}{
		{name: "simple domain", entry: "example.com", expect: true},
		{name: "multi-level domain", entry: "api.openai.com", expect: true},
		{name: "domain with hyphen", entry: "my-site.example.org", expect: true},
		{name: "deep nested domain", entry: "sub1.sub2.sub3.example.com", expect: true},
		{name: "wildcard domain not supported", entry: "*.example.com", expect: false},
		{name: "wildcard multi-level not supported", entry: "*.api.openai.com", expect: false},
		{name: "wildcard without dot is not FQDN", entry: "*example.com", expect: false},
		{name: "single label is not FQDN", entry: "localhost", expect: false},
		{name: "empty string", entry: "", expect: false},
		{name: "IP address is not FQDN", entry: "8.8.8.8", expect: false},
		{name: "CIDR is not FQDN", entry: "10.0.0.0/8", expect: false},
		{name: "TLD too short", entry: "example.a", expect: false},
		{name: "label starts with hyphen", entry: "-bad.example.com", expect: false},
		{name: "double dots", entry: "..bad.com", expect: false},
		{name: "garbage string", entry: ">>>invalid", expect: false},
		{name: "not-an-ip is not FQDN", entry: "not-an-ip", expect: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, IsFQDN(tt.entry))
		})
	}
}

func TestNormalizeToCIDR(t *testing.T) {
	tests := []struct {
		name   string
		entry  string
		expect string
	}{
		{name: "IPv4 bare address becomes /32", entry: "1.2.3.4", expect: "1.2.3.4/32"},
		{name: "IPv6 bare address becomes /128", entry: "::1", expect: "::1/128"},
		{name: "IPv4-mapped IPv6 becomes /128", entry: "::ffff:1.2.3.4", expect: "::ffff:1.2.3.4/128"},
		{name: "IPv6 full form becomes /128", entry: "2001:db8::1", expect: "2001:db8::1/128"},
		{name: "already CIDR v4 returned as-is", entry: "10.0.0.0/8", expect: "10.0.0.0/8"},
		{name: "already CIDR v6 returned as-is", entry: "fe80::/64", expect: "fe80::/64"},
		{name: "invalid string returned as-is", entry: "not-an-ip", expect: "not-an-ip"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, NormalizeToCIDR(tt.entry))
		})
	}
}
