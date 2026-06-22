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

package block

import (
	"context"
	"testing"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypeV3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffic-extension/model"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins"
)

func strPtr(s string) *string { return &s }

func TestBuildResponse(t *testing.T) {
	tests := []struct {
		name       string
		action     *v1alpha1.BlockAction
		wantStatus envoyTypeV3.StatusCode
		wantBody   []byte
	}{
		{
			name:       "custom status and body",
			action:     &v1alpha1.BlockAction{StatusCode: 451, Body: strPtr(`{"error":"blocked"}`)},
			wantStatus: envoyTypeV3.StatusCode(451),
			wantBody:   []byte(`{"error":"blocked"}`),
		},
		{
			name:       "default status when zero",
			action:     &v1alpha1.BlockAction{},
			wantStatus: envoyTypeV3.StatusCode(403),
			wantBody:   nil,
		},
		{
			name:       "nil body produces empty body",
			action:     &v1alpha1.BlockAction{StatusCode: 429, Body: nil},
			wantStatus: envoyTypeV3.StatusCode(429),
			wantBody:   nil,
		},
		{
			name:       "empty string body is preserved",
			action:     &v1alpha1.BlockAction{StatusCode: 403, Body: strPtr("")},
			wantStatus: envoyTypeV3.StatusCode(403),
			wantBody:   []byte(""),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := buildResponse(tt.action)

			immediate, ok := resp.Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
			if !ok {
				t.Fatalf("expected ImmediateResponse, got %T", resp.Response)
			}

			if immediate.ImmediateResponse.Status == nil {
				t.Fatal("expected non-nil status")
			}
			if got := immediate.ImmediateResponse.Status.Code; got != tt.wantStatus {
				t.Errorf("status: want %v, got %v", tt.wantStatus, got)
			}

			if string(immediate.ImmediateResponse.Body) != string(tt.wantBody) {
				t.Errorf("body: want %q, got %q", tt.wantBody, immediate.ImmediateResponse.Body)
			}
		})
	}
}

func TestPlugin_OnRequestHeaders_NoBlockAction(t *testing.T) {
	p := New()
	rule := &model.SecurityRule{
		Name:    "no-block",
		Actions: v1alpha1.SecurityRuleActions{},
	}

	res, err := p.OnRequestHeaders(context.Background(), &plugins.RequestContext{}, rule)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != plugins.ActionContinue {
		t.Errorf("expected ActionContinue, got %v", res.Action)
	}
}

func TestPlugin_OnRequestHeaders_NilActions(t *testing.T) {
	p := New()
	rule := &model.SecurityRule{Name: "no-actions"}

	res, err := p.OnRequestHeaders(context.Background(), &plugins.RequestContext{}, rule)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != plugins.ActionContinue {
		t.Errorf("expected ActionContinue, got %v", res.Action)
	}
}

func TestPlugin_OnRequestHeaders_BlockMatched(t *testing.T) {
	p := New()
	rule := &model.SecurityRule{
		Name: "block-rule",
		Actions: v1alpha1.SecurityRuleActions{
			Block: &v1alpha1.BlockAction{StatusCode: 451, Body: strPtr("nope")},
		},
	}

	res, err := p.OnRequestHeaders(context.Background(), &plugins.RequestContext{}, rule)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Action != plugins.ActionImmediate {
		t.Fatalf("expected ActionImmediate, got %v", res.Action)
	}
	if len(res.Responses) != 1 {
		t.Fatalf("expected 1 response, got %d", len(res.Responses))
	}
	immediate, ok := res.Responses[0].Response.(*extProcPb.ProcessingResponse_ImmediateResponse)
	if !ok {
		t.Fatalf("expected ImmediateResponse, got %T", res.Responses[0].Response)
	}
	if immediate.ImmediateResponse.Status.Code != envoyTypeV3.StatusCode(451) {
		t.Errorf("status: want 451, got %v", immediate.ImmediateResponse.Status.Code)
	}
	if string(immediate.ImmediateResponse.Body) != "nope" {
		t.Errorf("body: want %q, got %q", "nope", immediate.ImmediateResponse.Body)
	}
}
