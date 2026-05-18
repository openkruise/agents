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

package matcher

import (
	"net/url"
	"testing"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func newProfileWithRules(rules []v1alpha1.SecurityRule) *v1alpha1.SecurityProfile {
	return &v1alpha1.SecurityProfile{
		Spec: v1alpha1.SecurityProfileSpec{
			Rules: rules,
		},
	}
}

func TestMatchRequest_DomainMatch(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{
		{
			Name: "test-rule",
			Match: []v1alpha1.RuleMatch{
				{
					Domains: []string{"api.openai.com", "api.anthropic.com"},
				},
			},
		},
	})

	tests := []struct {
		name  string
		host  string
		match bool
	}{
		{"exact match", "api.openai.com", true},
		{"second domain match", "api.anthropic.com", true},
		{"case insensitive", "API.OPENAI.COM", true},
		{"no match", "api.example.com", false},
		{"empty host", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RequestInfo{Host: tt.host}
			idx, matched := MatchRequest(req, profile)
			if matched != tt.match {
				t.Errorf("expected match=%v, got %v", tt.match, matched)
			}
			if matched && idx != 0 {
				t.Errorf("expected rule index 0, got %d", idx)
			}
		})
	}
}

func TestMatchRequest_PathMatch(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{
		{
			Name: "path-rule",
			Match: []v1alpha1.RuleMatch{
				{
					Domains: []string{"*"},
					Paths: []v1alpha1.PathMatch{
						{Type: v1alpha1.PathMatchTypePrefix, Value: "/v1/chat"},
						{Type: v1alpha1.PathMatchTypeExact, Value: "/health"},
					},
				},
			},
		},
	})

	tests := []struct {
		name  string
		path  string
		match bool
	}{
		{"prefix match", "/v1/chat/completions", true},
		{"prefix match exact boundary", "/v1/chat", true},
		{"exact match", "/health", true},
		{"no match", "/v2/models", false},
		{"similar but not prefix", "/v1/other", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RequestInfo{Host: "anything", Path: tt.path}
			_, matched := MatchRequest(req, profile)
			if matched != tt.match {
				t.Errorf("path %q: expected match=%v, got %v", tt.path, tt.match, matched)
			}
		})
	}
}

func TestMatchRequest_PathRegex(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{
		{
			Name: "regex-rule",
			Match: []v1alpha1.RuleMatch{
				{
					Domains: []string{"*"},
					Paths: []v1alpha1.PathMatch{
						{Type: v1alpha1.PathMatchTypeRegex, Value: `^/api/v\d+/users$`},
					},
				},
			},
		},
	})

	tests := []struct {
		name  string
		path  string
		match bool
	}{
		{"regex match v1", "/api/v1/users", true},
		{"regex match v2", "/api/v2/users", true},
		{"no match missing users", "/api/v1/groups", false},
		{"invalid path", "/api/users", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RequestInfo{Host: "anything", Path: tt.path}
			_, matched := MatchRequest(req, profile)
			if matched != tt.match {
				t.Errorf("path %q: expected match=%v, got %v", tt.path, tt.match, matched)
			}
		})
	}
}

func TestMatchRequest_MethodMatch(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{
		{
			Name: "method-rule",
			Match: []v1alpha1.RuleMatch{
				{
					Domains: []string{"*"},
					// CRD validation requires methods to be paired with paths;
					// add a permissive prefix so the test exercises method
					// matching without leaking other dimensions.
					Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/"}},
					Methods: []string{"POST", "PUT"},
				},
			},
		},
	})

	tests := []struct {
		name   string
		method string
		match  bool
	}{
		{"exact match POST", "POST", true},
		{"exact match PUT", "PUT", true},
		{"case insensitive", "post", true},
		{"no match", "GET", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RequestInfo{Host: "anything", Path: "/foo", Method: tt.method}
			_, matched := MatchRequest(req, profile)
			if matched != tt.match {
				t.Errorf("method %q: expected match=%v, got %v", tt.method, tt.match, matched)
			}
		})
	}
}

