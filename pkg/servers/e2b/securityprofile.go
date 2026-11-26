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
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

const (
	securityProfileMetadataKey = agentsv1alpha1.E2BPrefix + "security-profile"
	securityProfileAnnotation  = agentsv1alpha1.InternalPrefix + "security-profile"
	legacyNetworkRulesConflict = "network.rules would write " + agentsv1alpha1.AnnotationSecurityRules + ", which conflicts with " + securityProfileAnnotation +
		". Enable --e2b-network-annotation and create a new sandbox to use network.rules with a native profile."
)

func parseInlineSecurityProfile(raw string) (string, error) {
	if len(raw) > maxSecurityRulesBytes {
		return "", fmt.Errorf("profile exceeds %d bytes", maxSecurityRulesBytes)
	}
	var profile *struct {
		Rules    []agentsv1alpha1.SecurityRule         `json:"rules,omitempty"`
		TCPRules []agentsv1alpha1.TCPRule              `json:"tcpRules,omitempty"`
		Inputs   []agentsv1alpha1.SecurityProfileInput `json:"inputs,omitempty"`
		Audit    []agentsv1alpha1.AuditAction          `json:"audit,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return "", fmt.Errorf("invalid profile: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", fmt.Errorf("expected one profile JSON object")
	}
	if profile == nil {
		return "", fmt.Errorf("expected one profile JSON object")
	}
	if len(profile.Rules) > maxSecurityRules {
		return "", fmt.Errorf("rules: at most %d rules are allowed", maxSecurityRules)
	}
	for _, rule := range profile.Rules {
		if strings.HasPrefix(rule.Name, "agentio-e2b-") {
			return "", fmt.Errorf("rules: name %q uses a reserved prefix", rule.Name)
		}
		if len(rule.Match) == 0 {
			return "", fmt.Errorf("rule %q requires a match", rule.Name)
		}
		for i, match := range rule.Match {
			if len(match.Domains) == 0 {
				return "", fmt.Errorf("rule %q match[%d] requires domains", rule.Name, i)
			}
			for _, domain := range match.Domains {
				name := strings.ToLower(strings.TrimSuffix(domain, "."))
				if name != "*" && len(validation.IsDNS1123Subdomain(strings.TrimPrefix(name, "*."))) > 0 {
					return "", fmt.Errorf("rule %q has invalid domain %q", rule.Name, domain)
				}
			}
		}
	}
	if err := validateInlineTCPRules(profile.TCPRules); err != nil {
		return "", err
	}
	// encoding/json decodes null into a zero TCPMatch, which means match all.
	// Check the wire value before persisting a profile containing an explicit null.
	if len(profile.TCPRules) > 0 && strings.Contains(raw, "null") {
		var wire struct {
			TCPRules []struct {
				Match []*struct{} `json:"match"`
			} `json:"tcpRules"`
		}
		if err := json.Unmarshal([]byte(raw), &wire); err != nil {
			return "", fmt.Errorf("invalid tcpRules: %w", err)
		}
		for i, rule := range wire.TCPRules {
			for j, match := range rule.Match {
				if match == nil {
					return "", fmt.Errorf("tcpRules[%d].match[%d]: expected an object", i, j)
				}
			}
		}
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		return "", fmt.Errorf("encode profile: %w", err)
	}
	if len(encoded) > maxSecurityRulesBytes {
		return "", fmt.Errorf("normalized profile exceeds %d bytes", maxSecurityRulesBytes)
	}
	return string(encoded), nil
}

func validateInlineTCPRules(rules []agentsv1alpha1.TCPRule) error {
	if len(rules) > maxSecurityRules {
		return fmt.Errorf("tcpRules: at most %d rules are allowed", maxSecurityRules)
	}
	names := make(map[string]struct{}, len(rules))
	for i, rule := range rules {
		if rule.Name == "" || len(rule.Name) > 253 || strings.HasPrefix(rule.Name, "agentio-e2b-") {
			return fmt.Errorf("tcpRules[%d]: invalid or reserved name %q", i, rule.Name)
		}
		if _, exists := names[rule.Name]; exists {
			return fmt.Errorf("tcpRules[%d]: duplicate name %q", i, rule.Name)
		}
		names[rule.Name] = struct{}{}
		if rule.Action != agentsv1alpha1.TCPActionAllow && rule.Action != agentsv1alpha1.TCPActionDeny {
			return fmt.Errorf("tcpRules[%d]: action must be Allow or Deny", i)
		}
		if len(rule.Match) == 0 {
			return fmt.Errorf("tcpRules[%d]: match must contain at least one clause", i)
		}
		for j, match := range rule.Match {
			for _, value := range match.CIDRs {
				prefix, err := netip.ParsePrefix(value)
				if err == nil && !prefix.Addr().Is4In6() {
					continue
				}
				addr, err := netip.ParseAddr(value)
				if err != nil || addr.Zone() != "" || addr.Is4In6() {
					return fmt.Errorf("tcpRules[%d].match[%d]: invalid IP or CIDR %q", i, j, value)
				}
			}
			for _, port := range match.Ports {
				if port < 1 || port > 65535 {
					return fmt.Errorf("tcpRules[%d].match[%d]: port %d is outside 1-65535", i, j, port)
				}
			}
		}
	}
	return nil
}
