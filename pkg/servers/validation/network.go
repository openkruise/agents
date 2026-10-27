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

// Package validation validates shared API network inputs and normalizes inline
// security rules. Protocol decoding and transport errors remain with each API.
package validation

import (
	"fmt"
	"strings"

	"github.com/openkruise/agents/pkg/utils/network"
)

// maxNetworkEntriesPerList caps entries per allowOut/denyOut list to prevent
// oversized TrafficPolicy CRs from exhausting apiserver resources.
const maxNetworkEntriesPerList = 100

// ValidateAllowOut checks that allowOut entries are valid CIDR, IP, or FQDN.
// Wildcard domains are not supported.
func ValidateAllowOut(allowOut []string) error {
	if len(allowOut) > maxNetworkEntriesPerList {
		return fmt.Errorf("allowOut list exceeds maximum of %d entries", maxNetworkEntriesPerList)
	}
	for _, entry := range allowOut {
		if strings.Contains(entry, "*") {
			return fmt.Errorf("invalid allowOut entry: %q wildcard domains are not supported, use a concrete domain instead", entry)
		}
		if !network.IsCIDROrIP(entry) && !network.IsFQDN(entry) {
			return fmt.Errorf("invalid allowOut entry: %q is not a valid CIDR, IP, or domain", entry)
		}
	}
	return nil
}

// ValidateDenyOut checks that all denyOut entries are valid CIDR or bare IP addresses.
func ValidateDenyOut(denyOut []string) error {
	if len(denyOut) > maxNetworkEntriesPerList {
		return fmt.Errorf("denyOut list exceeds maximum of %d entries", maxNetworkEntriesPerList)
	}
	for _, entry := range denyOut {
		if !network.IsCIDROrIP(entry) {
			return fmt.Errorf("domains are not supported in denyOut: %q is not a valid CIDR or IP address", entry)
		}
	}
	return nil
}
