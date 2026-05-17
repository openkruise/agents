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

package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypeV3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"github.com/go-logr/logr"
	structpb "google.golang.org/protobuf/types/known/structpb"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/configstore"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/credential"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins/block"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins/bypass"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins/tokeninjection"
)

func TestExtractExtProcAttrs_NestedStructure(t *testing.T) {
	innerMap := map[string]interface{}{
		"filter_state['downstream_peer'].name":      "sleep-55874894df-mtqbk",
		"filter_state['downstream_peer'].namespace": "default",
		"filter_state['sandbox.token']":             "-",
		"filter_state['sandbox.labels']":            "YXBwPXNsZWVwLHNlcnZpY2UuaXN0aW8uaW8vY2Fub25pY2FsLW5hbWU9c2xlZXA=",
	}
	innerStruct, err := structpb.NewStruct(innerMap)
	if err != nil {
		t.Fatalf("Failed to create inner struct: %v", err)
	}

	attrs := map[string]*structpb.Struct{
		extProcAttrsKey: innerStruct,
	}

	result := extractExtProcAttrs(attrs)
	if result == nil {
		t.Fatal("Expected non-nil result from extractExtProcAttrs")
	}

	tests := []struct {
		name     string
		key      string
		expected string
	}{
		{
			name:     "extract pod name",
			key:      filterStateDownstreamPeerName,
			expected: "sleep-55874894df-mtqbk",
		},
		{
			name:     "extract pod namespace",
			key:      filterStateDownstreamPeerNamespace,
			expected: "default",
		},
		{
			name:     "extract sandbox token",
			key:      filterStateSandboxToken,
			expected: "-",
		},
		{
			name:     "extract sandbox labels",
			key:      filterStateSandboxLabels,
			expected: "YXBwPXNsZWVwLHNlcnZpY2UuaXN0aW8uaW8vY2Fub25pY2FsLW5hbWU9c2xlZXA=",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			val := extractFilterStateValueFromStruct(result, tc.key)
			if val != tc.expected {
				t.Errorf("Expected %q, got %q", tc.expected, val)
			}
		})
	}
}

func TestExtractExtProcAttrs_NilAndEmptyCases(t *testing.T) {
	result := extractExtProcAttrs(nil)
	if result != nil {
		t.Errorf("Expected nil for nil input, got %v", result)
	}

	result = extractExtProcAttrs(map[string]*structpb.Struct{})
	if len(result) > 0 {
		t.Errorf("Expected empty/nil result for empty map, got %v", result)
	}

	attrs := map[string]*structpb.Struct{
		"other_key": nil,
	}
	result = extractExtProcAttrs(attrs)
	if result != nil {
		t.Errorf("Expected nil for missing ext_proc key, got %v", result)
	}
}

func TestExtractFilterStateValueFromStruct(t *testing.T) {
	tests := []struct {
		name     string
		attrs    map[string]*structpb.Struct
		key      string
		expected string
	}{
		{
			name:     "nil attrs",
			attrs:    nil,
			key:      "test",
			expected: "",
		},
		{
			name:     "empty attrs",
			attrs:    map[string]*structpb.Struct{},
			key:      "test",
			expected: "",
		},
		{
			name: "direct key match",
			attrs: func() map[string]*structpb.Struct {
				m := map[string]*structpb.Struct{}
				s, _ := structpb.NewStruct(map[string]interface{}{"value": "value123"})
				m["my_key"] = s
				return m
			}(),
			key:      "my_key",
			expected: "value123",
		},
		{
			name: "filter_state key format",
			attrs: func() map[string]*structpb.Struct {
				m := map[string]*structpb.Struct{}
				s, _ := structpb.NewStruct(map[string]interface{}{"value": "my-pod"})
				m["filter_state['pod.name']"] = s
				return m
			}(),
			key:      "pod.name",
			expected: "my-pod",
		},
		{
			name: "suffix match for .name",
			attrs: func() map[string]*structpb.Struct {
				m := map[string]*structpb.Struct{}
				s, _ := structpb.NewStruct(map[string]interface{}{"value": "my-pod"})
				m["peer.name"] = s
				return m
			}(),
			key:      "name",
			expected: "my-pod",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractFilterStateValueFromStruct(tc.attrs, tc.key)
			if result != tc.expected {
				t.Errorf("Expected %q, got %q", tc.expected, result)
			}
		})
	}
}

