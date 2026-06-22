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

// Package model defines internal value types shared across traffic-extension
// packages. SecurityProfile bundles a SecurityProfile API object with its
// pre-parsed label selector so the request hot path can skip selector
// re-parsing on every match.
package model

import (
	"net/url"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

// RequestInfo contains the extracted HTTP request attributes for matching.
type RequestInfo struct {
	Host    string
	Port    int32
	Path    string
	Query   url.Values
	Method  string
	Headers map[string]string
}

// pathMatcher is a pre-compiled path matcher for a single PathMatch entry.
type pathMatcher struct {
	Type  v1alpha1.PathMatchType
	Value string
	Re    *regexp.Regexp // non-nil only when Type == Regex
}

// stringMatcher is a pre-compiled string matcher for HeaderMatch / QueryParamMatch.
type stringMatcher struct {
	Name  string
	Type  v1alpha1.StringMatchType
	Value string
	Re    *regexp.Regexp // non-nil only when Type == Regex
}

// RuleMatch holds pre-compiled matching information for a single v1alpha1.RuleMatch.
// It encapsulates domain, path, method, port, header, and query-param matching
// with regexps compiled once at profile load time.
type RuleMatch struct {
	Domains     []string
	Paths       []pathMatcher
	Methods     []string
	Ports       []int32
	Headers     []stringMatcher
	QueryParams []stringMatcher
}

// CompileRuleMatch compiles a v1alpha1.RuleMatch into a model.RuleMatch with
// pre-compiled regexps. Invalid regex patterns are compiled as nil and the
// compilation error is logged; such matchers fail closed at match time.
func CompileRuleMatch(logger logr.Logger, raw v1alpha1.RuleMatch) RuleMatch {
	rm := RuleMatch{
		Domains: raw.Domains,
		Methods: raw.Methods,
		Ports:   raw.Ports,
	}

	for _, p := range raw.Paths {
		pm := pathMatcher{Type: p.Type, Value: p.Value}
		if p.Type == v1alpha1.PathMatchTypeRegex {
			re, err := regexp.Compile(p.Value)
			if err != nil {
				logger.Error(err, "Invalid path regex; matcher fails closed",
					"pattern", p.Value)
			}
			pm.Re = re
		}
		rm.Paths = append(rm.Paths, pm)
	}

	for _, h := range raw.Headers {
		sm := stringMatcher{Name: strings.ToLower(h.Name), Type: h.Type, Value: h.Value}
		if h.Type == v1alpha1.StringMatchTypeRegex {
			re, err := regexp.Compile(h.Value)
			if err != nil {
				logger.Error(err, "Invalid header regex; matcher fails closed",
					"header", h.Name, "pattern", h.Value)
			}
			sm.Re = re
		}
		rm.Headers = append(rm.Headers, sm)
	}

	for _, q := range raw.QueryParams {
		sm := stringMatcher{Name: q.Name, Type: q.Type, Value: q.Value}
		if q.Type == v1alpha1.StringMatchTypeRegex {
			re, err := regexp.Compile(q.Value)
			if err != nil {
				logger.Error(err, "Invalid queryParam regex; matcher fails closed",
					"queryParam", q.Name, "pattern", q.Value)
			}
			sm.Re = re
		}
		rm.QueryParams = append(rm.QueryParams, sm)
	}

	return rm
}

// Matches checks if the request matches this compiled RuleMatch condition.
// All specified sub-conditions must be satisfied (AND semantics).
func (rm *RuleMatch) Matches(req *RequestInfo) bool {
	if !rm.matchDomain(req.Host) {
		return false
	}
	if len(rm.Paths) > 0 && !rm.matchPath(req.Path) {
		return false
	}
	if len(rm.Methods) > 0 && !rm.matchMethod(req.Method) {
		return false
	}
	if len(rm.Ports) > 0 && !rm.matchPort(req.Port) {
		return false
	}
	if len(rm.Headers) > 0 && !rm.matchHeaders(req.Headers) {
		return false
	}
	if len(rm.QueryParams) > 0 && !rm.matchQueryParams(req.Query) {
		return false
	}
	return true
}

func (rm *RuleMatch) matchDomain(host string) bool {
	if len(rm.Domains) == 0 {
		return false
	}
	for _, domain := range rm.Domains {
		if domain == "*" {
			return true
		}
		if strings.HasPrefix(domain, "*.") {
			suffix := domain[1:]
			if len(host) > len(suffix) && strings.EqualFold(host[len(host)-len(suffix):], suffix) {
				return true
			}
			continue
		}
		if strings.EqualFold(host, domain) {
			return true
		}
	}
	return false
}

func (rm *RuleMatch) matchPath(path string) bool {
	for i := range rm.Paths {
		pm := &rm.Paths[i]
		switch pm.Type {
		case v1alpha1.PathMatchTypeExact:
			if path == pm.Value {
				return true
			}
		case v1alpha1.PathMatchTypePrefix:
			if strings.HasPrefix(path, pm.Value) {
				return true
			}
		case v1alpha1.PathMatchTypeRegex:
			if pm.Re != nil && pm.Re.MatchString(path) {
				return true
			}
		}
	}
	return false
}

func (rm *RuleMatch) matchMethod(method string) bool {
	for _, m := range rm.Methods {
		if strings.EqualFold(method, m) {
			return true
		}
	}
	return false
}

func (rm *RuleMatch) matchPort(port int32) bool {
	if port == 0 {
		return false
	}
	for _, p := range rm.Ports {
		if p == port {
			return true
		}
	}
	return false
}

func (rm *RuleMatch) matchHeaders(reqHeaders map[string]string) bool {
	for i := range rm.Headers {
		h := &rm.Headers[i]
		val, ok := reqHeaders[h.Name]
		if !ok {
			return false
		}
		if !h.matchValue(val) {
			return false
		}
	}
	return true
}

func (sm *stringMatcher) matchValue(val string) bool {
	switch sm.Type {
	case "", v1alpha1.StringMatchTypeExact:
		return val == sm.Value
	case v1alpha1.StringMatchTypePrefix:
		return strings.HasPrefix(val, sm.Value)
	case v1alpha1.StringMatchTypeRegex:
		if sm.Re != nil {
			return sm.Re.MatchString(val)
		}
		return false
	default:
		return false
	}
}

func (rm *RuleMatch) matchQueryParams(query url.Values) bool {
	for i := range rm.QueryParams {
		q := &rm.QueryParams[i]
		vals, ok := query[q.Name]
		if !ok || len(vals) == 0 {
			return false
		}
		if !q.matchValue(vals[0]) {
			return false
		}
	}
	return true
}

// SecurityRule holds pre-compiled match conditions for a single SecurityRule.
type SecurityRule struct {
	Name    string
	Actions v1alpha1.SecurityRuleActions
	Matches []RuleMatch
}

// MatchesRequest returns true if the request matches ANY of this rule's conditions.
func (cr *SecurityRule) MatchesRequest(req *RequestInfo) bool {
	for i := range cr.Matches {
		if cr.Matches[i].Matches(req) {
			return true
		}
	}
	return false
}

// CompileRules compiles all rules from a SecurityProfile spec into SecurityRules.
// Regex compilation errors encountered while compiling individual matches are
// logged via the provided logger.
func CompileRules(logger logr.Logger, rules []v1alpha1.SecurityRule) []SecurityRule {
	compiled := make([]SecurityRule, len(rules))
	for i, rule := range rules {
		ruleLogger := logger.WithValues("rule", rule.Name)
		cr := SecurityRule{
			Name:    rule.Name,
			Actions: rule.Actions,
		}
		for _, m := range rule.Match {
			cr.Matches = append(cr.Matches, CompileRuleMatch(ruleLogger, m))
		}
		compiled[i] = cr
	}
	return compiled
}

// SecurityProfile is the in-memory representation of a v1alpha1.SecurityProfile
// with its label selector parsed once at write time.
type SecurityProfile struct {
	Profile       *v1alpha1.SecurityProfile
	Selector      labels.Selector
	SecurityRules []SecurityRule // parallel to Profile.Spec.Rules
}

// NewSecurityProfile converts a v1alpha1.SecurityProfile into a model.SecurityProfile
// with a pre-parsed label selector and pre-compiled rule regexps. Regex
// compilation errors are logged via the provided logger; the matchers that
// failed to compile will fail closed at request time.
// Returns an error if the profile's selector is invalid.
func NewSecurityProfile(logger logr.Logger, profile *v1alpha1.SecurityProfile) (*SecurityProfile, error) {
	selector, err := metav1.LabelSelectorAsSelector(&profile.Spec.Selector)
	if err != nil {
		return nil, err
	}
	profileLogger := logger.WithValues(
		"profile", profile.Namespace+"/"+profile.Name)
	return &SecurityProfile{
		Profile:       profile,
		Selector:      selector,
		SecurityRules: CompileRules(profileLogger, profile.Spec.Rules),
	}, nil
}
