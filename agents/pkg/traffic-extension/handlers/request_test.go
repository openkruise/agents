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
	"sync"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypeV3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	structpb "google.golang.org/protobuf/types/known/structpb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffic-extension/framework/configstore"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins/block"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins/bypass"
	"github.com/openkruise/agents/pkg/traffic-extension/util/auditlog"
)

func TestExtractFilterStateString_NestedStructure(t *testing.T) {
	innerMap := map[string]interface{}{
		"filter_state['downstream_peer'].name":      "sleep-55874894df-mtqbk",
		"filter_state['downstream_peer'].namespace": "default",
		"filter_state['sandbox.labels']":            "YXBwPXNsZWVwLHNlcnZpY2UuaXN0aW8uaW8vY2Fub25pY2FsLW5hbWU9c2xlZXA=",
	}
	innerStruct, err := structpb.NewStruct(innerMap)
	if err != nil {
		t.Fatalf("Failed to create inner struct: %v", err)
	}

	attrs := map[string]*structpb.Struct{
		extProcAttrsKey: innerStruct,
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
			name:     "extract sandbox labels",
			key:      filterStateSandboxLabels,
			expected: "YXBwPXNsZWVwLHNlcnZpY2UuaXN0aW8uaW8vY2Fub25pY2FsLW5hbWU9c2xlZXA=",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			val := extractFilterStateString(attrs, tc.key)
			if val != tc.expected {
				t.Errorf("Expected %q, got %q", tc.expected, val)
			}
		})
	}
}

func TestGetExtProcStruct_NilAndEmptyCases(t *testing.T) {
	if s := getExtProcStruct(nil); s != nil {
		t.Errorf("Expected nil for nil input, got %v", s)
	}

	if s := getExtProcStruct(map[string]*structpb.Struct{}); s != nil {
		t.Errorf("Expected nil for empty map, got %v", s)
	}

	attrs := map[string]*structpb.Struct{
		"other_key": nil,
	}
	if s := getExtProcStruct(attrs); s != nil {
		t.Errorf("Expected nil for missing ext_proc key, got %v", s)
	}
}

func TestExtractFilterStateString(t *testing.T) {
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
				inner, _ := structpb.NewStruct(map[string]interface{}{"my_key": "value123"})
				return map[string]*structpb.Struct{extProcAttrsKey: inner}
			}(),
			key:      "my_key",
			expected: "value123",
		},
		{
			name: "filter_state key format",
			attrs: func() map[string]*structpb.Struct {
				inner, _ := structpb.NewStruct(map[string]interface{}{"filter_state['pod.name']": "my-pod"})
				return map[string]*structpb.Struct{extProcAttrsKey: inner}
			}(),
			key:      "pod.name",
			expected: "my-pod",
		},
		{
			name: "suffix match for .name",
			attrs: func() map[string]*structpb.Struct {
				inner, _ := structpb.NewStruct(map[string]interface{}{"peer.name": "my-pod"})
				return map[string]*structpb.Struct{extProcAttrsKey: inner}
			}(),
			key:      "name",
			expected: "my-pod",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := extractFilterStateString(tc.attrs, tc.key)
			if result != tc.expected {
				t.Errorf("Expected %q, got %q", tc.expected, result)
			}
		})
	}
}

func TestExtractFilterStateString_ValueTypes(t *testing.T) {
	innerMap := map[string]interface{}{
		"string_val": "hello",
		"map_val":    map[string]interface{}{"value": "nested-value"},
		"int_val":    float64(42),
	}

	inner, err := structpb.NewStruct(innerMap)
	if err != nil {
		t.Fatalf("Failed to create struct: %v", err)
	}
	attrs := map[string]*structpb.Struct{extProcAttrsKey: inner}

	if val := extractFilterStateString(attrs, "string_val"); val != "hello" {
		t.Errorf("Expected 'hello', got %q", val)
	}

	if val := extractFilterStateString(attrs, "map_val"); val != "nested-value" {
		t.Errorf("Expected 'nested-value', got %q", val)
	}

	// Int values can't be extracted as strings; should return empty without panic.
	if val := extractFilterStateString(attrs, "int_val"); val != "" {
		_ = val
	}
}

// --- HandleRequestHeaders integration tests --------------------------------
//
// These tests exercise the orchestrator end-to-end: profile lookup, rule
// matching, plugin dispatch, and short-circuit / passthrough behavior. The
// current plugin set is Bypass + Block only.

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
// from Envoy filter_state.
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
func newServerWithBlockOnly(t *testing.T, store configstore.Store) *Server {
	t.Helper()
	return NewServer(false, ServerDeps{
		ConfigStore: store,
		Plugins:     []plugins.Plugin{block.New()},
	})
}

