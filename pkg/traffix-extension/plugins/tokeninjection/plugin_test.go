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

package tokeninjection

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/credential"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins"
)

// fakeProviderServer returns a credential client wired to a configurable
// httptest server. The body parameter is returned as the JSON response body;
// statusCode is the HTTP status. The caller is responsible for calling Close.
func fakeProviderServer(t *testing.T, statusCode int, body string) (*credential.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statusCode)
		_, _ = w.Write([]byte(body))
	}))
	t.Setenv("IDENTITY_PROVIDER_URL", srv.URL)
	// Disable mTLS by pointing to nonexistent paths so the client uses plain HTTP.
	t.Setenv("CREDENTIAL_PROVIDER_CLIENT_CERT_PATH", "/nonexistent")
	t.Setenv("CREDENTIAL_PROVIDER_CLIENT_KEY_PATH", "/nonexistent")
	t.Setenv("CREDENTIAL_PROVIDER_CA_CERT_PATH", "/nonexistent")
	return credential.NewClient(), srv
}

func validProviderRef() *corev1.TypedLocalObjectReference {
	return &corev1.TypedLocalObjectReference{
		APIGroup: ptr("agentidentity.alibabacloud.com"),
		Kind:     "CredentialProvider",
		Name:     "llm-api-key",
	}
}

func tokenAction(when *v1alpha1.ActionCondition, fail v1alpha1.FailStrategy) *v1alpha1.SecurityRule {
	return &v1alpha1.SecurityRule{
		Name: "token-rule",
		Actions: &v1alpha1.SecurityRuleActions{
			TokenTransformation: &v1alpha1.TokenTransformationAction{
				When:             when,
				FailStrategy:     fail,
				TargetHeader:     "Authorization",
				ValueTemplate:    "Bearer {{ .Token }}",
				TokenProviderRef: validProviderRef(),
			},
		},
	}
}

func TestPluginNameAndNew(t *testing.T) {
	if New(nil).Name() != PluginName {
		t.Errorf("plugin name mismatch")
	}
}

// TestOnRequestHeaders_NoTransformation_Continue covers the early-out paths
// (nil Actions, nil TokenTransformation, Disabled).
func TestOnRequestHeaders_NoTransformation_Continue(t *testing.T) {
	p := New(nil)
	tests := []struct {
		name string
		rule *v1alpha1.SecurityRule
	}{
		{"nil actions", &v1alpha1.SecurityRule{Name: "r"}},
		{"no token transformation", &v1alpha1.SecurityRule{Name: "r", Actions: &v1alpha1.SecurityRuleActions{}}},
		{"disabled", &v1alpha1.SecurityRule{
			Name: "r",
			Actions: &v1alpha1.SecurityRuleActions{
				TokenTransformation: &v1alpha1.TokenTransformationAction{Disabled: true},
			},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := p.OnRequestHeaders(context.Background(), &plugins.RequestContext{}, tt.rule)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Action != plugins.ActionContinue {
				t.Errorf("expected Continue, got %v", res.Action)
			}
		})
	}
}

// TestOnRequestHeaders_NoSandboxToken_Continue covers the "no token" branch.
func TestOnRequestHeaders_NoSandboxToken_Continue(t *testing.T) {
	p := New(nil)
	rule := tokenAction(nil, v1alpha1.FailStrategyAllow)

	for _, rctx := range []*plugins.RequestContext{
		{}, // SandboxToken nil
		{SandboxToken: &credential.SandboxToken{AccessToken: ""}},
	} {
		res, err := p.OnRequestHeaders(context.Background(), rctx, rule)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if res.Action != plugins.ActionContinue {
			t.Errorf("expected Continue, got %v", res.Action)
		}
	}
}

// TestOnRequestHeaders_WhenNotMet_Continue verifies the plugin skips
// injection when the When condition does not match.
func TestOnRequestHeaders_WhenNotMet_Continue(t *testing.T) {
	rule := tokenAction(
		&v1alpha1.ActionCondition{Header: "Authorization", Pattern: "^Bearer __PLACEHOLDER__"},
		v1alpha1.FailStrategyAllow,
	)
	rctx := &plugins.RequestContext{
		SandboxToken: &credential.SandboxToken{AccessToken: "user-token"},
		Headers:      map[string]string{"authorization": "Basic xxxx"},
	}
	res, err := New(nil).OnRequestHeaders(context.Background(), rctx, rule)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != plugins.ActionContinue {
		t.Errorf("expected Continue, got %v", res.Action)
	}
}