func TestExtractNestedMap_ConvertsValues(t *testing.T) {
	innerMap := map[string]interface{}{
		"string_val": "hello",
		"map_val":    map[string]interface{}{"nested": "value"},
		"int_val":    float64(42),
	}

	s, err := structpb.NewStruct(innerMap)
	if err != nil {
		t.Fatalf("Failed to create struct: %v", err)
	}

	result := extractNestedMap(s)
	if result == nil {
		t.Fatal("Expected non-nil result")
	}

	if val := extractFilterStateValueFromStruct(result, "string_val"); val != "hello" {
		t.Errorf("Expected 'hello', got %q", val)
	}

	if _, ok := result["map_val"]; !ok {
		t.Error("Expected map_val key to exist")
	}

	if val := extractFilterStateValueFromStruct(result, "int_val"); val != "" {
		// Int values can't be extracted as strings, which is fine.
		// The important thing is it doesn't panic.
		_ = val
	}
}

func TestParseSandboxToken(t *testing.T) {
	validJSONTokenJSON := `{
		"requestId":"",
		"accessToken":"eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZ2VudF8xMjM0NTY3ODkwIn0.signature",
		"sandboxClientId":"x4Bs0OBxd7Yi44y3UBbwQl0PzC0LlvrA8kI0k7nuL7Y="
	}`
	validJSONTokenB64 := base64.StdEncoding.EncodeToString([]byte(validJSONTokenJSON))

	tests := []struct {
		name        string
		input       string
		expected    *credential.SandboxToken
		expectError bool
	}{
		{
			name:        "valid base64-encoded JSON token",
			input:       validJSONTokenB64,
			expectError: false,
			expected: &credential.SandboxToken{
				RequestID:       "",
				AccessToken:     "eyJhbGciOiJSUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhZ2VudF8xMjM0NTY3ODkwIn0.signature",
				SandboxClientID: "x4Bs0OBxd7Yi44y3UBbwQl0PzC0LlvrA8kI0k7nuL7Y=",
			},
		},
		{
			name:        "empty token",
			input:       "",
			expectError: true,
			expected:    nil,
		},
		{
			name:        "invalid base64",
			input:       "not-base64-at-all!!",
			expectError: true,
			expected:    nil,
		},
		{
			name:        "valid base64 but invalid JSON",
			input:       base64.StdEncoding.EncodeToString([]byte("not-json")),
			expectError: true,
			expected:    nil,
		},
		{
			name:        "missing accessToken field",
			input:       base64.StdEncoding.EncodeToString([]byte(`{"requestId":"","sandboxClientId":"abc="}`)),
			expectError: false,
			expected: &credential.SandboxToken{
				RequestID:       "",
				AccessToken:     "",
				SandboxClientID: "abc=",
			},
		},
		{
			name:        "missing sandboxClientId field",
			input:       base64.StdEncoding.EncodeToString([]byte(`{"requestId":"req1","accessToken":"tok123"}`)),
			expectError: false,
			expected: &credential.SandboxToken{
				RequestID:       "req1",
				AccessToken:     "tok123",
				SandboxClientID: "",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := ParseSandboxToken(tc.input)
			if tc.expectError {
				if err == nil {
					t.Errorf("Expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Errorf("Unexpected error: %v", err)
				return
			}
			if tc.expected != nil {
				if result == nil {
					t.Errorf("Expected non-nil result")
					return
				}
				if result.RequestID != tc.expected.RequestID {
					t.Errorf("RequestID mismatch: expected %q, got %q", tc.expected.RequestID, result.RequestID)
				}
				if result.AccessToken != tc.expected.AccessToken {
					t.Errorf("AccessToken mismatch: expected %q, got %q", tc.expected.AccessToken, result.AccessToken)
				}
				if result.SandboxClientID != tc.expected.SandboxClientID {
					t.Errorf("SandboxClientID mismatch: expected %q, got %q", tc.expected.SandboxClientID, result.SandboxClientID)
				}
			}
		})
	}
}

// --- HandleRequestHeaders integration tests --------------------------------
//
// These tests exercise the orchestrator end-to-end: profile lookup, rule
// matching, plugin dispatch (Block / TokenInjection), and short-circuit /
// passthrough behavior. They use the real configstore plus a Block-only
// plugin set so token injection is intentionally not exercised here.

func strPtr(s string) *string { return &s }

// makeRequestHeaders builds an extProcPb.HttpHeaders with the given
// pseudo-headers for testing.
func makeRequestHeaders(host, path, method string) *extProcPb.HttpHeaders {
	return &extProcPb.HttpHeaders{
		Headers: &corev3.HeaderMap{
			Headers: []*corev3.HeaderValue{
				{Key: ":authority", RawValue: []byte(host)},
				{Key: ":path", RawValue: []byte(path)},
				{Key: ":method", RawValue: []byte(method)},
			},
		},
	}
}

// makeAttrsWithLabels constructs the structpb attrs that the handler reads
// from Envoy filter_state. We omit the sandbox token so token injection
// would never run; only Block evaluation can fire.
func makeAttrsWithLabels(namespace, name, labelsB64 string) map[string]*structpb.Struct {
	inner, _ := structpb.NewStruct(map[string]interface{}{
		filterStateDownstreamPeerNamespace: namespace,
		filterStateDownstreamPeerName:      name,
		filterStateSandboxLabels:           labelsB64,
	})
	return map[string]*structpb.Struct{
		extProcAttrsKey: inner,
	}
}

// newProfile builds a SecurityProfile with the given selector and rules.
func newProfile(name, namespace string, selector map[string]string, rules []v1alpha1.SecurityRule) *v1alpha1.SecurityProfile {
	return &v1alpha1.SecurityProfile{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: v1alpha1.SecurityProfileSpec{
			Selector: metav1.LabelSelector{MatchLabels: selector},
			Rules:    rules,
		},
	}
}

// "app=blocked" base64-encoded, matching the format ParseSandboxLabels expects.
const testLabelsB64 = "YXBwPWJsb2NrZWQ="

// newServerWithBlockOnly constructs a Server wired only with the block plugin.
// Token-injection is intentionally not registered so we can exercise the
// orchestrator without plumbing a credential client.
func newServerWithBlockOnly(t *testing.T, store configstore.Store) *Server {
	t.Helper()
	return NewServer(false, ServerDeps{
		ConfigStore: store,
		Plugins:     []plugins.Plugin{block.New()},
	})
}

// newServerWithBypassFirst wires Bypass ahead of Block so the orchestrator
// reflects production plugin order. Token-injection is intentionally omitted
// to avoid plumbing a credential client.
func newServerWithBypassFirst(t *testing.T, store configstore.Store) *Server {
	t.Helper()
	return NewServer(false, ServerDeps{
		ConfigStore: store,
		Plugins:     []plugins.Plugin{bypass.New(), block.New()},
	})
}

func TestHandleRequestHeaders_BlockMatched(t *testing.T) {
	store := configstore.NewStore()
	body := `{"error":"forbidden"}`
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name: "block-secret-path",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/admin"}},
				Methods: []string{"GET"},
			}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 451, Body: strPtr(body)},
			},
		},
	}))

	srv := newServerWithBlockOnly(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/admin/keys", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	immediate, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse, got %T", resps[0].Response)
	}
	if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode(451) {
		t.Errorf("status: want 451, got %v", immediate.ImmediateResponse.Status.Code)
	}
	if string(immediate.ImmediateResponse.Body) != body {
		t.Errorf("body: want %q, got %q", body, immediate.ImmediateResponse.Body)
	}
}

