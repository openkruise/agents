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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// Inline security-rules limits. They bound the agents.kruise.io/security-rules
// annotation so neither the Sandbox object nor the data-plane compilation can
// be exhausted by a single create request.
const (
	maxSecurityRules            = 64
	maxSecurityRuleDomains      = 64
	maxHeaderManipulationSet    = 16
	maxHeaderManipulationRemove = 16
	maxSecurityRulesBytes       = 32 * 1024
	maxHeaderValueLength        = 2048
)

// headerNamePattern mirrors the CRD tchar subset used by AuditHeader.Name and
// HeaderValue.Name so inline rules can never persist a name the data plane
// would reject.
var headerNamePattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`)

// resolveSecurityRules produces the normalized agents.kruise.io/security-rules
// annotation value from the two exclusive input entries: the reserved
// e2b.agents.kruise.io/security-rules metadata key and the native E2B
// network.rules field. It returns "" when neither entry is used, which keeps
// requests without egress rules byte-identical to today's behavior.
func resolveSecurityRules(request *models.NewSandboxRequest) (string, error) {
	inlineRaw := request.Extensions.SecurityRulesRaw
	hasInline := request.Extensions.SecurityRulesPresent
	hasNetworkRules := request.Network != nil && len(request.Network.Rules) > 0
	if hasInline && hasNetworkRules {
		return "", fmt.Errorf("metadata key %q and network.rules are mutually exclusive: use exactly one entry for inline security rules",
			models.ExtensionKeySecurityRules)
	}
	if hasInline && inlineRaw == "" {
		return "", fmt.Errorf("metadata key %q must not be empty: provide a security-rules JSON array or omit the key",
			models.ExtensionKeySecurityRules)
	}

	var rules []agentsv1alpha1.SecurityRule
	switch {
	case hasInline:
		parsed, err := parseInlineSecurityRules(inlineRaw)
		if err != nil {
			return "", err
		}
		rules = parsed
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

	if err := validateInlineSecurityRules(rules); err != nil {
		return "", err
	}
	return marshalSecurityRules(rules)
}

// resolveSecurityRulesUpdate produces the replacement annotation value for a
// runtime network update. A nil rules map (field absent) keeps the existing
// chain and returns present=false; an explicit empty object, or rules with no
// effective transform, clears the chain; a non-empty map is translated and
// validated exactly like the creation path, including the whitelist-mode
// allowOut contract against the update's own allowOut list.
func resolveSecurityRulesUpdate(req *models.SandboxNetworkUpdateConfig) (rulesJSON string, present bool, err error) {
	if req.Rules == nil {
		return "", false, nil
	}
	if len(req.Rules) == 0 {
		return "", true, nil
	}
	translated, err := translateNetworkRules(&models.SandboxNetworkConfig{
		AllowOut: req.AllowOut,
		Rules:    req.Rules,
	})
	if err != nil {
		return "", false, err
	}
	if len(translated) == 0 {
		return "", true, nil
	}
	if err := validateInlineSecurityRules(translated); err != nil {
		return "", false, err
	}
	out, err := marshalSecurityRules(translated)
	if err != nil {
		return "", false, err
	}
	return out, true, nil
}

// parseInlineSecurityRules decodes the reserved metadata value strictly:
// unknown fields fail the request instead of being silently dropped, and the
// value must be exactly one JSON array with nothing but whitespace after it.
func parseInlineSecurityRules(raw string) ([]agentsv1alpha1.SecurityRule, error) {
	dec := json.NewDecoder(bytes.NewReader([]byte(raw)))
	dec.DisallowUnknownFields()
	var rules []agentsv1alpha1.SecurityRule
	if err := dec.Decode(&rules); err != nil {
		return nil, fmt.Errorf("metadata %q is not a valid security-rules JSON array: %v", models.ExtensionKeySecurityRules, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("metadata %q must contain exactly one security-rules JSON array value", models.ExtensionKeySecurityRules)
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("metadata %q must contain at least one security rule", models.ExtensionKeySecurityRules)
	}
	return rules, nil
}

// translateNetworkRules converts the native E2B network.rules field into
// inline security rules. Each domain with at least one header transform
// becomes one headerManipulation rule; E2B set/replace semantics map to
// headerManipulation.set. Domains are emitted in sorted order and headers are
// sorted by name so the persisted annotation is deterministic.
//
// In whitelist mode (a non-empty allowOut) a domain with an effective
// transform must also be listed in allowOut: the L7 rule only fires on
// traffic the L4 whitelist lets out, so a transform on an unreachable domain
// is a contract error the caller must see, not a rule that silently never
// runs. In open-egress mode (no allowOut) every domain is reachable and the
// check does not apply.
func translateNetworkRules(net *models.SandboxNetworkConfig) ([]agentsv1alpha1.SecurityRule, error) {
	domains := make([]string, 0, len(net.Rules))
	for domain := range net.Rules {
		domains = append(domains, domain)
	}
	sort.Strings(domains)

	whitelistMode := len(net.AllowOut) > 0
	allowed := make(map[string]struct{}, len(net.AllowOut))
	for _, entry := range net.AllowOut {
		allowed[strings.ToLower(entry)] = struct{}{}
	}

	rules := make([]agentsv1alpha1.SecurityRule, 0, len(domains))
	for _, domain := range domains {
		if domain == "" {
			return nil, fmt.Errorf("network.rules: empty domain key is not allowed")
		}
		if len(domain) > 253 {
			return nil, fmt.Errorf("network.rules[%q]: domain exceeds 253 characters", domain)
		}
		set, err := translateDomainTransforms(domain, net.Rules[domain])
		if err != nil {
			return nil, err
		}
		if len(set) == 0 {
			// A domain whose rules carry no effective transform needs no L7
			// rule; L4 allowOut/denyOut keep governing it.
			continue
		}
		if whitelistMode {
			if _, ok := allowed[strings.ToLower(domain)]; !ok {
				return nil, fmt.Errorf("network.rules[%q]: a domain with transform.headers must also be listed in network.allowOut", domain)
			}
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
// values for the same header, matching E2B set/replace semantics.
func translateDomainTransforms(domain string, domainRules []models.SandboxNetworkRule) ([]agentsv1alpha1.HeaderValue, error) {
	merged := map[string]agentsv1alpha1.HeaderValue{}
	for i, rule := range domainRules {
		if rule.EgressProxy != nil {
			return nil, fmt.Errorf("network.rules[%q][%d].egressProxy is not supported by the L7 egress policy engine", domain, i)
		}
		if rule.MaskRequestHost != nil {
			return nil, fmt.Errorf("network.rules[%q][%d].maskRequestHost is not supported by the L7 egress policy engine", domain, i)
		}
		if rule.Transform == nil {
			continue
		}
		for name, value := range rule.Transform.Headers {
			if err := validateHeaderAssignment(name, value); err != nil {
				return nil, fmt.Errorf("network.rules[%q][%d].transform.headers: %v", domain, i, err)
			}
			merged[strings.ToLower(name)] = agentsv1alpha1.HeaderValue{Name: name, Value: value}
		}
	}

	set := make([]agentsv1alpha1.HeaderValue, 0, len(merged))
	for _, hv := range merged {
		set = append(set, hv)
	}
	sort.Slice(set, func(i, j int) bool { return strings.ToLower(set[i].Name) < strings.ToLower(set[j].Name) })
	return set, nil
}

// securityRuleNameForDomain derives a deterministic rule name from a domain.
// Characters outside the CRD name alphabet are folded to '-' so wildcard and
// dotted domains always yield a valid name.
func securityRuleNameForDomain(domain string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(domain) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := "e2b-rules-" + strings.Trim(b.String(), "-")
	if len(name) > 253 {
		name = name[:253]
	}
	return name
}

// validateInlineSecurityRules enforces the inline action-set restrictions and
// size limits that apply to both input entries. Errors name the rule index so
// the HTTP response points at the offending entry.
func validateInlineSecurityRules(rules []agentsv1alpha1.SecurityRule) error {
	if len(rules) > maxSecurityRules {
		return fmt.Errorf("security-rules: %d rules exceed the maximum of %d", len(rules), maxSecurityRules)
	}
	for i := range rules {
		rule := &rules[i]
		if rule.Name == "" {
			return fmt.Errorf("security-rules[%d].name is required", i)
		}
		if len(rule.Name) > 253 {
			return fmt.Errorf("security-rules[%d].name exceeds 253 characters", i)
		}
		if len(rule.Match) == 0 {
			return fmt.Errorf("security-rules[%d].match must contain at least one entry", i)
		}
		for j := range rule.Match {
			if err := validateRuleMatch(i, j, &rule.Match[j]); err != nil {
				return err
			}
		}
		if err := validateRuleActions(i, &rule.Actions); err != nil {
			return err
		}
	}
	return nil
}

func validateRuleMatch(ruleIdx, matchIdx int, match *agentsv1alpha1.RuleMatch) error {
	path := fmt.Sprintf("security-rules[%d].match[%d]", ruleIdx, matchIdx)
	if len(match.Domains) == 0 {
		return fmt.Errorf("%s.domains is required", path)
	}
	if len(match.Domains) > maxSecurityRuleDomains {
		return fmt.Errorf("%s: %d domains exceed the maximum of %d", path, len(match.Domains), maxSecurityRuleDomains)
	}
	for _, domain := range match.Domains {
		if domain == "" {
			return fmt.Errorf("%s.domains contains an empty domain", path)
		}
	}
	for k := range match.Paths {
		p := &match.Paths[k]
		if p.Type == agentsv1alpha1.PathMatchTypeRegex {
			if _, err := regexp.Compile(p.Value); err != nil {
				return fmt.Errorf("%s.paths[%d] regex %q does not compile: %v", path, k, p.Value, err)
			}
		}
	}
	for k := range match.Headers {
		h := &match.Headers[k]
		if h.Type == agentsv1alpha1.StringMatchTypeRegex {
			if _, err := regexp.Compile(h.Value); err != nil {
				return fmt.Errorf("%s.headers[%d] regex %q does not compile: %v", path, k, h.Value, err)
			}
		}
	}
	for k := range match.QueryParams {
		q := &match.QueryParams[k]
		if q.Type == agentsv1alpha1.StringMatchTypeRegex {
			if _, err := regexp.Compile(q.Value); err != nil {
				return fmt.Errorf("%s.queryParams[%d] regex %q does not compile: %v", path, k, q.Value, err)
			}
		}
	}
	return nil
}

// validateRuleActions enforces the inline action set: only block and
// headerManipulation are accepted. bypass would short-circuit administrator
// baselines, and credential-backed or body-dependent actions need server-side
// materialization that the inline entry does not provide.
func validateRuleActions(ruleIdx int, actions *agentsv1alpha1.SecurityRuleActions) error {
	path := fmt.Sprintf("security-rules[%d].actions", ruleIdx)
	if actions.Bypass {
		return fmt.Errorf("%s.bypass: bypass is not allowed in inline rules", path)
	}
	if actions.TokenTransformation != nil {
		return fmt.Errorf("%s.tokenTransformation is not supported in inline rules", path)
	}
	if actions.MCPToolPolicy != nil {
		return fmt.Errorf("%s.mcpToolPolicy is not supported in inline rules", path)
	}
	if len(actions.Audit) > 0 {
		return fmt.Errorf("%s.audit is not supported in inline rules", path)
	}
	if actions.Block == nil && actions.HeaderManipulation == nil {
		return fmt.Errorf("%s: at least one of block or headerManipulation is required", path)
	}
	if actions.HeaderManipulation != nil {
		if err := validateHeaderManipulation(path+".headerManipulation", actions.HeaderManipulation); err != nil {
			return err
		}
	}
	return nil
}

func validateHeaderManipulation(path string, hm *agentsv1alpha1.HeaderManipulationAction) error {
	if len(hm.Set) == 0 && len(hm.Remove) == 0 {
		return fmt.Errorf("%s: at least one of set or remove must be specified", path)
	}
	if len(hm.Set) > maxHeaderManipulationSet {
		return fmt.Errorf("%s: %d set entries exceed the maximum of %d", path, len(hm.Set), maxHeaderManipulationSet)
	}
	if len(hm.Remove) > maxHeaderManipulationRemove {
		return fmt.Errorf("%s: %d remove entries exceed the maximum of %d", path, len(hm.Remove), maxHeaderManipulationRemove)
	}

	seen := make(map[string]string, len(hm.Set)+len(hm.Remove))
	for k := range hm.Set {
		hv := &hm.Set[k]
		if err := validateHeaderAssignment(hv.Name, hv.Value); err != nil {
			return fmt.Errorf("%s.set[%d]: %v", path, k, err)
		}
		lower := strings.ToLower(hv.Name)
		if prev, exists := seen[lower]; exists {
			if prev == "set" {
				return fmt.Errorf("%s: header %q appears more than once in set (names are case-insensitive)", path, hv.Name)
			}
			return fmt.Errorf("%s: header %q appears in both %s and set", path, hv.Name, prev)
		}
		seen[lower] = "set"
	}
	for k, name := range hm.Remove {
		if !headerNamePattern.MatchString(name) {
			return fmt.Errorf("%s.remove[%d]: header name %q is invalid", path, k, name)
		}
		if strings.EqualFold(name, "host") {
			return fmt.Errorf("%s.remove[%d]: Host cannot be modified", path, k)
		}
		lower := strings.ToLower(name)
		if prev, exists := seen[lower]; exists {
			return fmt.Errorf("%s: header %q appears in both %s and remove", path, name, prev)
		}
		seen[lower] = "remove"
	}
	return nil
}

func validateHeaderAssignment(name, value string) error {
	if !headerNamePattern.MatchString(name) {
		return fmt.Errorf("header name %q is invalid", name)
	}
	if strings.EqualFold(name, "host") {
		return fmt.Errorf("Host cannot be modified")
	}
	if len(value) > maxHeaderValueLength {
		return fmt.Errorf("header %q value exceeds %d characters", name, maxHeaderValueLength)
	}
	return nil
}

// marshalSecurityRules serializes the normalized rule chain. The annotation
// stores the server artifact, never the user's raw text.
func marshalSecurityRules(rules []agentsv1alpha1.SecurityRule) (string, error) {
	raw, err := json.Marshal(rules)
	if err != nil {
		return "", fmt.Errorf("marshal normalized security rules: %v", err)
	}
	if len(raw) > maxSecurityRulesBytes {
		return "", fmt.Errorf("security-rules: serialized rules exceed the maximum of %d bytes", maxSecurityRulesBytes)
	}
	return string(raw), nil
}