func TestMatchRequest_CombinedMatch(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{
		{
			Name: "combined-rule",
			Match: []v1alpha1.RuleMatch{
				{
					Domains: []string{"api.openai.com"},
					Paths: []v1alpha1.PathMatch{
						{Type: v1alpha1.PathMatchTypePrefix, Value: "/v1/"},
					},
					Methods: []string{"POST"},
				},
			},
		},
	})

	tests := []struct {
		name  string
		req   RequestInfo
		match bool
	}{
		{
			"all conditions met",
			RequestInfo{Host: "api.openai.com", Path: "/v1/chat/completions", Method: "POST"},
			true,
		},
		{
			"wrong domain",
			RequestInfo{Host: "api.example.com", Path: "/v1/chat", Method: "POST"},
			false,
		},
		{
			"wrong path",
			RequestInfo{Host: "api.openai.com", Path: "/v2/models", Method: "POST"},
			false,
		},
		{
			"wrong method",
			RequestInfo{Host: "api.openai.com", Path: "/v1/chat", Method: "GET"},
			false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, matched := MatchRequest(tt.req, profile)
			if matched != tt.match {
				t.Errorf("expected match=%v, got %v", tt.match, matched)
			}
		})
	}
}

func TestMatchRequest_MultipleMatchConditions(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{
		{
			Name: "multi-match-rule",
			Match: []v1alpha1.RuleMatch{
				{Domains: []string{"api.openai.com"}, Methods: []string{"POST"}},
				{Domains: []string{"api.anthropic.com"}, Methods: []string{"POST"}},
			},
		},
	})

	req1 := RequestInfo{Host: "api.openai.com", Method: "POST"}
	_, matched1 := MatchRequest(req1, profile)
	if !matched1 {
		t.Error("expected first condition to match")
	}

	req2 := RequestInfo{Host: "api.anthropic.com", Method: "POST"}
	_, matched2 := MatchRequest(req2, profile)
	if !matched2 {
		t.Error("expected second condition to match")
	}

	req3 := RequestInfo{Host: "api.example.com", Method: "POST"}
	_, matched3 := MatchRequest(req3, profile)
	if matched3 {
		t.Error("expected no match")
	}
}

func TestMatchRequest_MultipleRules(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{
		{
			Name: "first-rule",
			Match: []v1alpha1.RuleMatch{
				{Domains: []string{"api.openai.com"}},
			},
		},
		{
			Name: "second-rule",
			Match: []v1alpha1.RuleMatch{
				{Domains: []string{"api.anthropic.com"}},
			},
		},
	})

	req := RequestInfo{Host: "api.anthropic.com"}
	idx, matched := MatchRequest(req, profile)
	if !matched {
		t.Error("expected match on second rule")
	}
	if idx != 1 {
		t.Errorf("expected rule index 1, got %d", idx)
	}
}

func TestParseRequestInfo(t *testing.T) {
	h1 := map[string]string{":authority": "api.openai.com", ":path": "/v1/chat", ":method": "POST"}
	info1 := ParseRequestInfo(h1)
	if info1.Host != "api.openai.com" {
		t.Errorf("expected host 'api.openai.com', got %q", info1.Host)
	}
	if info1.Path != "/v1/chat" {
		t.Errorf("expected path '/v1/chat', got %q", info1.Path)
	}
	if info1.Method != "POST" {
		t.Errorf("expected method 'POST', got %q", info1.Method)
	}

	h2 := map[string]string{"host": "example.com", ":path": "/api"}
	info2 := ParseRequestInfo(h2)
	if info2.Host != "example.com" {
		t.Errorf("expected host 'example.com', got %q", info2.Host)
	}
}

func TestMatchRequest_NoRules(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{})
	req := RequestInfo{Host: "anything"}
	_, matched := MatchRequest(req, profile)
	if matched {
		t.Error("expected no match with empty rules")
	}
}