func TestHandleRequestHeaders_BlockNotMatched_FallsThrough(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name: "block-admin",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/admin"}},
				Methods: []string{"GET"},
			}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 403},
			},
		},
	}))

	srv := newServerWithBlockOnly(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/v1/chat", "POST"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse); ok {
		t.Fatal("did not expect ImmediateResponse for non-matching path")
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders); !ok {
		t.Fatalf("expected pass-through RequestHeaders, got %T", resps[0].Response)
	}
}

// TestHandleRequestHeaders_BlockBeatsTokenTransformation verifies plugin order
// preserves the legacy "Block beats Token even when Token rule appears first"
// semantics: the orchestrator short-circuits as soon as the second rule's
// BlockAction matches, regardless of whether an earlier rule produced a
// token-injection mutation.
func TestHandleRequestHeaders_BlockBeatsTokenTransformation(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name: "token-rule",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/v1/chat"}},
			}},
			Actions: &v1alpha1.SecurityRuleActions{
				TokenTransformation: &v1alpha1.TokenTransformationAction{
					TargetHeader:  "Authorization",
					ValueTemplate: "Bearer {{ .Token }}",
				},
			},
		},
		{
			Name: "block-rule",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/v1/chat"}},
			}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 429},
			},
		},
	}))

	// Block-only wiring is sufficient: the token rule produces ActionContinue
	// (no token plugin registered), and the block rule matches afterwards.
	srv := newServerWithBlockOnly(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/v1/chat/completions", "POST"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	immediate, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse from block rule, got %T", resps[0].Response)
	}
	if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode(429) {
		t.Errorf("status: want 429, got %v", immediate.ImmediateResponse.Status.Code)
	}
}

