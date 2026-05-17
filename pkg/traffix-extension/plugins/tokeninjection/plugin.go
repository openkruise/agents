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

// Package tokeninjection implements the TokenInjection plugin: when a
// SecurityRule has a TokenTransformation action, the plugin fetches a token
// from the credential provider and rewrites the configured request header.
package tokeninjection

import (
	"context"
	"fmt"

	corev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffix-extension/framework/credential"
	"github.com/openkruise/agents/pkg/traffix-extension/plugins"
	logutil "github.com/openkruise/agents/pkg/traffix-extension/util/logging"
)

// PluginName is the registry name of the TokenInjection plugin.
const PluginName = "token-injection"

// Plugin injects credential-provider tokens into request headers based on
// the rule's TokenTransformation action.
type Plugin struct {
	credClient *credential.Client
}

// New constructs a TokenInjection plugin bound to the given credential client.
func New(credClient *credential.Client) *Plugin {
	return &Plugin{credClient: credClient}
}

// Name implements plugins.Plugin.
func (p *Plugin) Name() string { return PluginName }

// OnRequestHeaders performs only the cheap, side-effect-free checks
// (transformation declared, enabled, sandbox token present, when condition
// satisfied) and claims the rule for deferred execution by returning
// ActionRecord. The expensive credential-provider RPC is deferred to
// Finalize so a later Block rule can short-circuit without paying for it.
//
// Returns ActionContinue when the rule is irrelevant or token injection
// cannot apply (no TokenTransformation, disabled, no sandbox token, or the
// when condition rejected the request).
//
// A failing when-condition check still routes through handleFailure, since
// it represents a malformed rule rather than a "not applicable" branch.
func (p *Plugin) OnRequestHeaders(ctx context.Context, rctx *plugins.RequestContext, rule *v1alpha1.SecurityRule) (plugins.Result, error) {
	if rule.Actions == nil || rule.Actions.TokenTransformation == nil {
		return plugins.ContinueResult(), nil
	}

	logger := log.FromContext(ctx)
	loggerD := logger.V(logutil.DEBUG)

	tt := rule.Actions.TokenTransformation
	if tt.Disabled {
		loggerD.Info("TokenTransformation action disabled, skipping", "rule", rule.Name)
		return plugins.ContinueResult(), nil
	}

	// Sandbox token is required for token injection.
	if rctx.SandboxToken == nil || rctx.SandboxToken.AccessToken == "" {
		loggerD.Info("Sandbox token unavailable, skipping token injection", "rule", rule.Name)
		return plugins.ContinueResult(), nil
	}

	// when condition gate.
	whenMet, err := CheckWhenCondition(tt.When, rctx.Headers)
	if err != nil {
		return p.handleFailure(ctx, tt, rctx, fmt.Errorf("when condition check failed: %w", err))
	}
	if !whenMet {
		loggerD.Info("When condition not met, skipping token injection", "rule", rule.Name)
		return plugins.ContinueResult(), nil
	}

	loggerD.Info("Token-injection rule recorded; deferring credential-provider call until rule scan completes",
		"rule", rule.Name, "pod", rctx.PodNN.String())
	return plugins.RecordResult(), nil
}

// Finalize executes the deferred token-fetch and produces the header
// mutation. The orchestrator calls this only after the rule scan completes
// without an Immediate response, so a later Block rule (which would have
// produced an Immediate) is guaranteed to short-circuit before any
// credential-provider RPC fires.
func (p *Plugin) Finalize(ctx context.Context, rctx *plugins.RequestContext, rule *v1alpha1.SecurityRule) (plugins.Result, error) {
	if rule == nil || rule.Actions == nil || rule.Actions.TokenTransformation == nil {
		// Defensive: the orchestrator should never call Finalize without a
		// recorded rule, but fall through cleanly if it does.
		return plugins.ContinueResult(), nil
	}

	logger := log.FromContext(ctx)
	loggerD := logger.V(logutil.DEBUG)
	tt := rule.Actions.TokenTransformation

	resp, err := p.fetchAndBuild(ctx, rule, rctx)
	if err != nil {
		return p.handleFailure(ctx, tt, rctx, err)
	}

	loggerD.Info("Token injected successfully",
		"targetHeader", tt.TargetHeader,
		"rule", rule.Name,
		"pod", rctx.PodNN.String())
	return plugins.MutateResult(resp), nil
}

// fetchAndBuild validates the provider ref, fetches the token, and renders the
// final header-mutation response.
func (p *Plugin) fetchAndBuild(ctx context.Context, rule *v1alpha1.SecurityRule, rctx *plugins.RequestContext) (*extProcPb.ProcessingResponse, error) {
	tt := rule.Actions.TokenTransformation

	if err := ValidateTokenProviderRef(tt.TokenProviderRef); err != nil {
		return nil, fmt.Errorf("invalid token provider ref: %w", err)
	}

	if p.credClient == nil {
		return nil, fmt.Errorf("credential client is not configured")
	}
	token, err := p.credClient.GetToken(ctx, rctx.SandboxToken.AccessToken, rctx.SandboxToken.SandboxClientID, tt.TokenProviderRef.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get token from credential provider: %w", err)
	}

	headerValue, err := BuildHeaderValue(tt.ValueTemplate, token)
	if err != nil {
		return nil, fmt.Errorf("failed to build header value: %w", err)
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*corev3.HeaderValueOption{
							{
								Header: &corev3.HeaderValue{
									Key:      tt.TargetHeader,
									RawValue: []byte(headerValue),
								},
							},
						},
					},
				},
			},
		},
	}, nil
}

// handleFailure converts an injection error into the appropriate plugin
// result based on the rule's FailStrategy.
func (p *Plugin) handleFailure(ctx context.Context, tt *v1alpha1.TokenTransformationAction, rctx *plugins.RequestContext, err error) (plugins.Result, error) {
	logger := log.FromContext(ctx)
	if tt.FailStrategy == v1alpha1.FailStrategyBlock {
		logger.Error(err, "Token injection failed, blocking request", "pod", rctx.PodNN.String())
		return plugins.Result{}, status.Errorf(codes.PermissionDenied, "token injection failed: %v", err)
	}
	logger.Error(err, "Token injection failed, passing through", "pod", rctx.PodNN.String())
	return plugins.ContinueResult(), nil
}
