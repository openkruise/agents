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

package opensandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/network"
)

type runtimeNetworkPolicy struct {
	DefaultAction string        `json:"defaultAction"`
	Egress        []networkRule `json:"egress"`
}

// PolicyStatusResponse reports the configured policy without an enforcement observation.
type PolicyStatusResponse struct {
	Status string               `json:"status"`
	Mode   string               `json:"mode"`
	Policy runtimeNetworkPolicy `json:"policy"`
}

func (s *Server) GetNetworkPolicy(r *http.Request) (web.ApiResponse[PolicyStatusResponse], *web.ApiError) {
	ctx := r.Context()
	sbx, defaultAction, apiErr := networkPolicySandbox(ctx)
	if apiErr != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, apiErr
	}
	config, err := s.manager.SelectSandboxNetworkPolicy(ctx, sbx)
	return networkPolicyResponse(defaultAction, config, err)
}

func (s *Server) ReplaceNetworkPolicy(r *http.Request) (web.ApiResponse[PolicyStatusResponse], *web.ApiError) {
	ctx := r.Context()
	sbx, defaultAction, apiErr := networkPolicySandbox(ctx)
	if apiErr != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, apiErr
	}
	var request runtimeNetworkPolicy
	if apiErr := decodeNetworkPolicyBody(r.Body, &request); apiErr != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, apiErr
	}
	if action := strings.ToLower(strings.TrimSpace(request.DefaultAction)); action != "" && action != defaultAction {
		return web.ApiResponse[PolicyStatusResponse]{}, invalidNetworkPolicy("defaultAction must match the existing sandbox policy")
	}
	config, err := parseRuntimeNetworkRules(request.Egress)
	if err != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, invalidNetworkPolicy(err.Error())
	}
	resolved, err := s.manager.ReplaceSandboxNetworkPolicy(ctx, sbx, config)
	return networkPolicyResponse(defaultAction, resolved, err)
}

func (s *Server) PatchNetworkPolicy(r *http.Request) (web.ApiResponse[PolicyStatusResponse], *web.ApiError) {
	ctx := r.Context()
	sbx, defaultAction, apiErr := networkPolicySandbox(ctx)
	if apiErr != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, apiErr
	}
	var request []networkRule
	if apiErr := decodeNetworkPolicyBody(r.Body, &request); apiErr != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, apiErr
	}
	if _, err := parseRuntimeNetworkRules(request); err != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, invalidNetworkPolicy(err.Error())
	}
	return s.changeNetworkPolicyRules(ctx, sbx, defaultAction, func(current []networkRule) []networkRule {
		return patchNetworkRules(current, request)
	})
}

func (s *Server) DeleteNetworkPolicyTargets(r *http.Request) (web.ApiResponse[PolicyStatusResponse], *web.ApiError) {
	ctx := r.Context()
	sbx, defaultAction, apiErr := networkPolicySandbox(ctx)
	if apiErr != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, apiErr
	}
	var request []*string
	if apiErr := decodeNetworkPolicyBody(r.Body, &request); apiErr != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, apiErr
	}
	targets := make(map[string]struct{}, len(request))
	for _, target := range request {
		if target == nil {
			return web.ApiResponse[PolicyStatusResponse]{}, invalidNetworkPolicy("network policy targets must be strings")
		}
		targets[network.NormalizeToCIDR(*target)] = struct{}{}
	}
	return s.changeNetworkPolicyRules(ctx, sbx, defaultAction, func(current []networkRule) []networkRule {
		return slices.DeleteFunc(current, func(rule networkRule) bool {
			_, remove := targets[network.NormalizeToCIDR(rule.Target)]
			return remove
		})
	})
}

// changeNetworkPolicyRules deliberately uses native read followed by full
// replacement. Concurrent mutations can overwrite changes made after the read.
func (s *Server) changeNetworkPolicyRules(
	ctx context.Context, sbx infra.Sandbox, defaultAction string, change func([]networkRule) []networkRule,
) (web.ApiResponse[PolicyStatusResponse], *web.ApiError) {
	current, err := s.manager.SelectSandboxNetworkPolicy(ctx, sbx)
	if err != nil {
		return networkPolicyResponse(defaultAction, current, err)
	}
	var config infra.SandboxNetworkConfig
	for _, rule := range change(networkRulesFromConfig(current)) {
		rule.addTo(&config)
	}
	if slices.Equal(config.AllowOut, current.AllowOut) && slices.Equal(config.DenyOut, current.DenyOut) {
		return networkPolicyResponse(defaultAction, current, nil)
	}
	if err := validateNetworkConfig(config); err != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, invalidNetworkPolicy(err.Error())
	}
	resolved, err := s.manager.ReplaceSandboxNetworkPolicy(ctx, sbx, config)
	return networkPolicyResponse(defaultAction, resolved, err)
}