// TestHandleRequestHeaders_NoProfileMatch verifies the passthrough path when
// no profile selector matches the pod labels.
func TestHandleRequestHeaders_NoProfileMatch(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "other"}, nil))

	srv := newServerWithBlockOnly(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders); !ok {
		t.Fatalf("expected pass-through, got %T", resps[0].Response)
	}
}

// TestHandleRequestHeaders_RuleWithNilActions covers the rule-skip branch when
// a rule matches but its Actions are nil.
func TestHandleRequestHeaders_RuleWithNilActions(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:    "no-actions",
			Match:   []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: nil,
		},
	}))
	srv := newServerWithBlockOnly(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders); !ok {
		t.Fatalf("expected pass-through for nil actions, got %T", resps[0].Response)
	}
}

// TestHandleRequestHeaders_MultipleProfiles_AlphabeticalOrder verifies that
// when multiple profiles match the same pod, profile-name sort order
// determines which Block fires.
func TestHandleRequestHeaders_MultipleProfiles_AlphabeticalOrder(t *testing.T) {
	store := configstore.NewStore()
	// "alpha" sorts before "beta" — its 401 must win.
	store.ProfileSet(newProfile("alpha", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:  "block-401",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 401},
			},
		},
	}))
	store.ProfileSet(newProfile("beta", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:  "block-403",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 403},
			},
		},
	}))

	srv := newServerWithBlockOnly(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	immediate := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
	if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode(401) {
		t.Errorf("expected 401 (alphabetically earlier profile), got %v", immediate.ImmediateResponse.Status.Code)
	}
}

// TestHandleRequestHeaders_UnimplementedActionWarns verifies the orchestrator
// passes through and logs (no error) when a rule declares an action that no
// plugin handles (e.g. Forwarding / IdentityInjection).
func TestHandleRequestHeaders_UnimplementedActionWarns(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:  "unimplemented-rule",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{
				Forwarding:        &v1alpha1.ForwardingAction{TargetHost: "x"},
				IdentityInjection: &v1alpha1.IdentityInjectionAction{},
				SecurityCheck:     &v1alpha1.SecurityCheckAction{},
				Mirroring:         &v1alpha1.MirroringAction{},
				RateLimit:         &v1alpha1.RateLimitAction{},
			},
		},
	}))
	srv := newServerWithBlockOnly(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders); !ok {
		t.Fatalf("expected pass-through, got %T", resps[0].Response)
	}
}

