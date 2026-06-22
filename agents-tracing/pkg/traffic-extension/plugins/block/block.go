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

// Package block implements the Block plugin: a terminal action that converts
// a matching SecurityRule's BlockAction into an Envoy ImmediateResponse.
package block

import (
	"context"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	envoyTypeV3 "github.com/envoyproxy/go-control-plane/envoy/type/v3"
	"sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/traffic-extension/model"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins"
	logutil "github.com/openkruise/agents/pkg/traffic-extension/util/logging"
)

// PluginName is the registry name of the Block plugin.
const PluginName = "block"

// defaultBlockStatus is used when BlockAction.StatusCode is zero
// (omitted by the user). The CRD also defaults this to 403.
const defaultBlockStatus = 403

// Plugin short-circuits requests that match a rule with a BlockAction.
// Block evaluation does not depend on sandbox identity.
type Plugin struct{}

// New returns a new Block plugin instance.
func New() *Plugin { return &Plugin{} }

// Name implements plugins.Plugin.
func (p *Plugin) Name() string { return PluginName }

// OnRequestHeaders returns ActionImmediate when the rule has a BlockAction,
// ActionContinue otherwise.
func (p *Plugin) OnRequestHeaders(ctx context.Context, rctx *plugins.RequestContext, rule *model.SecurityRule) (plugins.Result, error) {
	if rule.Actions.Block == nil {
		return plugins.ContinueResult(), nil
	}

	logger := log.FromContext(ctx)
	logger.V(logutil.DEBUG).Info("Block action matched, short-circuiting request",
		"rule", rule.Name,
		"profile", profileName(rctx),
		"pod", rctx.PodNN.String())

	return plugins.ImmediateResult(buildResponse(rule.Actions.Block)), nil
}

// buildResponse converts a BlockAction into an Envoy ImmediateResponse.
// Body is sent verbatim; Envoy applies a default text/plain content-type.
func buildResponse(action *v1alpha1.BlockAction) *extProcPb.ProcessingResponse {
	code := action.StatusCode
	if code == 0 {
		code = defaultBlockStatus
	}

	immediate := &extProcPb.ImmediateResponse{
		Status: &envoyTypeV3.HttpStatus{
			Code: envoyTypeV3.StatusCode(code),
		},
	}
	if action.Body != nil {
		immediate.Body = []byte(*action.Body)
	}

	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ImmediateResponse{
			ImmediateResponse: immediate,
		},
	}
}

// Finalize implements plugins.Plugin. Block never returns ActionRecord.
func (p *Plugin) Finalize(_ context.Context, _ *plugins.RequestContext, _ *model.SecurityRule) (plugins.Result, error) {
	return plugins.ContinueResult(), nil
}

func profileName(rctx *plugins.RequestContext) string {
	if rctx == nil || rctx.Profile == nil {
		return ""
	}
	return rctx.Profile.Profile.Name
}