// TestMatchDomain_Wildcards covers the special "*" / "*.example.com" patterns.
func TestMatchDomain_Wildcards(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		domains []string
		want    bool
	}{
		{"empty domains never match", "api.example.com", nil, false},
		{"empty domains never match (zero len)", "api.example.com", []string{}, false},
		{"global wildcard matches anything", "anything.test", []string{"*"}, true},
		{"global wildcard matches empty host", "", []string{"*"}, true},

		{"prefix wildcard matches subdomain", "foo.example.com", []string{"*.example.com"}, true},
		{"prefix wildcard matches deep subdomain", "a.b.example.com", []string{"*.example.com"}, true},
		{"prefix wildcard does NOT match apex", "example.com", []string{"*.example.com"}, false},
		{"prefix wildcard does NOT match unrelated", "fooexample.com", []string{"*.example.com"}, false},
		{"prefix wildcard is case-insensitive", "FOO.EXAMPLE.COM", []string{"*.example.com"}, true},

		{"exact host case insensitive", "API.OPENAI.COM", []string{"api.openai.com"}, true},
		{"any of multiple domains", "api.x.com", []string{"a.com", "api.x.com"}, true},
		{"none of multiple domains", "api.z.com", []string{"a.com", "api.x.com"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchDomain(tt.host, tt.domains)
			if got != tt.want {
				t.Errorf("matchDomain(%q, %v) = %v, want %v", tt.host, tt.domains, got, tt.want)
			}
		})
	}
}

// TestSplitHostPort exercises the (host, port) extractor, including IPv6.
func TestSplitHostPort(t *testing.T) {
	tests := []struct {
		in       string
		wantHost string
		wantPort int32
	}{
		{"", "", 0},
		{"example.com", "example.com", 0},
		{"example.com:8080", "example.com", 8080},
		{"example.com:0", "example.com", 0},        // port=0 invalid → 0
		{"example.com:99999", "example.com", 0},    // out of range → 0
		{"example.com:abc", "example.com", 0},      // non-numeric → 0
		{"127.0.0.1:443", "127.0.0.1", 443},
		{"[::1]:8080", "::1", 8080},
		{"[2001:db8::1]:443", "2001:db8::1", 443},
		{"[::1]", "::1", 0},                        // bracket form, no port
		{"[unclosed", "[unclosed", 0},              // malformed: returned unchanged
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			gotHost, gotPort := splitHostPort(tt.in)
			if gotHost != tt.wantHost || gotPort != tt.wantPort {
				t.Errorf("splitHostPort(%q) = (%q,%d), want (%q,%d)",
					tt.in, gotHost, gotPort, tt.wantHost, tt.wantPort)
			}
		})
	}
}

// TestSplitPathAndQuery covers the path/query splitter.
func TestSplitPathAndQuery(t *testing.T) {
	tests := []struct {
		in, wantPath, wantQuery string
	}{
		{"", "", ""},
		{"/foo", "/foo", ""},
		{"/foo?x=1", "/foo", "x=1"},
		{"/foo?x=1&y=2", "/foo", "x=1&y=2"},
		{"/foo?", "/foo", ""},
		{"?onlyquery", "", "onlyquery"},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			p, q := splitPathAndQuery(tt.in)
			if p != tt.wantPath || q != tt.wantQuery {
				t.Errorf("splitPathAndQuery(%q) = (%q,%q), want (%q,%q)",
					tt.in, p, q, tt.wantPath, tt.wantQuery)
			}
		})
	}
}

