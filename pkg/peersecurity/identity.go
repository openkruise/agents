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

package peersecurity

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"strings"
)

// parseAllowedClientCNs splits the raw flag or environment value the same way
// runtime splits --allowed-client-cns: an empty string is no restriction, and
// every comma-separated entry is kept verbatim.
func parseAllowedClientCNs(s string) []string {
	if s == "" {
		return []string{}
	}
	return strings.Split(s, ",")
}

// attachClientIdentityAllowlist installs a handshake check on the inbound peer
// TLS config. An empty value installs nothing, leaving authorization at the CA.
// VerifyConnection is used so a resumed session is still checked against the
// snapshot that is currently loaded.
func attachClientIdentityAllowlist(cfg *tls.Config, raw string) {
	allowed := parseAllowedClientCNs(raw)
	if len(allowed) == 0 {
		return
	}
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		return authorizePeerClient(cs, allowed)
	}
}

// authorizePeerClient admits a connection with no client certificate so Gateway
// health probes can complete the handshake. A presented certificate must already
// have been verified, and its leaf identity must match the allow-list.
func authorizePeerClient(cs tls.ConnectionState, allowed []string) error {
	if len(cs.PeerCertificates) == 0 {
		return nil
	}
	if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 {
		return fmt.Errorf("client certificate chain is empty")
	}
	leaf := cs.VerifiedChains[0][0]
	if clientIdentityAllowed(leaf, allowed) {
		return nil
	}
	return fmt.Errorf("client certificate identity is not allowed: CN=%q", leaf.Subject.CommonName)
}

// clientIdentityAllowed matches the leaf certificate's CN and DNS SANs against
// allowed. Empty entries never match, including a certificate with an empty CN.
func clientIdentityAllowed(leaf *x509.Certificate, allowed []string) bool {
	for _, name := range allowed {
		if name == "" {
			continue
		}
		if leaf.Subject.CommonName == name {
			return true
		}
		for _, dnsName := range leaf.DNSNames {
			if dnsName == name {
				return true
			}
		}
	}
	return false
}
