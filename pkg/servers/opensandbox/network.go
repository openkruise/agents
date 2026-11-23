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
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/validation"
	"github.com/openkruise/agents/pkg/servers/web"
)

const (
	networkActionAllow = "allow"
	networkActionDeny  = "deny"
)

type networkRule struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

func (r *networkRule) normalizeAction() bool {
	switch strings.ToLower(strings.TrimSpace(r.Action)) {
	case networkActionAllow:
		r.Action = networkActionAllow
	case "", networkActionDeny:
		r.Action = networkActionDeny
	default:
		return false
	}
	return true
}

func (r networkRule) addTo(config *infra.SandboxNetworkConfig) {
	if r.Action == networkActionAllow {
		config.AllowOut = append(config.AllowOut, r.Target)
	} else {
		config.DenyOut = append(config.DenyOut, r.Target)
	}
}

func validateNetworkConfig(config infra.SandboxNetworkConfig) error {
	if err := validation.ValidateAllowOut(config.AllowOut); err != nil {
		return err
	}
	return validation.ValidateDenyOut(config.DenyOut)
}

// resolveEgressPolicy converts destinations to native allow/deny lists. An
// explicit default deny uses the native internet-access label, not a final rule.
func resolveEgressPolicy(input *NetworkPolicy) (*infra.SandboxNetworkConfig, *web.ApiError) {
	if input == nil {
		return nil, nil
	}
	bad := func(message string) (*infra.SandboxNetworkConfig, *web.ApiError) {
		return nil, &web.ApiError{Code: http.StatusBadRequest, Message: message}
	}
	action := strings.ToLower(strings.TrimSpace(input.DefaultAction))
	if action != "" && action != networkActionAllow && action != networkActionDeny {
		return bad("unsupported networkPolicy defaultAction")
	}
	var config infra.SandboxNetworkConfig
	for i, raw := range input.Egress {
		var rule networkRule
		if err := json.Unmarshal(raw, &rule); err != nil {
			return bad(fmt.Sprintf("invalid egress rule %d: %v", i, err))
		}
		if !rule.normalizeAction() {
			return bad(fmt.Sprintf("unsupported egress action at rule %d", i))
		}
		rule.addTo(&config)
	}
	if err := validateNetworkConfig(config); err != nil {
		return bad(err.Error())
	}
	if len(config.AllowOut) == 0 && len(config.DenyOut) == 0 {
		if action == networkActionDeny {
			// Keep the traffic-proxy runtime for the administrator's global
			// deny policy, even without a per-sandbox TrafficPolicy.
			return &infra.SandboxNetworkConfig{}, nil
		}
		return nil, nil
	}
	return &config, nil
}

func allowInternetAccess(input *NetworkPolicy) string {
	if input != nil && strings.EqualFold(strings.TrimSpace(input.DefaultAction), networkActionDeny) {
		return agentsv1alpha1.False
	}
	return agentsv1alpha1.True
}
