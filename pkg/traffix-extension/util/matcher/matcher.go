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

// Package matcher provides request matching logic against SecurityProfile rules.
package matcher

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// RequestInfo contains the extracted HTTP request attributes for matching.
//
// Host is the request authority with the ":port" suffix removed; Port
// captures the numeric port from that authority, or 0 when the client did
// not include one (e.g. relying on the scheme default).
//
// Path is the request path with any "?query" suffix stripped; Query holds
// the parsed query parameters. Path matchers run against Path only, never
// against the raw ":path".
type RequestInfo struct {
	// Host is the request authority with the ":port" suffix removed.
	Host string
	// Port is the numeric port suffix from the authority, or 0 when the
	// client did not include one.
	Port int32
	// Path is the request path WITHOUT any "?query" suffix.
	Path string
	// Query is the parsed query map. nil when the request had no query
	// string. Multi-value parameters retain all values; matchers consume
	// only the first occurrence per key.
	Query url.Values
	// Method is the HTTP method (:method pseudo-header).
	Method string
	// Headers is the lowercase-keyed request header map. Used for
	// HeaderMatch evaluation. Optional; nil is treated as "no headers".
	Headers map[string]string
}

// MatchRequest checks if the given request matches any rule in the profile.
// It returns the index of the first matching rule and true, or -1 and false if no match.
func MatchRequest(req RequestInfo, profile *v1alpha1.SecurityProfile) (int, bool) {
	for i, rule := range profile.Spec.Rules {
		if MatchesRule(req, rule) {
			return i, true
		}
	}
	return -1, false
}

// MatchesRule checks if the request matches a specific rule.
// A request matches a rule if it matches ANY of the conditions in the Match list.
func MatchesRule(req RequestInfo, rule v1alpha1.SecurityRule) bool {
	for _, matchCond := range rule.Match {
		if matchesCondition(req, matchCond) {
			return true
		}
	}
	return false
}

// matchesCondition checks if a request matches a single RuleMatch condition.
// All specified sub-conditions within a match condition must be satisfied (AND).
//
// Empty-field semantics:
//   - Domains: required (the CRD enforces MinItems=1). An empty list is
//     treated as "match nothing" defensively.
//   - Paths/Methods/Ports/Headers/QueryParams: empty list means "match
//     anything" for that dimension (the dimension is not constrained).
func matchesCondition(req RequestInfo, match v1alpha1.RuleMatch) bool {
	if !matchDomain(req.Host, match.Domains) {
		return false
	}

	if len(match.Paths) > 0 && !matchPath(req.Path, match.Paths) {
		return false
	}

	if len(match.Methods) > 0 && !matchMethod(req.Method, match.Methods) {
		return false
	}

	if len(match.Ports) > 0 && !matchPort(req.Port, match.Ports) {
		return false
	}

	if len(match.Headers) > 0 && !matchHeaders(req.Headers, match.Headers) {
		return false
	}

	if len(match.QueryParams) > 0 && !matchQueryParams(req.Query, match.QueryParams) {
		return false
	}

	return true
}