// TestOnRequestHeaders_Success_Mutate covers the full happy path:
// valid sandbox token + valid provider ref + 200 OK with apiKey.
func TestOnRequestHeaders_Success_Mutate(t *testing.T) {
	cli, srv := fakeProviderServer(t, http.StatusOK, `{"apiKey":"real-token"}`)
	defer srv.Close()

	p := New(cli)
	rule := tokenAction(nil, v1alpha1.FailStrategyAllow)
	rctx := &plugins.RequestContext{
		SandboxToken: &credential.SandboxToken{AccessToken: "user-token"},
		PodNN:        types.NamespacedName{Namespace: "ns", Name: "pod"},
	}
	res, err := p.OnRequestHeaders(context.Background(), rctx, rule)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != plugins.ActionMutate {
		t.Fatalf("expected Mutate, got %v", res.Action)
	}
	if len(res.Responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(res.Responses))
	}
	headers := res.Responses[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders)
	set := headers.RequestHeaders.Response.HeaderMutation.SetHeaders
	if len(set) != 1 || string(set[0].Header.RawValue) != "Bearer real-token" {
		t.Errorf("unexpected SetHeaders: %+v", set)
	}
	// Confirm header struct shape (defensive).
	_ = []*corev3.HeaderValueOption{}
}

// TestOnRequestHeaders_FailBlock_ReturnsPermissionDenied verifies that any
// fetch failure with FailStrategyBlock surfaces a gRPC PermissionDenied error.
func TestOnRequestHeaders_FailBlock_ReturnsPermissionDenied(t *testing.T) {
	cli, srv := fakeProviderServer(t, http.StatusInternalServerError, `oops`)
	defer srv.Close()

	rule := tokenAction(nil, v1alpha1.FailStrategyBlock)
	rctx := &plugins.RequestContext{
		SandboxToken: &credential.SandboxToken{AccessToken: "user-token"},
	}
	_, err := New(cli).OnRequestHeaders(context.Background(), rctx, rule)
	if err == nil {
		t.Fatal("expected error from FailStrategyBlock")
	}
	if status.Code(err) != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", status.Code(err))
	}
}

// TestOnRequestHeaders_FailAllow_FallsThrough verifies that a fetch failure
// with FailStrategyAllow returns ActionContinue, letting the request through.
func TestOnRequestHeaders_FailAllow_FallsThrough(t *testing.T) {
	cli, srv := fakeProviderServer(t, http.StatusBadGateway, `bad`)
	defer srv.Close()

	rule := tokenAction(nil, v1alpha1.FailStrategyAllow)
	rctx := &plugins.RequestContext{
		SandboxToken: &credential.SandboxToken{AccessToken: "user-token"},
	}
	res, err := New(cli).OnRequestHeaders(context.Background(), rctx, rule)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != plugins.ActionContinue {
		t.Errorf("expected Continue, got %v", res.Action)
	}
}

// TestOnRequestHeaders_BadProviderRef triggers the validation error path
// without hitting the network.
func TestOnRequestHeaders_BadProviderRef(t *testing.T) {
	rule := &v1alpha1.SecurityRule{
		Name: "bad-ref",
		Actions: &v1alpha1.SecurityRuleActions{
			TokenTransformation: &v1alpha1.TokenTransformationAction{
				FailStrategy:     v1alpha1.FailStrategyAllow,
				TargetHeader:     "Authorization",
				ValueTemplate:    "Bearer {{ .Token }}",
				TokenProviderRef: &corev1.TypedLocalObjectReference{Kind: "Wrong"},
			},
		},
	}
	rctx := &plugins.RequestContext{
		SandboxToken: &credential.SandboxToken{AccessToken: "user-token"},
	}
	res, err := New(nil).OnRequestHeaders(context.Background(), rctx, rule)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != plugins.ActionContinue {
		t.Errorf("expected Continue under FailStrategyAllow, got %v", res.Action)
	}
}

// TestOnRequestHeaders_NilCredClient_Block surfaces the "client not configured"
// branch as PermissionDenied when FailStrategy=Block.
func TestOnRequestHeaders_NilCredClient_Block(t *testing.T) {
	rule := tokenAction(nil, v1alpha1.FailStrategyBlock)
	rctx := &plugins.RequestContext{
		SandboxToken: &credential.SandboxToken{AccessToken: "user-token"},
	}
	_, err := New(nil).OnRequestHeaders(context.Background(), rctx, rule)
	if err == nil || status.Code(err) != codes.PermissionDenied {
		t.Errorf("expected PermissionDenied, got %v", err)
	}
}

// TestOnRequestHeaders_BadValueTemplate covers the template-rendering failure path.
func TestOnRequestHeaders_BadValueTemplate(t *testing.T) {
	cli, srv := fakeProviderServer(t, http.StatusOK, `{"apiKey":"real-token"}`)
	defer srv.Close()

	rule := &v1alpha1.SecurityRule{
		Name: "bad-tmpl",
		Actions: &v1alpha1.SecurityRuleActions{
			TokenTransformation: &v1alpha1.TokenTransformationAction{
				FailStrategy:     v1alpha1.FailStrategyAllow,
				TargetHeader:     "Authorization",
				ValueTemplate:    "Bearer {{ .Token", // unclosed action
				TokenProviderRef: validProviderRef(),
			},
		},
	}
	rctx := &plugins.RequestContext{
		SandboxToken: &credential.SandboxToken{AccessToken: "user-token"},
	}
	res, err := New(cli).OnRequestHeaders(context.Background(), rctx, rule)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != plugins.ActionContinue {
		t.Errorf("expected Continue under Allow, got %v", res.Action)
	}
}
