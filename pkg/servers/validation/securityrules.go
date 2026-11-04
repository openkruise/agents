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
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
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

// NormalizeSecurityRules validates inline rules, applies action defaults, and
// serializes the annotation value. Empty input produces no annotation. Defaults
// are applied to the supplied rules, matching the native API behavior.
func NormalizeSecurityRules(rules []agentsv1alpha1.SecurityRule) (string, error) {
	if len(rules) == 0 {
		return "", nil
	}
	if err := validateInlineSecurityRules(rules); err != nil {
		return "", err
	}
	return marshalSecurityRules(rules)
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
		if err := ValidateHeaderAssignment(hv.Name, hv.Value); err != nil {
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

// ValidateHeaderAssignment checks a header name and value before protocol
// normalization or inline security-rule persistence.
func ValidateHeaderAssignment(name, value string) error {
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