func networkPolicySandbox(ctx context.Context) (infra.Sandbox, string, *web.ApiError) {
	sbx, apiErr := sandboxFromContextOrError(ctx)
	if apiErr != nil {
		return nil, "", apiErr
	}
	if !isSandboxViewable(sbx) {
		return nil, "", &web.ApiError{Code: http.StatusNotFound, Message: errMessageSandboxNotFound}
	}
	switch sbx.GetLabels()[agentsv1alpha1.LabelAllowInternetAccess] {
	case agentsv1alpha1.True:
		return sbx, networkActionAllow, nil
	case agentsv1alpha1.False:
		return sbx, networkActionDeny, nil
	default:
		return nil, "", &web.ApiError{Code: http.StatusNotImplemented, Message: "sandbox has no supported default network action"}
	}
}

func decodeNetworkPolicyBody(body io.Reader, dst any) *web.ApiError {
	decoder := json.NewDecoder(body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return invalidNetworkPolicy(fmt.Sprintf("invalid request body: %v", err))
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return invalidNetworkPolicy("request body must contain exactly one JSON value")
	}
	return nil
}

func invalidNetworkPolicy(message string) *web.ApiError {
	return &web.ApiError{Code: http.StatusBadRequest, Message: message}
}

func parseRuntimeNetworkRules(rules []networkRule) (infra.SandboxNetworkConfig, error) {
	var config infra.SandboxNetworkConfig
	for index := range rules {
		rule := &rules[index]
		if !rule.normalizeAction() {
			return infra.SandboxNetworkConfig{}, fmt.Errorf("unsupported egress action at rule %d", index)
		}
		rule.Target = network.NormalizeToCIDR(rule.Target)
		rule.addTo(&config)
	}
	return config, validateNetworkConfig(config)
}

func networkRulesFromConfig(config infra.SandboxNetworkConfig) []networkRule {
	rules := make([]networkRule, 0, len(config.AllowOut)+len(config.DenyOut))
	for _, target := range config.AllowOut {
		rules = append(rules, networkRule{Action: networkActionAllow, Target: target})
	}
	for _, target := range config.DenyOut {
		rules = append(rules, networkRule{Action: networkActionDeny, Target: target})
	}
	return rules
}

func patchNetworkRules(current, patch []networkRule) []networkRule {
	if len(patch) == 0 {
		return current
	}
	byTarget := make(map[string]string, len(patch))
	for _, rule := range patch {
		if _, exists := byTarget[rule.Target]; !exists {
			byTarget[rule.Target] = rule.Action
		}
	}
	applied := make(map[string]bool, len(byTarget))
	result := make([]networkRule, 0, len(current)+len(byTarget))
	for _, rule := range current {
		key := network.NormalizeToCIDR(rule.Target)
		if action, replace := byTarget[key]; replace {
			if applied[key] {
				continue
			}
			rule.Action = action
			applied[key] = true
		}
		result = append(result, rule)
	}
	for _, rule := range patch {
		if !applied[rule.Target] {
			result = append(result, rule)
			applied[rule.Target] = true
		}
	}
	return result
}

func networkPolicyResponse(
	defaultAction string, config infra.SandboxNetworkConfig, err error,
) (web.ApiResponse[PolicyStatusResponse], *web.ApiError) {
	if err != nil {
		return web.ApiResponse[PolicyStatusResponse]{}, mapLifecycleErrorToApiError(err)
	}
	rules := networkRulesFromConfig(config)
	// Mode describes configuration intent, even when the proxy has yet to converge.
	mode := defaultAction + "_all"
	if len(rules) > 0 {
		mode = "enforcing"
	}
	return web.ApiResponse[PolicyStatusResponse]{
		Body: PolicyStatusResponse{
			Status: "ok",
			Mode:   mode,
			Policy: runtimeNetworkPolicy{DefaultAction: defaultAction, Egress: rules},
		},
	}, nil
}