// TestParseRequestInfo_SplitsHostAndPath verifies the front-door parser splits
// ":authority" into Host+Port and ":path" into Path+Query.
func TestParseRequestInfo_SplitsHostAndPath(t *testing.T) {
	info := ParseRequestInfo(map[string]string{
		":authority": "api.example.com:8443",
		":path":      "/v1/chat?x=1&y=2",
		":method":    "POST",
	})
	if info.Host != "api.example.com" {
		t.Errorf("expected host=api.example.com, got %q", info.Host)
	}
	if info.Port != 8443 {
		t.Errorf("expected port=8443, got %d", info.Port)
	}
	if info.Path != "/v1/chat" {
		t.Errorf("path should NOT include query, got %q", info.Path)
	}
	if info.Query.Get("x") != "1" || info.Query.Get("y") != "2" {
		t.Errorf("expected x=1 y=2, got %v", info.Query)
	}

	// Falls back to "host" header when :authority is absent.
	info2 := ParseRequestInfo(map[string]string{
		"host":    "fallback.example.com:1234",
		":path":   "/p",
		":method": "GET",
	})
	if info2.Host != "fallback.example.com" || info2.Port != 1234 {
		t.Errorf("expected host=fallback.example.com:1234, got %q:%d", info2.Host, info2.Port)
	}
	if info2.Query != nil {
		t.Errorf("expected nil query for path with no '?', got %v", info2.Query)
	}

	// Empty headers map yields zero RequestInfo (with non-nil Headers map).
	info3 := ParseRequestInfo(map[string]string{})
	if info3.Host != "" || info3.Path != "" || info3.Method != "" || info3.Port != 0 {
		t.Errorf("expected empty info, got %+v", info3)
	}
}

// TestMatchRequest_HeadersAND covers HeaderMatch evaluation: multiple
// HeaderMatch entries must all match (AND), case-insensitive name lookup,
// and missing-header / bad-pattern paths.
func TestMatchRequest_HeadersAND(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{
		{
			Name: "headers-rule",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"*"},
				Headers: []v1alpha1.HeaderMatch{
					{Name: "X-Tenant", Type: v1alpha1.StringMatchTypeRegex, Value: "^acme-"},
					{Name: "Authorization", Type: v1alpha1.StringMatchTypeRegex, Value: "^Bearer "},
				},
			}},
		},
	})

	tests := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{
			"all headers match",
			map[string]string{"x-tenant": "acme-prod", "authorization": "Bearer abc"},
			true,
		},
		{
			"one header missing",
			map[string]string{"x-tenant": "acme-prod"},
			false,
		},
		{
			"one header pattern fails",
			map[string]string{"x-tenant": "other-prod", "authorization": "Bearer abc"},
			false,
		},
		{
			"name lookup is case-insensitive (rule uses X-Tenant, request lower)",
			map[string]string{"x-tenant": "acme-staging", "authorization": "Bearer xyz"},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RequestInfo{Host: "example.com", Headers: tt.headers}
			_, matched := MatchRequest(req, profile)
			if matched != tt.want {
				t.Errorf("got %v, want %v", matched, tt.want)
			}
		})
	}
}

// TestMatchRequest_Ports covers Ports list matching: the port list is ORed,
// missing port (Port==0) never matches a non-empty list, and an empty list
// is treated as "unconstrained".
func TestMatchRequest_Ports(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{{
		Name:  "ports-rule",
		Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}, Ports: []int32{443, 8443}}},
	}})

	tests := []struct {
		name string
		port int32
		want bool
	}{
		{"first port matches", 443, true},
		{"second port matches", 8443, true},
		{"unrelated port does not match", 8080, false},
		{"missing port (0) does not match", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RequestInfo{Host: "example.com", Port: tt.port}
			_, matched := MatchRequest(req, profile)
			if matched != tt.want {
				t.Errorf("got %v, want %v", matched, tt.want)
			}
		})
	}
}