// TestWarnUnimplementedActions_CoversAllBranches drives the helper directly
// to ensure each branch (and the early-return paths) is hit. We use a
// discard sink so test output stays clean.
func TestWarnUnimplementedActions_CoversAllBranches(t *testing.T) {
	logger := logr.Discard()
	profile := &v1alpha1.SecurityProfile{}
	profile.Name = "p"
	profile.Namespace = "ns"

	// nil actions — early return.
	warnUnimplementedActions(logger, profile, &v1alpha1.SecurityRule{Name: "r"})

	// Empty actions — no unimplemented entries, second early return.
	warnUnimplementedActions(logger, profile, &v1alpha1.SecurityRule{
		Name:    "r",
		Actions: &v1alpha1.SecurityRuleActions{},
	})

	// All unimplemented set — full enumeration.
	warnUnimplementedActions(logger, profile, &v1alpha1.SecurityRule{
		Name: "r",
		Actions: &v1alpha1.SecurityRuleActions{
			Forwarding:        &v1alpha1.ForwardingAction{},
			IdentityInjection: &v1alpha1.IdentityInjectionAction{},
			SecurityCheck:     &v1alpha1.SecurityCheckAction{},
			Mirroring:         &v1alpha1.MirroringAction{},
			RateLimit:         &v1alpha1.RateLimitAction{},
		},
	})
}

// TestResolveSandboxToken_AllBranches drives the small helper directly to
// cover the env-var fallback, sentinel, parse-error, empty-accessToken, and
// missing-clientId branches in one place.
func TestResolveSandboxToken_AllBranches(t *testing.T) {
	t.Setenv(defaultSandboxTokenEnvVar, "")

	if tok := resolveSandboxToken(context.Background(), ""); tok != nil {
		t.Errorf("expected nil token when neither attrs nor env are set")
	}
	if tok := resolveSandboxToken(context.Background(), "-"); tok != nil {
		t.Errorf("expected nil for sentinel '-'")
	}
	if tok := resolveSandboxToken(context.Background(), "not-base64!!"); tok != nil {
		t.Errorf("expected nil on parse error")
	}

	// Empty AccessToken — treated as no token.
	emptyJSON := `{"requestId":"r","accessToken":"","sandboxClientId":"c"}`
	if tok := resolveSandboxToken(context.Background(), base64.StdEncoding.EncodeToString([]byte(emptyJSON))); tok != nil {
		t.Errorf("expected nil for empty accessToken")
	}

	// Missing sandboxClientId — should still return a token.
	noCID := `{"requestId":"r","accessToken":"a","sandboxClientId":""}`
	if tok := resolveSandboxToken(context.Background(), base64.StdEncoding.EncodeToString([]byte(noCID))); tok == nil {
		t.Errorf("expected non-nil token even when sandboxClientId is empty")
	}

	// Env-var fallback path.
	good := `{"requestId":"r","accessToken":"a","sandboxClientId":"c"}`
	t.Setenv(defaultSandboxTokenEnvVar, base64.StdEncoding.EncodeToString([]byte(good)))
	if tok := resolveSandboxToken(context.Background(), ""); tok == nil || tok.AccessToken != "a" {
		t.Errorf("expected env fallback to populate token, got %+v", tok)
	}
}

// TestPassThroughHandlers covers the trivial body / trailer / response stubs
// so they show up in coverage and accidental regressions surface immediately.
func TestPassThroughHandlers(t *testing.T) {
	srv := newServerWithBlockOnly(t, configstore.NewStore())
	ctx := context.Background()

	if r, err := srv.HandleRequestBody(ctx, nil); err != nil || len(r) != 1 {
		t.Errorf("HandleRequestBody: got len=%d err=%v", len(r), err)
	}
	if r, err := srv.HandleRequestTrailers(ctx, nil); err != nil || len(r) != 1 {
		t.Errorf("HandleRequestTrailers: got len=%d err=%v", len(r), err)
	}
	if r, err := srv.HandleResponseHeaders(ctx, nil); err != nil || len(r) != 1 {
		t.Errorf("HandleResponseHeaders: got len=%d err=%v", len(r), err)
	}
	if r, err := srv.HandleResponseBody(ctx, nil); err != nil || len(r) != 1 {
		t.Errorf("HandleResponseBody: got len=%d err=%v", len(r), err)
	}
	if r, err := srv.HandleResponseTrailers(ctx, nil); err != nil || len(r) != 1 {
		t.Errorf("HandleResponseTrailers: got len=%d err=%v", len(r), err)
	}
}

