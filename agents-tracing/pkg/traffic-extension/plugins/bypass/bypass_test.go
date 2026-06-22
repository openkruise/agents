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

package bypass

import (
	"context"
	"testing"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffic-extension/model"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins"
)

func TestPluginName(t *testing.T) {
	if got := New().Name(); got != PluginName {
		t.Errorf("Name(): want %q, got %q", PluginName, got)
	}
}

func TestPlugin_OnRequestHeaders(t *testing.T) {
	tests := []struct {
		name       string
		rule       *model.SecurityRule
		wantAction plugins.Action
	}{
		{
			name:       "nil actions falls through",
			rule:       &model.SecurityRule{Name: "no-actions"},
			wantAction: plugins.ActionContinue,
		},
		{
			name: "bypass=false falls through",
			rule: &model.SecurityRule{
				Name:    "no-bypass",
				Actions: v1alpha1.SecurityRuleActions{Bypass: false},
			},
			wantAction: plugins.ActionContinue,
		},
		{
			name: "non-bypass action falls through",
			rule: &model.SecurityRule{
				Name: "block-rule",
				Actions: v1alpha1.SecurityRuleActions{
					Block: &v1alpha1.BlockAction{StatusCode: 403},
				},
			},
			wantAction: plugins.ActionContinue,
		},
		{
			name: "bypass=true short-circuits",
			rule: &model.SecurityRule{
				Name:    "bypass-rule",
				Actions: v1alpha1.SecurityRuleActions{Bypass: true},
			},
			wantAction: plugins.ActionImmediate,
		},
		{
			name: "bypass=true wins even when other terminal actions are also set",
			rule: &model.SecurityRule{
				Name: "bypass-and-block",
				Actions: v1alpha1.SecurityRuleActions{
					Bypass: true,
					Block:  &v1alpha1.BlockAction{StatusCode: 403},
				},
			},
			wantAction: plugins.ActionImmediate,
		},
	}

	p := New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rctx := &plugins.RequestContext{
				PodNN:   types.NamespacedName{Namespace: "ns", Name: "pod-x"},
				Profile: &model.SecurityProfile{Profile: &v1alpha1.SecurityProfile{}},
			}
			res, err := p.OnRequestHeaders(context.Background(), rctx, tt.rule)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if res.Action != tt.wantAction {
				t.Fatalf("Action: want %v, got %v", tt.wantAction, res.Action)
			}
			if res.Action != plugins.ActionImmediate {
				return
			}

			// On bypass, the plugin must hand back exactly one passthrough
			// RequestHeaders response — never an ImmediateResponse, since
			// bypass forwards upstream rather than rejecting the request.
			if len(res.Responses) != 1 {
				t.Fatalf("Responses: want 1, got %d", len(res.Responses))
			}
			rh, ok := res.Responses[0].Response.(*extProcPb.ProcessingResponse_RequestHeaders)
			if !ok {
				t.Fatalf("expected RequestHeaders passthrough, got %T", res.Responses[0].Response)
			}
			if rh.RequestHeaders == nil {
				t.Fatal("expected non-nil RequestHeaders payload")
			}
			if rh.RequestHeaders.GetResponse() != nil &&
				rh.RequestHeaders.GetResponse().GetHeaderMutation() != nil {
				t.Errorf("expected no header mutation in passthrough, got %+v",
					rh.RequestHeaders.GetResponse().GetHeaderMutation())
			}
		})
	}
}

func TestProfileName(t *testing.T) {
	tests := []struct {
		name string
		rctx *plugins.RequestContext
		want string
	}{
		{name: "nil rctx", rctx: nil, want: ""},
		{name: "nil profile", rctx: &plugins.RequestContext{}, want: ""},
		{
			name: "named profile",
			rctx: &plugins.RequestContext{
				Profile: &model.SecurityProfile{Profile: &v1alpha1.SecurityProfile{}},
			},
			want: "",
		},
		{
			name: "named profile populated",
			rctx: func() *plugins.RequestContext {
				sp := &v1alpha1.SecurityProfile{}
				sp.Name = "my-profile"
				return &plugins.RequestContext{Profile: &model.SecurityProfile{Profile: sp}}
			}(),
			want: "my-profile",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := profileName(tt.rctx); got != tt.want {
				t.Errorf("profileName: want %q, got %q", tt.want, got)
			}
		})
	}
}

// TestPassThroughResponse_Shape pins the exact wire shape the orchestrator
// expects so a future refactor doesn't accidentally swap in an
// ImmediateResponse (which would terminate the request rather than forward it).
func TestPassThroughResponse_Shape(t *testing.T) {
	resp := passThroughResponse()
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
	if _, ok := resp.Response.(*extProcPb.ProcessingResponse_ImmediateResponse); ok {
		t.Fatal("passthrough must NOT be an ImmediateResponse")
	}
	rh, ok := resp.Response.(*extProcPb.ProcessingResponse_RequestHeaders)
	if !ok {
		t.Fatalf("expected RequestHeaders, got %T", resp.Response)
	}
	if rh.RequestHeaders == nil {
		t.Fatal("expected non-nil RequestHeaders payload")
	}
}
