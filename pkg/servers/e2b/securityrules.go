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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/validation"
)

// securityRuleNamePrefix marks server-generated rules translated from the
// native network.rules field, distinguishing them from metadata-authored ones.
const securityRuleNamePrefix = "e2b-rules-"

// rejectUnsupportedNetworkFeatures fails requests that carry the upstream
// top-level egressProxy / maskRequestHost fields. Neither is supported by
// the L7 egress policy engine, and dropping them silently would let callers
// believe proxying or host masking is in effect.
func rejectUnsupportedNetworkFeatures(egressProxy json.RawMessage, maskRequestHost *string) error {
	if len(egressProxy) > 0 && string(egressProxy) != "null" {
		return fmt.Errorf("network.egressProxy is not supported by the L7 egress policy engine")
	}
	if maskRequestHost != nil {
		return fmt.Errorf("network.maskRequestHost is not supported by the L7 egress policy engine")
	}
	return nil
}

// resolveSecurityRules produces the normalized agents.kruise.io/security-rules
// annotation value from the two exclusive input entries: the reserved
// e2b.agents.kruise.io/security-rules metadata key and the native E2B
// network.rules field. It returns "" when neither entry is used, which keeps
// requests without egress rules byte-identical to today's behavior.
func resolveSecurityRules(request *models.NewSandboxRequest) (string, error) {
	if request.Network != nil {
		if err := rejectUnsupportedNetworkFeatures(request.Network.EgressProxy, request.Network.MaskRequestHost); err != nil {
			return "", err
		}
	}
	inline := request.Extensions.SecurityRules
	hasInline := len(inline) > 0
	hasNetworkRules := request.Network != nil && len(request.Network.Rules) > 0
	if hasInline && hasNetworkRules {
		return "", fmt.Errorf("metadata key %q and network.rules are mutually exclusive: use exactly one entry for inline security rules",
			models.ExtensionKeySecurityRules)
	}

	var rules []agentsv1alpha1.SecurityRule
	switch {
	case hasInline:
		rules = inline
	case hasNetworkRules:
		translated, err := translateNetworkRules(request.Network)
		if err != nil {
			return "", err
		}
		if len(translated) == 0 {
			return "", nil
		}
		rules = translated
	default:
		return "", nil
	}

	return validation.NormalizeSecurityRules(rules)
}