// TestGetFallbackToken_EnvSet verifies the env-var fallback for the sandbox
// token reader.
func TestGetFallbackToken_EnvSet(t *testing.T) {
	t.Setenv(defaultSandboxTokenEnvVar, "fallback-value")
	if got := getFallbackToken(); got != "fallback-value" {
		t.Errorf("expected fallback-value, got %q", got)
	}
}

func TestHandleRequestHeaders_BlockFiresWithoutSandboxToken(t *testing.T) {
	// No sandbox token in attrs and no DEFAULT_SANDBOX_TOKEN env var.
	// Block must still fire — it does not depend on agent identity.
	t.Setenv(defaultSandboxTokenEnvVar, "")

	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name: "block-all",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypeExact, Value: "/forbidden"}},
			}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 403, Body: strPtr("nope")},
			},
		},
	}))

	srv := newServerWithBlockOnly(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/forbidden", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	immediate, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse, got %T", resps[0].Response)
	}
	if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode(403) {
		t.Errorf("status: want 403, got %v", immediate.ImmediateResponse.Status.Code)
	}
	if string(immediate.ImmediateResponse.Body) != "nope" {
		t.Errorf("body: want %q, got %q", "nope", immediate.ImmediateResponse.Body)
	}
}

// TestHandleRequestHeaders_BypassMatched_ForwardsUnmodified verifies a Bypass
// rule short-circuits the chain with a passthrough response (NOT an
// ImmediateResponse, which would terminate the request).
func TestHandleRequestHeaders_BypassMatched_ForwardsUnmodified(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name: "trust-internal",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"internal.local"},
			}},
			Actions: &v1alpha1.SecurityRuleActions{Bypass: true},
		},
	}))

	srv := newServerWithBypassFirst(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("internal.local", "/anything", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse); ok {
		t.Fatal("Bypass must NOT produce an ImmediateResponse")
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders); !ok {
		t.Fatalf("expected pass-through RequestHeaders, got %T", resps[0].Response)
	}
}

// TestHandleRequestHeaders_BypassNotMatched_FallsThroughToBlock verifies the
// bypass plugin only short-circuits when the rule explicitly opts in. A rule
// that has Block but no Bypass must still produce the Block response.
func TestHandleRequestHeaders_BypassNotMatched_FallsThroughToBlock(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:  "block-only",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 403},
			},
		},
	}))

	srv := newServerWithBypassFirst(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	immediate, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse from block, got %T", resps[0].Response)
	}
	if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode(403) {
		t.Errorf("status: want 403, got %v", immediate.ImmediateResponse.Status.Code)
	}
}

// TestHandleRequestHeaders_BypassBeatsBlockSameRule verifies that when a
// single rule pathologically declares both Bypass=true and a Block action,
// Bypass wins because it is registered ahead of Block in the plugin chain.
func TestHandleRequestHeaders_BypassBeatsBlockSameRule(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:  "bypass-and-block",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{
				Bypass: true,
				Block:  &v1alpha1.BlockAction{StatusCode: 403},
			},
		},
	}))

	srv := newServerWithBypassFirst(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse); ok {
		t.Fatal("Bypass must outrank Block — got an ImmediateResponse")
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders); !ok {
		t.Fatalf("expected pass-through, got %T", resps[0].Response)
	}
}

// TestHandleRequestHeaders_BypassRuleSkipsLaterBlockRule verifies cross-rule
// short-circuit semantics: an earlier Bypass rule prevents a later Block rule
// from running, even though the Block rule would otherwise match.
func TestHandleRequestHeaders_BypassRuleSkipsLaterBlockRule(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name: "bypass-internal-first",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"internal.local"},
			}},
			Actions: &v1alpha1.SecurityRuleActions{Bypass: true},
		},
		{
			Name:  "block-everything-else",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 403},
			},
		},
	}))

	srv := newServerWithBypassFirst(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("internal.local", "/anything", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse); ok {
		t.Fatal("first-rule Bypass must skip the later Block rule")
	}
	if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders); !ok {
		t.Fatalf("expected pass-through, got %T", resps[0].Response)
	}
}