// matchDomain checks if the host matches any of the domain patterns.
// Returns false when the domain list is empty (defensive — the CRD enforces
// MinItems=1, but an unvalidated profile must not match every request).
//
// Supported patterns:
//   - "*"            — matches any host.
//   - "*.example.com" — wildcard prefix; matches "foo.example.com" and
//     "a.b.example.com" but not "example.com" itself.
//   - "api.example.com" — exact host match (case-insensitive).
func matchDomain(host string, domains []string) bool {
	if len(domains) == 0 {
		return false
	}
	for _, domain := range domains {
		if domain == "*" {
			return true
		}
		if strings.HasPrefix(domain, "*.") {
			// "*.example.com" -> require host ends with ".example.com".
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

// matchPath checks if the path matches any of the path conditions.
func matchPath(path string, paths []v1alpha1.PathMatch) bool {
	for _, pm := range paths {
		if matchSinglePath(path, pm) {
			return true
		}
	}
	return false
}

// matchSinglePath checks if the path matches a single PathMatch condition.
func matchSinglePath(path string, pm v1alpha1.PathMatch) bool {
	switch pm.Type {
	case v1alpha1.PathMatchTypeExact:
		return path == pm.Value
	case v1alpha1.PathMatchTypePrefix:
		return strings.HasPrefix(path, pm.Value)
	case v1alpha1.PathMatchTypeRegex:
		matched, err := regexp.MatchString(pm.Value, path)
		return err == nil && matched
	default:
		return false
	}
}

// matchMethod checks if the method matches any of the allowed methods (case-insensitive).
func matchMethod(method string, methods []string) bool {
	for _, m := range methods {
		if strings.EqualFold(method, m) {
			return true
		}
	}
	return false
}

// matchPort checks the request port against the allowed list. A request
// without an explicit port (Port==0) never matches a non-empty list — the
// caller can write a separate rule without Ports to also match the default.
func matchPort(port int32, ports []int32) bool {
	if port == 0 {
		return false
	}
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}

// matchStringValue dispatches a single string match against value using the
// requested match strategy. An invalid Regex pattern fails closed rather
// than erroring — the CRD validates length but cannot validate RE2 syntax,
// and we must not let a malformed rule crash the data path.
//
// An empty Type is treated as Exact, mirroring the CRD default. An unknown
// Type also fails closed.
func matchStringValue(value string, t v1alpha1.StringMatchType, operand string) bool {
	switch t {
	case "", v1alpha1.StringMatchTypeExact:
		return value == operand
	case v1alpha1.StringMatchTypePrefix:
		return strings.HasPrefix(value, operand)
	case v1alpha1.StringMatchTypeRegex:
		matched, err := regexp.MatchString(operand, value)
		return err == nil && matched
	default:
		return false
	}
}

// matchHeaders evaluates HeaderMatch entries; all must match (AND).
//
// Header names are case-insensitive; the lookup uses lowercase keys.
// reqHeaders is expected to already be lowercase-keyed (the request handler
// normalizes Envoy headers when building it).
func matchHeaders(reqHeaders map[string]string, headers []v1alpha1.HeaderMatch) bool {
	for _, h := range headers {
		val, ok := reqHeaders[strings.ToLower(h.Name)]
		if !ok {
			return false
		}
		if !matchStringValue(val, h.Type, h.Value) {
			return false
		}
	}
	return true
}

// matchQueryParams evaluates QueryParamMatch entries; all must match (AND).
//
// Lookup uses the case-sensitive key; the matched value is the FIRST entry
// for the key (url.Values.Get semantics). A missing key fails the match.
func matchQueryParams(query url.Values, params []v1alpha1.QueryParamMatch) bool {
	for _, q := range params {
		vals, ok := query[q.Name]
		if !ok || len(vals) == 0 {
			return false
		}
		if !matchStringValue(vals[0], q.Type, q.Value) {
			return false
		}
	}
	return true
}

// splitHostPort extracts (host, port) from a request authority. IPv6
// authorities in bracket form (e.g. "[::1]:8080") are returned without the
// brackets and the trailing port. A non-numeric or out-of-range port is
// returned as 0 (no match for any non-empty Ports list).
func splitHostPort(authority string) (string, int32) {
	if authority == "" {
		return "", 0
	}
	if authority[0] == '[' {
		end := strings.IndexByte(authority, ']')
		if end <= 0 {
			return authority, 0
		}
		host := authority[1:end]
		// Look for trailing ":port" after the closing bracket.
		if end+1 < len(authority) && authority[end+1] == ':' {
			return host, parsePort(authority[end+2:])
		}
		return host, 0
	}
	// Plain hostname or IPv4: split on the last colon. Using IndexByte is
	// unambiguous for non-bracketed authorities.
	if idx := strings.IndexByte(authority, ':'); idx >= 0 {
		return authority[:idx], parsePort(authority[idx+1:])
	}
	return authority, 0
}

// parsePort returns the port number, or 0 when the input is empty,
// non-numeric, or out of the [1, 65535] range.
func parsePort(s string) int32 {
	if s == "" {
		return 0
	}
	v, err := strconv.Atoi(s)
	if err != nil || v <= 0 || v > 65535 {
		return 0
	}
	return int32(v)
}

// splitPathAndQuery separates the "/path" and "query=string" halves of a
// raw ":path" pseudo-header. The returned query is the verbatim text after
// the first "?" (empty when the path has no query).
func splitPathAndQuery(rawPath string) (string, string) {
	if idx := strings.IndexByte(rawPath, '?'); idx >= 0 {
		return rawPath[:idx], rawPath[idx+1:]
	}
	return rawPath, ""
}

// ParseRequestInfo extracts RequestInfo from Envoy header values.
// Envoy sends pseudo-headers (:method, :path, :authority, :scheme) and the
// Host header. :authority is the gRPC/HTTP2 equivalent of Host.
//
// Splits performed here:
//   - :authority "host:port"  -> Host, Port
//   - :path "/p?q=1"          -> Path, Query
//
// Port inference: when :authority does not include an explicit port, the
// default port is taken from :scheme — http=80, https=443. This lets a rule
// that lists `ports: [80]` match plain `http://host/...` requests where the
// client did not spell out ":80". An explicit port in the authority always
// overrides the inference; an unrecognized scheme leaves Port=0.
//
// The headers map is captured in the returned RequestInfo so that header-
// based rule matching can use it; callers SHOULD pass a lowercase-keyed
// map (Envoy normalizes header names to lowercase per HTTP/2).
func ParseRequestInfo(headers map[string]string) RequestInfo {
	info := RequestInfo{Headers: headers}

	if auth, ok := headers[":authority"]; ok && auth != "" {
		info.Host, info.Port = splitHostPort(auth)
	} else if host, ok := headers["host"]; ok && host != "" {
		info.Host, info.Port = splitHostPort(host)
	}

	// Infer the port from :scheme when the authority omitted it.
	if info.Port == 0 {
		info.Port = inferPortFromScheme(headers[":scheme"])
	}

	if path, ok := headers[":path"]; ok {
		var rawQuery string
		info.Path, rawQuery = splitPathAndQuery(path)
		if rawQuery != "" {
			// ParseQuery returns parsed values even on partial errors; bad
			// pairs are silently dropped, which is acceptable for matching.
			info.Query, _ = url.ParseQuery(rawQuery)
		}
	}

	if method, ok := headers[":method"]; ok {
		info.Method = method
	}

	return info
}

// inferPortFromScheme returns the conventional default port for the scheme,
// or 0 when the scheme is empty or unrecognized.
func inferPortFromScheme(scheme string) int32 {
	switch strings.ToLower(scheme) {
	case "http":
		return 80
	case "https":
		return 443
	default:
		return 0
	}
}

// ParseHeaderValue extracts a header value from the Envoy headers.
// Handles multiple representations of the same header name.
func ParseHeaderValue(headers map[string]string, name string) (string, error) {
	if val, ok := headers[name]; ok {
		return val, nil
	}
	return "", fmt.Errorf("header %q not found", name)
}