// TestParseRequestInfo_InfersPortFromScheme verifies that an authority
// without an explicit port falls back to the scheme's default (80/443),
// while an unrecognized or missing scheme yields Port=0.
func TestParseRequestInfo_InfersPortFromScheme(t *testing.T) {
	tests := []struct {
		name      string
		authority string
		scheme    string
		wantPort  int32
	}{
		{"http scheme infers 80", "api.example.com", "http", 80},
		{"HTTPS scheme infers 443 (case-insensitive)", "api.example.com", "HTTPS", 443},
		{"explicit port overrides http inference", "api.example.com:8080", "http", 8080},
		{"explicit port overrides https inference", "api.example.com:9443", "https", 9443},
		{"unknown scheme leaves Port=0", "api.example.com", "ftp", 0},
		{"missing scheme leaves Port=0", "api.example.com", "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := map[string]string{
				":authority": tt.authority,
				":path":      "/",
				":method":    "GET",
			}
			if tt.scheme != "" {
				h[":scheme"] = tt.scheme
			}
			info := ParseRequestInfo(h)
			if info.Port != tt.wantPort {
				t.Errorf("got Port=%d, want %d", info.Port, tt.wantPort)
			}
		})
	}
}

// TestInferPortFromScheme exercises the helper directly.
func TestInferPortFromScheme(t *testing.T) {
	cases := map[string]int32{
		"http":  80,
		"HTTP":  80,
		"https": 443,
		"Https": 443,
		"ftp":   0,
		"":      0,
	}
	for in, want := range cases {
		if got := inferPortFromScheme(in); got != want {
			t.Errorf("inferPortFromScheme(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestMatchPort_EmptyList verifies the empty Ports list semantics directly.
func TestMatchPort_EmptyList(t *testing.T) {
	if !matchesCondition(
		RequestInfo{Host: "example.com", Port: 8080},
		v1alpha1.RuleMatch{Domains: []string{"*"}}, // no ports → unconstrained
	) {
		t.Error("empty Ports list must NOT block matching")
	}
}

// TestMatchRequest_QueryParams covers QueryParam AND semantics, missing key,
// and bad-pattern fail-closed behaviour.
func TestMatchRequest_QueryParams(t *testing.T) {
	profile := newProfileWithRules([]v1alpha1.SecurityRule{{
		Name: "qp-rule",
		Match: []v1alpha1.RuleMatch{{
			Domains: []string{"*"},
			QueryParams: []v1alpha1.QueryParamMatch{
				{Name: "tenant", Type: v1alpha1.StringMatchTypeRegex, Value: "^acme-"},
				{Name: "env", Type: v1alpha1.StringMatchTypeRegex, Value: "^prod$"},
			},
		}},
	}})

	tests := []struct {
		name  string
		query url.Values
		want  bool
	}{
		{
			"both params match",
			url.Values{"tenant": []string{"acme-prod"}, "env": []string{"prod"}},
			true,
		},
		{
			"first value of multi-value key is used",
			url.Values{"tenant": []string{"acme-prod", "other-prod"}, "env": []string{"prod"}},
			true,
		},
		{
			"one missing key",
			url.Values{"tenant": []string{"acme-prod"}},
			false,
		},
		{
			"empty value (key present but value list empty)",
			url.Values{"tenant": []string{}, "env": []string{"prod"}},
			false,
		},
		{
			"pattern fails on second key",
			url.Values{"tenant": []string{"acme-prod"}, "env": []string{"staging"}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := RequestInfo{Host: "example.com", Query: tt.query}
			_, matched := MatchRequest(req, profile)
			if matched != tt.want {
				t.Errorf("got %v, want %v", matched, tt.want)
			}
		})
	}
}

// TestMatchQueryParams_BadPattern fails closed when the regex is invalid.
func TestMatchQueryParams_BadPattern(t *testing.T) {
	q := url.Values{"x": []string{"foo"}}
	if matchQueryParams(q, []v1alpha1.QueryParamMatch{
		{Name: "x", Type: v1alpha1.StringMatchTypeRegex, Value: "([invalid"},
	}) {
		t.Error("invalid regex must not match")
	}
}

// TestMatchPath_NowExcludesQuery confirms the new path semantics: a Prefix
// rule on "/foo" still matches a request whose raw :path was "/foo?x=1",
// but Exact "/foo" also matches it (which it did NOT before this PR).
func TestMatchPath_NowExcludesQuery(t *testing.T) {
	info := ParseRequestInfo(map[string]string{
		":authority": "example.com",
		":path":      "/foo?x=1",
		":method":    "GET",
	})
	if info.Path != "/foo" {
		t.Fatalf("expected pure path /foo, got %q", info.Path)
	}

	exactRule := v1alpha1.SecurityRule{
		Match: []v1alpha1.RuleMatch{{
			Domains: []string{"*"},
			Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypeExact, Value: "/foo"}},
		}},
	}
	if !MatchesRule(info, exactRule) {
		t.Error("Exact /foo must match a request whose raw :path included a query")
	}
}

// TestMatchHeaders_BadPattern verifies invalid regex patterns fail closed
// (no match) rather than panicking.
func TestMatchHeaders_BadPattern(t *testing.T) {
	headers := map[string]string{"x-foo": "bar"}
	if matchHeaders(headers, []v1alpha1.HeaderMatch{
		{Name: "X-Foo", Type: v1alpha1.StringMatchTypeRegex, Value: "([invalid"},
	}) {
		t.Error("invalid regex must not match")
	}
}

// TestMatchSinglePath_UnknownType protects the default branch when an unknown
// PathMatch type slips through (e.g. via stale clients before CRD validation).
func TestMatchSinglePath_UnknownType(t *testing.T) {
	if matchSinglePath("/anything", v1alpha1.PathMatch{Type: "Glob", Value: "*"}) {
		t.Error("unknown PathMatch type must not match")
	}
}

// TestMatchStringValue exercises every dispatch arm of the shared
// header/query-param helper, including the empty-Type default and the
// unknown-Type fail-closed branch.
func TestMatchStringValue(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		t       v1alpha1.StringMatchType
		operand string
		want    bool
	}{
		{"default empty Type behaves as Exact (match)", "Bearer abc", "", "Bearer abc", true},
		{"default empty Type behaves as Exact (miss)", "Bearer abc", "", "Bearer", false},
		{"Exact match", "secret", v1alpha1.StringMatchTypeExact, "secret", true},
		{"Exact miss", "secret-x", v1alpha1.StringMatchTypeExact, "secret", false},
		{"Prefix match", "Bearer eyJ.abc", v1alpha1.StringMatchTypePrefix, "Bearer ", true},
		{"Prefix miss", "Basic xyz", v1alpha1.StringMatchTypePrefix, "Bearer ", false},
		{"Regex match", "abc-42", v1alpha1.StringMatchTypeRegex, `^[a-z]+-\d+$`, true},
		{"Regex miss", "ABC", v1alpha1.StringMatchTypeRegex, `^[a-z]+$`, false},
		{"Regex invalid pattern fails closed", "anything", v1alpha1.StringMatchTypeRegex, `([invalid`, false},
		{"unknown Type fails closed", "anything", v1alpha1.StringMatchType("Glob"), "*", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchStringValue(tt.value, tt.t, tt.operand); got != tt.want {
				t.Errorf("matchStringValue(%q, %q, %q) = %v, want %v",
					tt.value, tt.t, tt.operand, got, tt.want)
			}
		})
	}
}