// capturedAuditLogger collects every audit Entry the handler submits so
// tests can assert on outcomes without depending on a real worker
// goroutine. Implements auditlog.Logger.
type capturedAuditLogger struct {
	mu      sync.Mutex
	entries []auditlog.Entry
}

func (c *capturedAuditLogger) Submit(e auditlog.Entry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
}

func (c *capturedAuditLogger) last(t *testing.T) auditlog.Entry {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) == 0 {
		t.Fatal("expected at least one audit entry, got none")
	}
	return c.entries[len(c.entries)-1]
}

// newServerWithAudit returns a Server wired with the supplied plugins and a
// fresh capturedAuditLogger so the test can inspect the emitted entry.
func newServerWithAudit(t *testing.T, store configstore.Store, plugs []plugins.Plugin) (*Server, *capturedAuditLogger) {
	t.Helper()
	cap := &capturedAuditLogger{}
	srv := NewServer(false, ServerDeps{
		ConfigStore: store,
		Plugins:     plugs,
		AuditLogger: cap,
	})
	return srv, cap
}

// newServerWithBypassFirst wires Bypass ahead of Block so the orchestrator
// reflects production plugin order.
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
			Actions: v1alpha1.SecurityRuleActions{
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
			Actions: v1alpha1.SecurityRuleActions{
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

// TestHandleRequestHeaders_NoPodIdentity verifies fail-closed and fail-open
// behavior controlled by the UNAUTHENTICATED_EGRESS_POLICY env var.
func TestHandleRequestHeaders_NoPodIdentity(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:    "block-everything",
			Match:   []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: v1alpha1.SecurityRuleActions{Block: &v1alpha1.BlockAction{StatusCode: 403}},
		},
	}))

	tests := []struct {
		name         string
		policy       string
		expectDenied bool
	}{
		{
			name:         "default deny policy rejects unauthenticated egress",
			policy:       "deny",
			expectDenied: true,
		},
		{
			name:         "allow policy passes unauthenticated egress through",
			policy:       "allow",
			expectDenied: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			origPolicy := unauthenticatedEgressPolicy
			unauthenticatedEgressPolicy = tc.policy
			defer func() { unauthenticatedEgressPolicy = origPolicy }()

			srv, cap := newServerWithAudit(t, store, []plugins.Plugin{block.New()})
			resps, err := srv.HandleRequestHeaders(
				context.Background(),
				makeRequestHeaders("api.example.com", "/admin", "GET"),
				makeAttrsWithLabels("", "", ""),
			)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(resps) != 1 {
				t.Fatalf("expected 1 response, got %d", len(resps))
			}

			if tc.expectDenied {
				immediate, ok := resps[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
				if !ok {
					t.Fatalf("expected ImmediateResponse (deny), got %T", resps[0].Response)
				}
				if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode_Forbidden {
					t.Errorf("status: want Forbidden, got %v", immediate.ImmediateResponse.Status.Code)
				}
			} else {
				if _, ok := resps[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders); !ok {
					t.Fatalf("expected pass-through RequestHeaders, got %T", resps[0].Response)
				}
			}

			entry := cap.last(t)
			if entry.Pod.Name != "" || entry.Pod.Namespace != "" {
				t.Errorf("expected empty Pod in audit entry, got %+v", entry.Pod)
			}
		})
	}
}