// resolveSecurityRulesUpdate produces the replacement annotation value for a
// runtime network update. A nil rules map (field absent) keeps the existing
// chain and returns present=false; an explicit empty object, or rules with no
// effective transform, clears the chain; a non-empty map is translated and
// validated exactly like the creation path.
func resolveSecurityRulesUpdate(req *models.SandboxNetworkUpdateConfig) (rulesJSON string, present bool, err error) {
	if err := rejectUnsupportedNetworkFeatures(req.EgressProxy, req.MaskRequestHost); err != nil {
		return "", false, err
	}
	if req.Rules == nil {
		return "", false, nil
	}
	if len(req.Rules) == 0 {
		return "", true, nil
	}
	translated, err := translateNetworkRules(&models.SandboxNetworkConfig{
		Rules: req.Rules,
	})
	if err != nil {
		return "", false, err
	}
	if len(translated) == 0 {
		return "", true, nil
	}
	out, err := validation.NormalizeSecurityRules(translated)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// translateNetworkRules converts the native E2B network.rules field into
// inline security rules. Each domain with at least one header transform
// becomes one headerManipulation rule; E2B set/replace semantics map to
// headerManipulation.set. Domains are emitted in sorted order and headers are
// sorted by name so the persisted annotation is deterministic.
//
// Matching the upstream E2B contract, a rules domain does not grant egress
// and is not required to appear in allowOut: reachability may come from the
// request's own allowOut or from administrator-level traffic policy, and the
// L7 rule simply never fires on traffic the L4 layer does not let out.
func translateNetworkRules(net *models.SandboxNetworkConfig) ([]agentsv1alpha1.SecurityRule, error) {
	domains := make([]string, 0, len(net.Rules))
	for domain := range net.Rules {
		domains = append(domains, domain)
	}
	sort.Strings(domains)

	rules := make([]agentsv1alpha1.SecurityRule, 0, len(domains))
	// DNS names are case-insensitive, so case-variant keys are the same domain;
	// rejecting them here surfaces the input problem instead of a downstream
	// generated-name collision.
	seenDomains := make(map[string]string, len(domains))
	for _, domain := range domains {
		if domain == "" {
			return nil, fmt.Errorf("network.rules: empty domain key is not allowed")
		}
		if len(domain) > 253 {
			return nil, fmt.Errorf("network.rules[%q]: domain exceeds 253 characters", domain)
		}
		lower := strings.ToLower(domain)
		if prev, dup := seenDomains[lower]; dup {
			return nil, fmt.Errorf("network.rules: %q and %q are the same domain (domains are case-insensitive)",
				prev, domain)
		}
		seenDomains[lower] = domain
		set, err := translateDomainTransforms(domain, net.Rules[domain])
		if err != nil {
			return nil, err
		}
		if len(set) == 0 {
			// A domain whose rules carry no effective transform needs no L7
			// rule; L4 allowOut/denyOut keep governing it.
			continue
		}
		rules = append(rules, agentsv1alpha1.SecurityRule{
			Name: securityRuleNameForDomain(domain),
			Match: []agentsv1alpha1.RuleMatch{{
				Domains: []string{domain},
			}},
			Actions: agentsv1alpha1.SecurityRuleActions{
				HeaderManipulation: &agentsv1alpha1.HeaderManipulationAction{Set: set},
			},
		})
	}
	return rules, nil
}

// translateDomainTransforms merges every transform.headers map of one domain
// into a single headerManipulation.set list. Later rules replace earlier
// values for the same header, matching E2B set/replace semantics. Names are
// normalized to lowercase — HeaderValue.Name is lowercase-only in the CRD
// schema — and two case-variant keys inside one map are rejected because Go
// map iteration order would otherwise make the winner nondeterministic.
func translateDomainTransforms(domain string, domainRules []models.SandboxNetworkRule) ([]agentsv1alpha1.HeaderValue, error) {
	merged := map[string]agentsv1alpha1.HeaderValue{}
	for i, rule := range domainRules {
		if rule.Transform == nil {
			continue
		}
		inThisMap := make(map[string]string, len(rule.Transform.Headers))
		for name, value := range rule.Transform.Headers {
			if err := validation.ValidateHeaderAssignment(name, value); err != nil {
				return nil, fmt.Errorf("network.rules[%q][%d].transform.headers: %v", domain, i, err)
			}
			lower := strings.ToLower(name)
			if prev, dup := inThisMap[lower]; dup {
				return nil, fmt.Errorf("network.rules[%q][%d].transform.headers: %q and %q are the same header (names are case-insensitive)",
					domain, i, prev, name)
			}
			inThisMap[lower] = name
			merged[lower] = agentsv1alpha1.HeaderValue{Name: lower, Value: value}
		}
	}

	set := make([]agentsv1alpha1.HeaderValue, 0, len(merged))
	for _, hv := range merged {
		set = append(set, hv)
	}
	sort.Slice(set, func(i, j int) bool { return set[i].Name < set[j].Name })
	return set, nil
}

// securityRuleNameForDomain derives a deterministic rule name from a domain.
// Characters outside the CRD name alphabet are folded to '-' so wildcard and
// dotted domains always yield a valid name. Folding and truncation are lossy,
// so a short hash of the original (lowercased) domain is appended to keep
// names collision-free across distinct domains such as a_b.com and a-b.com.
func securityRuleNameForDomain(domain string) string {
	lower := strings.ToLower(domain)
	var b strings.Builder
	for _, r := range lower {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	sum := sha256.Sum256([]byte(lower))
	suffix := "-" + hex.EncodeToString(sum[:4])
	name := securityRuleNamePrefix + strings.Trim(b.String(), "-")
	if len(name)+len(suffix) > 253 {
		name = name[:253-len(suffix)]
	}
	return name + suffix
}