// TestHandleRequestHeaders_BlockRuleBeatsLaterBypassRule sanity-checks the
// reverse order: a Block rule that matches first must short-circuit before
// the orchestrator ever reaches a later Bypass rule.
func TestHandleRequestHeaders_BlockRuleBeatsLaterBypassRule(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:  "block-first",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 451},
			},
		},
		{
			Name:    "bypass-second",
			Match:   []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{Bypass: true},
		},
	}))

	srv := newServerWithBypassFirst(t, store)
	resps, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	immediate, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse from earlier Block rule, got %T", resps[0].Response)
	}
	if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode(451) {
		t.Errorf("status: want 451, got %v", immediate.ImmediateResponse.Status.Code)
	}
}

// --- Token-injection deferral tests --------------------------------------
//
// These tests pin the contract that the orchestrator records token-injection
// matches during scan and only fetches credentials in a finalize phase. A
// later Block rule must short-circuit BEFORE the credential-provider RPC
// fires.

// fakeIdentityProvider returns an httptest.Server that serves a constant
// successful token response, plus a counter recording how many times the
// server was called. Callers must Close the server.
func fakeIdentityProvider(t *testing.T) (*credential.Client, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"apiKey":"finalize-token"}`))
	}))
	t.Setenv("IDENTITY_PROVIDER_URL", srv.URL)
	t.Setenv("CREDENTIAL_PROVIDER_CLIENT_CERT_PATH", "/nonexistent")
	t.Setenv("CREDENTIAL_PROVIDER_CLIENT_KEY_PATH", "/nonexistent")
	t.Setenv("CREDENTIAL_PROVIDER_CA_CERT_PATH", "/nonexistent")
	return credential.NewClient(), srv, &calls
}

// newServerWithFullChain wires the production plugin order
// (bypass → block → tokeninjection) so the orchestrator's deferred
// finalize phase can be exercised end-to-end.
func newServerWithFullChain(t *testing.T, store configstore.Store, credClient *credential.Client) *Server {
	t.Helper()
	return NewServer(false, ServerDeps{
		ConfigStore: store,
		Plugins:     []plugins.Plugin{bypass.New(), block.New(), tokeninjection.New(credClient)},
	})
}

// makeAttrsWithSandboxToken bundles a base64-encoded SandboxToken alongside
// the pod identity attrs the handler reads from filter_state.
func makeAttrsWithSandboxToken(namespace, name, labelsB64 string, token credential.SandboxToken) map[string]*structpb.Struct {
	js, _ := json.Marshal(token)
	tokenB64 := base64.StdEncoding.EncodeToString(js)
	inner, _ := structpb.NewStruct(map[string]interface{}{
		filterStateDownstreamPeerNamespace: namespace,
		filterStateDownstreamPeerName:      name,
		filterStateSandboxLabels:           labelsB64,
		filterStateSandboxToken:            tokenB64,
	})
	return map[string]*structpb.Struct{
		extProcAttrsKey: inner,
	}
}

func tokenTransformationRule(name, pathPrefix string) v1alpha1.SecurityRule {
	return v1alpha1.SecurityRule{
		Name: name,
		Match: []v1alpha1.RuleMatch{{
			Domains: []string{"*"},
			Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: pathPrefix}},
		}},
		Actions: &v1alpha1.SecurityRuleActions{
			TokenTransformation: &v1alpha1.TokenTransformationAction{
				FailStrategy:  v1alpha1.FailStrategyAllow,
				TargetHeader:  "Authorization",
				ValueTemplate: "Bearer {{ .Token }}",
				TokenProviderRef: &corev1.TypedLocalObjectReference{
					APIGroup: strPtr("agentidentity.alibabacloud.com"),
					Kind:     "CredentialProvider",
					Name:     "llm-api-key",
				},
			},
		},
	}
}

// TestHandleRequestHeaders_BlockRuleSuppressesDeferredTokenFetch is the core
// regression guard for the doc-vs-code alignment fix: when a request matches
// both a tokenTransformation rule (earlier) and a Block rule (later), the
// orchestrator must skip the credential-provider RPC entirely and return the
// Block ImmediateResponse.
func TestHandleRequestHeaders_BlockRuleSuppressesDeferredTokenFetch(t *testing.T) {
	cli, srv, calls := fakeIdentityProvider(t)
	defer srv.Close()

	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		tokenTransformationRule("token-rule", "/v1/chat"),
		{
			Name: "block-rule",
			Match: []v1alpha1.RuleMatch{{
				Domains: []string{"*"},
				Paths:   []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/v1/chat"}},
			}},
			Actions: &v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 429},
			},
		},
	}))

	server := newServerWithFullChain(t, store, cli)
	resps, err := server.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/v1/chat/completions", "POST"),
		makeAttrsWithSandboxToken("default", "pod-x", testLabelsB64, credential.SandboxToken{
			RequestID:       "req-1",
			AccessToken:     "user-token",
			SandboxClientID: "client-1",
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(resps) != 1 {
		t.Fatalf("expected 1 response, got %d", len(resps))
	}
	immediate, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse from block, got %T", resps[0].Response)
	}
	if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode(429) {
		t.Errorf("status: want 429, got %v", immediate.ImmediateResponse.Status.Code)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("credential provider must NOT be called when a later Block matches; got %d calls", got)
	}
}

// TestHandleRequestHeaders_TokenRule_FinalizeMutates exercises the happy
// case where only a tokenTransformation rule matches: the rule is recorded
// during scan and then the finalize phase performs the credential-provider
// fetch and emits the SetHeaders mutation.
func TestHandleRequestHeaders_TokenRule_FinalizeMutates(t *testing.T) {
	cli, srv, calls := fakeIdentityProvider(t)
	defer srv.Close()

	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		tokenTransformationRule("token-rule", "/v1/chat"),
	}))

	server := newServerWithFullChain(t, store, cli)
	resps, err := server.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/v1/chat/completions", "POST"),
		makeAttrsWithSandboxToken("default", "pod-x", testLabelsB64, credential.SandboxToken{
			RequestID:       "req-1",
			AccessToken:     "user-token",
			SandboxClientID: "client-1",
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("expected 1 credential provider call, got %d", got)
	}
	if len(resps) != 1 {
		t.Fatalf("expected 1 response from finalize, got %d", len(resps))
	}
	rh, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("expected RequestHeaders response from finalize, got %T", resps[0].Response)
	}
	set := rh.RequestHeaders.GetResponse().GetHeaderMutation().GetSetHeaders()
	if len(set) != 1 || string(set[0].Header.RawValue) != "Bearer finalize-token" {
		t.Errorf("unexpected SetHeaders: %+v", set)
	}
}

// TestHandleRequestHeaders_BypassRuleSuppressesDeferredTokenFetch confirms
// the same deferral guarantee for Bypass: when an earlier Bypass rule
// matches, a later token rule's credential-provider RPC must NOT fire.
func TestHandleRequestHeaders_BypassRuleSuppressesDeferredTokenFetch(t *testing.T) {
	cli, srv, calls := fakeIdentityProvider(t)
	defer srv.Close()

	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:    "bypass-everything",
			Match:   []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: &v1alpha1.SecurityRuleActions{Bypass: true},
		},
		tokenTransformationRule("token-rule", "/"),
	}))

	server := newServerWithFullChain(t, store, cli)
	resps, err := server.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/anything", "GET"),
		makeAttrsWithSandboxToken("default", "pod-x", testLabelsB64, credential.SandboxToken{
			RequestID:       "req-1",
			AccessToken:     "user-token",
			SandboxClientID: "client-1",
		}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Errorf("credential provider must NOT be called when an earlier Bypass matches; got %d calls", got)
	}
	if len(resps) != 1 {
		t.Fatalf("expected 1 passthrough response, got %d", len(resps))
	}
	rh, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("expected RequestHeaders passthrough, got %T", resps[0].Response)
	}
	if rh.RequestHeaders.GetResponse().GetHeaderMutation() != nil {
		t.Errorf("Bypass response should carry no header mutation, got %+v",
			rh.RequestHeaders.GetResponse().GetHeaderMutation())
	}
}