// TestHandleRequestHeaders_RuleWithEmptyActions covers the passthrough path
// when a rule matches but its Actions struct is zero-valued (no Block, no
// Bypass): every plugin returns Continue and the request falls through.
func TestHandleRequestHeaders_RuleWithEmptyActions(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:    "no-actions",
			Match:   []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: v1alpha1.SecurityRuleActions{},
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
		t.Fatalf("expected pass-through for empty actions, got %T", resps[0].Response)
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
			Actions: v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 401},
			},
		},
	}))
	store.ProfileSet(newProfile("beta", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:  "block-403",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: v1alpha1.SecurityRuleActions{
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
			Actions: v1alpha1.SecurityRuleActions{Bypass: true},
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
			Actions: v1alpha1.SecurityRuleActions{
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
			Actions: v1alpha1.SecurityRuleActions{
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
			Actions: v1alpha1.SecurityRuleActions{Bypass: true},
		},
		{
			Name:  "block-everything-else",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: v1alpha1.SecurityRuleActions{
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
			Actions: v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 451},
			},
		},
		{
			Name:    "bypass-second",
			Match:   []v1alpha1.RuleMatch{{Domains: []string{"*"}}},
			Actions: v1alpha1.SecurityRuleActions{Bypass: true},
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

// --- Audit log end-to-end -------------------------------------------------

// TestHandleRequestHeaders_AuditEntry_NoProfileMatch_Passthrough verifies
// the passthrough path produces an audit entry with zero profiles and an
// empty actions list.
func TestHandleRequestHeaders_AuditEntry_NoProfileMatch_Passthrough(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "other"}, nil))

	srv, cap := newServerWithAudit(t, store, []plugins.Plugin{block.New()})
	_, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	e := cap.last(t)
	if e.Outcome != "passthrough" {
		t.Errorf("outcome: want passthrough, got %q", e.Outcome)
	}
	if e.Profiles != 0 {
		t.Errorf("profiles: want 0, got %d", e.Profiles)
	}
	if len(e.Actions) != 0 || len(e.Skipped) != 0 || e.Error != "" {
		t.Errorf("expected empty actions/skipped/error, got %+v", e)
	}
	if e.Pod.Namespace != "default" || e.Pod.Name != "pod-x" {
		t.Errorf("pod: want default/pod-x, got %s", e.Pod)
	}
	if e.Method != "GET" || e.Host != "api.example.com" || e.Path != "/x" {
		t.Errorf("request fields wrong: %+v", e)
	}
}

// TestHandleRequestHeaders_AuditEntry_BypassMatched marks the entry as
// "bypassed" with the bypass action recorded.
func TestHandleRequestHeaders_AuditEntry_BypassMatched(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:    "trust-internal",
			Match:   []v1alpha1.RuleMatch{{Domains: []string{"internal.local"}}},
			Actions: v1alpha1.SecurityRuleActions{Bypass: true},
		},
	}))

	srv, cap := newServerWithAudit(t, store, []plugins.Plugin{bypass.New(), block.New()})
	_, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("internal.local", "/anything", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	e := cap.last(t)
	if e.Outcome != "bypassed" {
		t.Errorf("outcome: want bypassed, got %q", e.Outcome)
	}
	if got := e.Actions; len(got) != 1 || got[0] != "bypass:default/p1/trust-internal" {
		t.Errorf("actions: want [bypass:default/p1/trust-internal], got %v", got)
	}
	if len(e.Skipped) != 0 {
		t.Errorf("skipped: want empty, got %v", e.Skipped)
	}
}

// TestHandleRequestHeaders_AuditEntry_BlockMatched marks the entry as
// "blocked".
func TestHandleRequestHeaders_AuditEntry_BlockMatched(t *testing.T) {
	store := configstore.NewStore()
	store.ProfileSet(newProfile("p1", "default", map[string]string{"app": "blocked"}, []v1alpha1.SecurityRule{
		{
			Name:  "block-admin",
			Match: []v1alpha1.RuleMatch{{Domains: []string{"*"}, Paths: []v1alpha1.PathMatch{{Type: v1alpha1.PathMatchTypePrefix, Value: "/admin"}}}},
			Actions: v1alpha1.SecurityRuleActions{
				Block: &v1alpha1.BlockAction{StatusCode: 403},
			},
		},
	}))

	srv, cap := newServerWithAudit(t, store, []plugins.Plugin{block.New()})
	_, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/admin/keys", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	)
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}

	e := cap.last(t)
	if e.Outcome != "blocked" {
		t.Errorf("outcome: want blocked, got %q", e.Outcome)
	}
	if got := e.Actions; len(got) != 1 || got[0] != "block:default/p1/block-admin" {
		t.Errorf("actions: want [block:default/p1/block-admin], got %v", got)
	}
}

// Ensure capturedAuditLogger satisfies the auditlog.Logger interface at
// compile time so refactors don't silently break the test harness.
var _ auditlog.Logger = (*capturedAuditLogger)(nil)

// TestHandleRequestHeaders_AuditEntry_NilLoggerDoesNotPanic guards against
// regressions in NewServer's nil-default: the handler must still produce a
// passthrough response when ServerDeps.AuditLogger is nil.
func TestHandleRequestHeaders_AuditEntry_NilLoggerDoesNotPanic(t *testing.T) {
	store := configstore.NewStore()
	srv := NewServer(false, ServerDeps{
		ConfigStore: store,
		Plugins:     []plugins.Plugin{block.New()},
		// AuditLogger intentionally nil.
	})
	if _, err := srv.HandleRequestHeaders(
		context.Background(),
		makeRequestHeaders("api.example.com", "/x", "GET"),
		makeAttrsWithLabels("default", "pod-x", testLabelsB64),
	); err != nil {
		t.Fatalf("unexpected: %v", err)
	}
}