// TestMatchHeaders_AllStrategies covers Exact / Prefix / Regex against
// HeaderMatch and verifies the default (empty Type → Exact) behaviour.
func TestMatchHeaders_AllStrategies(t *testing.T) {
	headers := map[string]string{
		"x-tenant":      "acme-prod",
		"authorization": "Bearer eyJ.abc",
		"x-flag":        "on",
	}
	tests := []struct {
		name string
		m    []v1alpha1.HeaderMatch
		want bool
	}{
		{
			"exact-by-default matches",
			[]v1alpha1.HeaderMatch{{Name: "X-Flag", Value: "on"}}, // empty Type
			true,
		},
		{
			"explicit Exact misses",
			[]v1alpha1.HeaderMatch{{Name: "X-Flag", Type: v1alpha1.StringMatchTypeExact, Value: "off"}},
			false,
		},
		{
			"Prefix matches Authorization",
			[]v1alpha1.HeaderMatch{{Name: "Authorization", Type: v1alpha1.StringMatchTypePrefix, Value: "Bearer "}},
			true,
		},
		{
			"Prefix does NOT match when value is shorter than the operand",
			[]v1alpha1.HeaderMatch{{Name: "X-Flag", Type: v1alpha1.StringMatchTypePrefix, Value: "online"}},
			false,
		},
		{
			"Regex matches",
			[]v1alpha1.HeaderMatch{{Name: "X-Tenant", Type: v1alpha1.StringMatchTypeRegex, Value: `^acme-`}},
			true,
		},
		{
			"AND across two strategies",
			[]v1alpha1.HeaderMatch{
				{Name: "X-Flag", Value: "on"},
				{Name: "Authorization", Type: v1alpha1.StringMatchTypePrefix, Value: "Bearer "},
			},
			true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchHeaders(headers, tt.m); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMatchQueryParams_AllStrategies covers Exact / Prefix / Regex on
// QueryParamMatch.
func TestMatchQueryParams_AllStrategies(t *testing.T) {
	q := url.Values{
		"token":  []string{"secret-42"},
		"env":    []string{"prod"},
		"layout": []string{"compact"},
	}
	tests := []struct {
		name string
		m    []v1alpha1.QueryParamMatch
		want bool
	}{
		{
			"default Exact match",
			[]v1alpha1.QueryParamMatch{{Name: "env", Value: "prod"}},
			true,
		},
		{
			"default Exact miss",
			[]v1alpha1.QueryParamMatch{{Name: "env", Value: "staging"}},
			false,
		},
		{
			"Prefix match",
			[]v1alpha1.QueryParamMatch{{Name: "token", Type: v1alpha1.StringMatchTypePrefix, Value: "secret-"}},
			true,
		},
		{
			"Regex match",
			[]v1alpha1.QueryParamMatch{{Name: "token", Type: v1alpha1.StringMatchTypeRegex, Value: `^secret-\d+$`}},
			true,
		},
		{
			"AND across two strategies",
			[]v1alpha1.QueryParamMatch{
				{Name: "env", Value: "prod"},                                                            // Exact
				{Name: "token", Type: v1alpha1.StringMatchTypePrefix, Value: "secret-"},                 // Prefix
				{Name: "layout", Type: v1alpha1.StringMatchTypeRegex, Value: `^(compact|expanded)$`},    // Regex
			},
			true,
		},
		{
			"Prefix mismatch on one entry causes overall miss",
			[]v1alpha1.QueryParamMatch{
				{Name: "env", Value: "prod"},
				{Name: "token", Type: v1alpha1.StringMatchTypePrefix, Value: "public-"},
			},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchQueryParams(q, tt.m); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

// TestMatchesCondition_DomainsRequired ensures an empty Domains list is
// treated as fail-closed even when other dimensions match — this is the
// defensive contract documented in the matcher.
func TestMatchesCondition_DomainsRequired(t *testing.T) {
	cond := v1alpha1.RuleMatch{
		// No Domains.
		Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/"}},
		Methods: []string{"GET"},
	}
	req := RequestInfo{Host: "anything", Path: "/foo", Method: "GET"}
	if matchesCondition(req, cond) {
		t.Error("missing Domains must fail closed")
	}
}

// TestParseHeaderValue_FoundAndMissing covers the small lookup helper.
func TestParseHeaderValue_FoundAndMissing(t *testing.T) {
	headers := map[string]string{"x-foo": "bar"}

	if v, err := ParseHeaderValue(headers, "x-foo"); err != nil || v != "bar" {
		t.Errorf("expected (\"bar\", nil), got (%q, %v)", v, err)
	}
	if _, err := ParseHeaderValue(headers, "missing"); err == nil {
		t.Error("expected error for missing header")
	}
}
