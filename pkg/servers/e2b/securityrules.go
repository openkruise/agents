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
	"reflect"
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

// securityRuleNamePrefix marks server-generated rules translated from the
// native network.rules field, distinguishing them from metadata-authored ones.
const securityRuleNamePrefix = "e2b-rules-"

// headerNameSyntaxPattern is the RFC 7230 tchar subset accepted as header
// name syntax on both input surfaces. Persisted names are additionally
// lowercase-only: HeaderValue.Name and HeaderManipulationAction.Remove
// require lowercase in the CRD schema, so the native path normalizes with
// strings.ToLower and the metadata path rejects uppercase explicitly.
var headerNameSyntaxPattern = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+\-.^_|~]+$`)

// httpMethods is the CRD RuleMatch.Methods enum.
var httpMethods = map[string]struct{}{
	"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {},
	"DELETE": {}, "OPTIONS": {}, "CONNECT": {}, "TRACE": {},
}

// schemePattern is the CRD RuleMatch.Schemes item pattern.
var schemePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+\-.]*$`)

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

	if err := validateInlineSecurityRules(rules); err != nil {
		return "", err
	}
	return marshalSecurityRules(rules)
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
	if err := validateInlineSecurityRules(translated); err != nil {
		return "", false, err
	}
	out, err := marshalSecurityRules(translated)
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
			if err := validateHeaderAssignment(name, value); err != nil {
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

// validateInlineSecurityRules enforces the inline action-set restrictions and
// size limits that apply to both input entries, and fills the block
// statusCode default — the annotation bypasses the apiserver, so kubebuilder
// defaults never run for it. Errors name the rule index so the HTTP response
// points at the offending entry.
func validateInlineSecurityRules(rules []agentsv1alpha1.SecurityRule) error {
	if len(rules) > maxSecurityRules {
		return fmt.Errorf("security-rules: %d rules exceed the maximum of %d", len(rules), maxSecurityRules)
	}
	seenNames := make(map[string]int, len(rules))
	for i := range rules {
		rule := &rules[i]
		if rule.Name == "" {
			return fmt.Errorf("security-rules[%d].name is required", i)
		}
		if len(rule.Name) > 253 {
			return fmt.Errorf("security-rules[%d].name exceeds 253 characters", i)
		}
		if prev, dup := seenNames[rule.Name]; dup {
			return fmt.Errorf("security-rules[%d].name %q duplicates security-rules[%d].name", i, rule.Name, prev)
		}
		seenNames[rule.Name] = i
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

// validateRuleMatch re-applies the CRD RuleMatch schema constraints that the
// annotation path skips (kubebuilder markers only run in the apiserver): a
// value the CRD would reject must not reach the data-plane compiler through
// the annotation either.
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
		if len(domain) > 253 {
			return fmt.Errorf("%s.domains: %q exceeds 253 characters", path, domain)
		}
	}
	for k, method := range match.Methods {
		if _, ok := httpMethods[method]; !ok {
			return fmt.Errorf("%s.methods[%d]: %q is not a valid HTTP method", path, k, method)
		}
	}
	for k, port := range match.Ports {
		if port < 1 || port > 65535 {
			return fmt.Errorf("%s.ports[%d]: %d is outside 1-65535", path, k, port)
		}
	}
	for k, scheme := range match.Schemes {
		if len(scheme) > 32 || !schemePattern.MatchString(scheme) {
			return fmt.Errorf("%s.schemes[%d]: %q is not a valid scheme", path, k, scheme)
		}
	}
	for k := range match.Paths {
		p := &match.Paths[k]
		switch p.Type {
		// An empty type is valid: EPE applies the CRD default (Prefix) at
		// match time, mirroring what apiserver defaulting would have done.
		case "", agentsv1alpha1.PathMatchTypePrefix, agentsv1alpha1.PathMatchTypeExact, agentsv1alpha1.PathMatchTypeRegex:
		default:
			return fmt.Errorf("%s.paths[%d].type: %q is not one of Prefix, Exact, Regex", path, k, p.Type)
		}
		if p.Type == agentsv1alpha1.PathMatchTypeRegex {
			if _, err := regexp.Compile(p.Value); err != nil {
				return fmt.Errorf("%s.paths[%d] regex %q does not compile: %v", path, k, p.Value, err)
			}
		}
	}
	for k := range match.Headers {
		h := &match.Headers[k]
		if len(h.Name) > 256 || !headerNameSyntaxPattern.MatchString(h.Name) {
			return fmt.Errorf("%s.headers[%d]: header name %q is invalid", path, k, h.Name)
		}
		if h.Value == "" {
			return fmt.Errorf("%s.headers[%d]: value is required", path, k)
		}
		if err := validateStringMatchType(h.Type); err != nil {
			return fmt.Errorf("%s.headers[%d].type: %v", path, k, err)
		}
		if h.Type == agentsv1alpha1.StringMatchTypeRegex {
			if _, err := regexp.Compile(h.Value); err != nil {
				return fmt.Errorf("%s.headers[%d] regex %q does not compile: %v", path, k, h.Value, err)
			}
		}
	}
	for k := range match.QueryParams {
		q := &match.QueryParams[k]
		if err := validateStringMatchType(q.Type); err != nil {
			return fmt.Errorf("%s.queryParams[%d].type: %v", path, k, err)
		}
		if q.Type == agentsv1alpha1.StringMatchTypeRegex {
			if _, err := regexp.Compile(q.Value); err != nil {
				return fmt.Errorf("%s.queryParams[%d] regex %q does not compile: %v", path, k, q.Value, err)
			}
		}
	}
	return nil
}

// validateStringMatchType re-applies the CRD StringMatchType enum. An empty
// type is valid: EPE applies the CRD default (Exact) at match time, mirroring
// what apiserver defaulting would have done.
func validateStringMatchType(t agentsv1alpha1.StringMatchType) error {
	switch t {
	case "", agentsv1alpha1.StringMatchTypeExact, agentsv1alpha1.StringMatchTypePrefix, agentsv1alpha1.StringMatchTypeRegex:
		return nil
	default:
		return fmt.Errorf("%q is not one of Exact, Prefix, Regex", t)
	}
}

// validateRuleActions enforces the inline action allowlist: only block and
// headerManipulation are accepted. bypass would short-circuit administrator
// baselines, and credential-backed or body-dependent actions need server-side
// materialization that the inline entry does not provide. The final
// zero-value check rejects any action field this function does not know
// about, so a new SecurityRuleActions field is denied to tenants by default
// instead of leaking through an outdated blocklist.
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
	rest := *actions
	rest.Bypass = false
	rest.TokenTransformation = nil
	rest.MCPToolPolicy = nil
	rest.Audit = nil
	rest.Block = nil
	rest.HeaderManipulation = nil
	if !reflect.DeepEqual(rest, agentsv1alpha1.SecurityRuleActions{}) {
		return fmt.Errorf("%s: only block and headerManipulation are allowed in inline rules", path)
	}
	if actions.Block == nil && actions.HeaderManipulation == nil {
		return fmt.Errorf("%s: at least one of block or headerManipulation is required", path)
	}
	if actions.Block != nil {
		if actions.Block.StatusCode == 0 {
			// The CRD default (+kubebuilder:default:=403) only runs in the
			// apiserver; the annotation must carry the resolved value.
			actions.Block.StatusCode = 403
		}
		if actions.Block.StatusCode < 100 || actions.Block.StatusCode > 599 {
			return fmt.Errorf("%s.block.statusCode: %d is outside 100-599", path, actions.Block.StatusCode)
		}
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
		// HeaderValue.Name is lowercase-only in the CRD schema; the user
		// authored this value directly, so an explicit error is more honest
		// than silent normalization.
		if hv.Name != strings.ToLower(hv.Name) {
			return fmt.Errorf("%s.set[%d]: header name %q must be lowercase", path, k, hv.Name)
		}
		if prev, exists := seen[hv.Name]; exists {
			if prev == "set" {
				return fmt.Errorf("%s: header %q appears more than once in set (names are case-insensitive)", path, hv.Name)
			}
			return fmt.Errorf("%s: header %q appears in both %s and set", path, hv.Name, prev)
		}
		seen[hv.Name] = "set"
	}
	for k, name := range hm.Remove {
		if !headerNameSyntaxPattern.MatchString(name) {
			return fmt.Errorf("%s.remove[%d]: header name %q is invalid", path, k, name)
		}
		if name != strings.ToLower(name) {
			return fmt.Errorf("%s.remove[%d]: header name %q must be lowercase", path, k, name)
		}
		if name == "host" {
			return fmt.Errorf("%s.remove[%d]: Host cannot be modified", path, k)
		}
		if prev, exists := seen[name]; exists {
			return fmt.Errorf("%s: header %q appears in both %s and remove", path, name, prev)
		}
		seen[name] = "remove"
	}
	return nil
}

func validateHeaderAssignment(name, value string) error {
	if !headerNameSyntaxPattern.MatchString(name) {
		return fmt.Errorf("header name %q is invalid", name)
	}
	if strings.EqualFold(name, "host") {
		return fmt.Errorf("Host cannot be modified")
	}
	if len(value) > maxHeaderValueLength {
		return fmt.Errorf("header %q value exceeds %d characters", name, maxHeaderValueLength)
	}
	// The value is stored and injected verbatim; a control character (most
	// importantly CR/LF) would let a tenant smuggle additional headers into
	// the mutated request.
	for i := 0; i < len(value); i++ {
		if value[i] < 0x20 || value[i] == 0x7f {
			return fmt.Errorf("header %q value contains a control character at position %d", name, i)
		}
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
