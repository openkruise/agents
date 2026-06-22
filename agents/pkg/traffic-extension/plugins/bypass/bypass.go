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

// Package bypass implements the Bypass plugin: a terminal action that
// short-circuits the rule chain and forwards the request unmodified. Unlike
// Block it does not produce a client-visible response — it simply tells the
// orchestrator to stop walking remaining rules and plugins so the request
// flows upstream as if no SecurityProfile had matched.
package bypass

import (
	"context"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/openkruise/agents/pkg/traffic-extension/model"
	"github.com/openkruise/agents/pkg/traffic-extension/plugins"
	logutil "github.com/openkruise/agents/pkg/traffic-extension/util/logging"
)

// PluginName is the registry name of the Bypass plugin.
const PluginName = "bypass"

// Plugin short-circuits requests that match a rule with Bypass=true.
// Bypass evaluation does not depend on sandbox identity and produces no
// header mutation; it merely tells the orchestrator to forward as-is and
// skip the remaining rules / plugins.
type Plugin struct{}

// New returns a new Bypass plugin instance.
func New() *Plugin { return &Plugin{} }

// Name implements plugins.Plugin.
func (p *Plugin) Name() string { return PluginName }

// OnRequestHeaders returns ActionImmediate with a passthrough response when
// the rule has Bypass=true, ActionContinue otherwise.
//
// The plugin is registered ahead of every other plugin so a matching Bypass
// rule wins over Block / TokenInjection / etc. within the same rule chain.
func (p *Plugin) OnRequestHeaders(ctx context.Context, rctx *plugins.RequestContext, rule *model.SecurityRule) (plugins.Result, error) {
	if !rule.Actions.Bypass {
		return plugins.ContinueResult(), nil
	}

	logger := log.FromContext(ctx)
	logger.V(logutil.DEBUG).Info("Bypass action matched, forwarding request unmodified",
		"rule", rule.Name,
		"profile", profileName(rctx),
		"pod", rctx.PodNN.String())

	return plugins.ImmediateResult(passThroughResponse()), nil
}

// passThroughResponse returns an Envoy ProcessingResponse that lets the
// request continue upstream without any header mutation. Mirrors the
// orchestrator's own passthrough so the wire shape is identical when no
// plugin produced any work.
func passThroughResponse() *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_RequestHeaders{
			RequestHeaders: &extProcPb.HeadersResponse{},
		},
	}
}

// Finalize implements plugins.Plugin. Bypass never returns ActionRecord.
func (p *Plugin) Finalize(_ context.Context, _ *plugins.RequestContext, _ *model.SecurityRule) (plugins.Result, error) {
	return plugins.ContinueResult(), nil
}

func profileName(rctx *plugins.RequestContext) string {
	if rctx == nil || rctx.Profile == nil {
		return ""
	}
	return rctx.Profile.Profile.Name
}
