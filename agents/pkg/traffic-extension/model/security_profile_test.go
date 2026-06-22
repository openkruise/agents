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

package model

import (
	"net/url"
	"testing"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// reqInfo is a one-line builder for RequestInfo so the test table stays
// readable.
func reqInfo(host, path, method string, port int32, headers map[string]string, query url.Values) RequestInfo {
	return RequestInfo{
		Host:    host,
		Port:    port,
		Path:    path,
		Method:  method,
		Headers: headers,
		Query:   query,
	}
}

// TestCompileRuleMatch_AndMatches drives every branch of RuleMatch.Matches
// through a table so the matcher's intent is documented alongside its
// behaviour.
func TestCompileRuleMatch_AndMatches(t *testing.T) {
	tests := []struct {
		name string
		raw  v1alpha1.RuleMatch
		req  RequestInfo
		want bool
	}{
		{
			name: "wildcard domain matches anything",
			raw:  v1alpha1.RuleMatch{Domains: []string{"*"}},
			req:  reqInfo("api.example.com", "/x", "GET", 0, nil, nil),
			want: true,
		},
		{
			name: "exact domain matches case-insensitively",
			raw:  v1alpha1.RuleMatch{Domains: []string{"API.example.com"}},
			req:  reqInfo("api.EXAMPLE.com", "/", "GET", 0, nil, nil),
			want: true,
		},
		{
			name: "suffix wildcard matches subdomain",
			raw:  v1alpha1.RuleMatch{Domains: []string{"*.example.com"}},
			req:  reqInfo("api.example.com", "/", "GET", 0, nil, nil),
			want: true,
		},
		{
			name: "suffix wildcard rejects bare apex",
			raw:  v1alpha1.RuleMatch{Domains: []string{"*.example.com"}},
			req:  reqInfo("example.com", "/", "GET", 0, nil, nil),
			want: false,
		},
		{
			name: "no domain match returns false",
			raw:  v1alpha1.RuleMatch{Domains: []string{"foo.com"}},
			req:  reqInfo("bar.com", "/", "GET", 0, nil, nil),
			want: false,
		},
		{
			name: "empty domains list never matches",
			raw:  v1alpha1.RuleMatch{Domains: nil},
			req:  reqInfo("any.com", "/", "GET", 0, nil, nil),
			want: false,
		},
		{
			name: "exact path match",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypeExact, Value: "/admin"}},
			},
			req:  reqInfo("h", "/admin", "GET", 0, nil, nil),
			want: true,
		},
		{
			name: "exact path mismatch",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypeExact, Value: "/admin"}},
			},
			req:  reqInfo("h", "/admin/keys", "GET", 0, nil, nil),
			want: false,
		},
		{
			name: "prefix path match",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/v1/chat"}},
			},
			req:  reqInfo("h", "/v1/chat/completions", "POST", 0, nil, nil),
			want: true,
		},
		{
			name: "regex path match",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypeRegex, Value: `^/v\d+/.*$`}},
			},
			req:  reqInfo("h", "/v2/foo", "GET", 0, nil, nil),
			want: true,
		},
		{
			name: "invalid regex fails closed",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypeRegex, Value: "["}},
			},
			req:  reqInfo("h", "/anything", "GET", 0, nil, nil),
			want: false,
		},
		{
			name: "method match case-insensitive",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Methods: []string{"POST"},
			},
			req:  reqInfo("h", "/", "post", 0, nil, nil),
			want: true,
		},
		{
			name: "method mismatch",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Methods: []string{"POST"},
			},
			req:  reqInfo("h", "/", "GET", 0, nil, nil),
			want: false,
		},
		{
			name: "port match",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Ports:   []int32{443, 8443},
			},
			req:  reqInfo("h", "/", "GET", 443, nil, nil),
			want: true,
		},
		{
			name: "port zero never matches non-empty ports",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Ports:   []int32{443},
			},
			req:  reqInfo("h", "/", "GET", 0, nil, nil),
			want: false,
		},
		{
			name: "port mismatch",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Ports:   []int32{443},
			},
			req:  reqInfo("h", "/", "GET", 80, nil, nil),
			want: false,
		},
		{
			name: "header exact match (header name lowercased internally)",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Headers: []v1alpha1.HeaderMatch{{Name: "X-Foo", Type: v1alpha1.StringMatchTypeExact, Value: "bar"}},
			},
			req:  reqInfo("h", "/", "GET", 0, map[string]string{"x-foo": "bar"}, nil),
			want: true,
		},
		{
			name: "header missing returns false",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Headers: []v1alpha1.HeaderMatch{{Name: "X-Foo", Type: v1alpha1.StringMatchTypeExact, Value: "bar"}},
			},
			req:  reqInfo("h", "/", "GET", 0, map[string]string{}, nil),
			want: false,
		},
		{
			name: "header value mismatch",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Headers: []v1alpha1.HeaderMatch{{Name: "X-Foo", Type: v1alpha1.StringMatchTypeExact, Value: "bar"}},
			},
			req:  reqInfo("h", "/", "GET", 0, map[string]string{"x-foo": "baz"}, nil),
			want: false,
		},
		{
			name: "header prefix match",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Headers: []v1alpha1.HeaderMatch{{Name: "Authorization", Type: v1alpha1.StringMatchTypePrefix, Value: "Bearer "}},
			},
			req:  reqInfo("h", "/", "GET", 0, map[string]string{"authorization": "Bearer xyz"}, nil),
			want: true,
		},
		{
			name: "header regex match",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Headers: []v1alpha1.HeaderMatch{{Name: "X-RequestID", Type: v1alpha1.StringMatchTypeRegex, Value: `^[a-f0-9]{8}$`}},
			},
			req:  reqInfo("h", "/", "GET", 0, map[string]string{"x-requestid": "deadbeef"}, nil),
			want: true,
		},
		{
			name: "invalid header regex fails closed",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Headers: []v1alpha1.HeaderMatch{{Name: "X-Bad", Type: v1alpha1.StringMatchTypeRegex, Value: "["}},
			},
			req:  reqInfo("h", "/", "GET", 0, map[string]string{"x-bad": "deadbeef"}, nil),
			want: false,
		},
		{
			name: "unknown header type fails closed",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"*"},
				Headers: []v1alpha1.HeaderMatch{{Name: "X-Foo", Type: "Weird", Value: "x"}},
			},
			req:  reqInfo("h", "/", "GET", 0, map[string]string{"x-foo": "x"}, nil),
			want: false,
		},
		{
			name: "query param exact match",
			raw: v1alpha1.RuleMatch{
				Domains:     []string{"*"},
				QueryParams: []v1alpha1.QueryParamMatch{{Name: "model", Type: v1alpha1.StringMatchTypeExact, Value: "gpt-4"}},
			},
			req:  reqInfo("h", "/", "GET", 0, nil, url.Values{"model": {"gpt-4"}}),
			want: true,
		},
		{
			name: "query param missing fails",
			raw: v1alpha1.RuleMatch{
				Domains:     []string{"*"},
				QueryParams: []v1alpha1.QueryParamMatch{{Name: "model", Type: v1alpha1.StringMatchTypeExact, Value: "gpt-4"}},
			},
			req:  reqInfo("h", "/", "GET", 0, nil, url.Values{}),
			want: false,
		},
		{
			name: "query param regex match",
			raw: v1alpha1.RuleMatch{
				Domains:     []string{"*"},
				QueryParams: []v1alpha1.QueryParamMatch{{Name: "v", Type: v1alpha1.StringMatchTypeRegex, Value: `^\d+$`}},
			},
			req:  reqInfo("h", "/", "GET", 0, nil, url.Values{"v": {"42"}}),
			want: true,
		},
		{
			name: "all match (AND) succeeds",
			raw: v1alpha1.RuleMatch{
				Domains: []string{"api.example.com"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/v1"}},
				Methods: []string{"POST"},
				Ports:   []int32{443},
				Headers: []v1alpha1.HeaderMatch{{Name: "X-K", Type: v1alpha1.StringMatchTypeExact, Value: "v"}},
				QueryParams: []v1alpha1.QueryParamMatch{
					{Name: "n", Type: v1alpha1.StringMatchTypePrefix, Value: "abc"},
				},
			},
			req: reqInfo("api.example.com", "/v1/x", "POST", 443,
				map[string]string{"x-k": "v"},
				url.Values{"n": {"abc-001"}}),
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rm := CompileRuleMatch(logr.Discard(), tc.raw)
			if got := rm.Matches(&tc.req); got != tc.want {
				t.Errorf("Matches() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSecurityRule_MatchesRequest_OrsMatchClauses checks that multiple
// match clauses in a SecurityRule are OR'd.
func TestSecurityRule_MatchesRequest_OrsMatchClauses(t *testing.T) {
	cr := SecurityRule{
		Matches: []RuleMatch{
			CompileRuleMatch(logr.Discard(), v1alpha1.RuleMatch{Domains: []string{"a.com"}}),
			CompileRuleMatch(logr.Discard(), v1alpha1.RuleMatch{Domains: []string{"b.com"}}),
		},
	}
	if !cr.MatchesRequest(&RequestInfo{Host: "b.com"}) {
		t.Errorf("expected OR semantics: b.com should match")
	}
	if cr.MatchesRequest(&RequestInfo{Host: "c.com"}) {
		t.Errorf("c.com should not match either clause")
	}
}

// TestCompileRuleMatch_InvalidRegex verifies that invalid regex patterns in
// path / header / queryParam matchers do not panic and produce nil Re,
// causing the matcher to fail closed at request time.
func TestCompileRuleMatch_InvalidRegex(t *testing.T) {
	rm := CompileRuleMatch(logr.Discard(), v1alpha1.RuleMatch{
		Paths: []v1alpha1.PathMatch{
			{Type: v1alpha1.PathMatchTypeRegex, Value: "([invalid"},
		},
		Headers: []v1alpha1.HeaderMatch{
			{Name: "X-K", Type: v1alpha1.StringMatchTypeRegex, Value: "([invalid"},
		},
		QueryParams: []v1alpha1.QueryParamMatch{
			{Name: "q", Type: v1alpha1.StringMatchTypeRegex, Value: "([invalid"},
		},
	})
	if len(rm.Paths) != 1 || rm.Paths[0].Re != nil {
		t.Errorf("expected nil Re for invalid path regex, got %+v", rm.Paths)
	}
	if len(rm.Headers) != 1 || rm.Headers[0].Re != nil {
		t.Errorf("expected nil Re for invalid header regex, got %+v", rm.Headers)
	}
	if len(rm.QueryParams) != 1 || rm.QueryParams[0].Re != nil {
		t.Errorf("expected nil Re for invalid queryParam regex, got %+v", rm.QueryParams)
	}

	// And the matchers fail closed: a request that would otherwise match
	// must be rejected.
	req := RequestInfo{
		Host:    "any.example",
		Path:    "anything",
		Headers: map[string]string{"x-k": "anything"},
		Query:   url.Values{"q": {"anything"}},
	}
	if rm.matchPath(req.Path) {
		t.Errorf("invalid path regex should fail closed")
	}
	if rm.matchHeaders(req.Headers) {
		t.Errorf("invalid header regex should fail closed")
	}
	if rm.matchQueryParams(req.Query) {
		t.Errorf("invalid queryParam regex should fail closed")
	}
}

// TestNewSecurityProfile covers the selector parsing path: a valid
// selector produces a *SecurityProfile, an invalid one returns an error
// without panicking.
func TestNewSecurityProfile(t *testing.T) {
	good := &v1alpha1.SecurityProfile{
		Spec: v1alpha1.SecurityProfileSpec{
			Selector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
		},
	}
	if sp, err := NewSecurityProfile(logr.Discard(), good); err != nil || sp == nil {
		t.Fatalf("good profile: err=%v sp=%v", err, sp)
	}

	bad := &v1alpha1.SecurityProfile{
		Spec: v1alpha1.SecurityProfileSpec{
			Selector: metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key: "!", Operator: metav1.LabelSelectorOpExists,
			}}},
		},
	}
	if sp, err := NewSecurityProfile(logr.Discard(), bad); err == nil || sp != nil {
		t.Fatalf("bad selector: expected error, got sp=%v err=%v", sp, err)
	}
}
