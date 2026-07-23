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

// Package network provides shared utilities for network CIDR/IP validation
// and normalization used by the e2b API layer and the sandbox-manager infra layer.
package network

import (
	"net"
	"regexp"
	"strings"
)

// IsCIDROrIP returns true if the entry is a valid CIDR or bare IP address.
func IsCIDROrIP(entry string) bool {
	if _, _, err := net.ParseCIDR(entry); err == nil {
		return true
	}
	return net.ParseIP(entry) != nil
}

// fqdnRegex matches FQDNs. Wildcards are not supported: the traffic-extension
// resolves FQDNs to IPs at runtime, and wildcards cannot resolve to a concrete IP.
var fqdnRegex = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,}$`)

// IsFQDN returns true if the entry is a valid FQDN. Wildcard domains are not supported.
func IsFQDN(entry string) bool {
	return fqdnRegex.MatchString(entry)
}

// SplitAllowOut separates allowOut entries into CIDR/IP entries and domain entries.
func SplitAllowOut(allowOut []string) (cidrs, domains []string) {
	for _, entry := range allowOut {
		if IsCIDROrIP(entry) {
			cidrs = append(cidrs, NormalizeToCIDR(entry))
		} else {
			domains = append(domains, entry)
		}
	}
	return cidrs, domains
}

// NormalizeToCIDR converts a bare IP to CIDR notation (/32 for IPv4, /128 for IPv6).
// Uses string notation (presence of ':') rather than To4() to correctly handle
// IPv4-mapped IPv6 addresses (e.g., "::ffff:1.2.3.4").
func NormalizeToCIDR(entry string) string {
	if _, _, err := net.ParseCIDR(entry); err == nil {
		return entry
	}
	if ip := net.ParseIP(entry); ip != nil {
		if !strings.Contains(entry, ":") {
			return entry + "/32"
		}
		return entry + "/128"
	}
	return entry
}
