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

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/validation"
	"github.com/openkruise/agents/pkg/servers/web"
)

const networkActionDeny = "deny"

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
	if action != "" && action != "allow" && action != networkActionDeny {
		return bad("unsupported networkPolicy defaultAction")
	}
	var config infra.SandboxNetworkConfig
	for i, raw := range input.Egress {
		var rule struct {
			Action string `json:"action"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &rule); err != nil {
			return bad(fmt.Sprintf("invalid egress rule %d: %v", i, err))
		}
		switch strings.ToLower(strings.TrimSpace(rule.Action)) {
		case "allow":
			config.AllowOut = append(config.AllowOut, rule.Target)
		case "", networkActionDeny:
			config.DenyOut = append(config.DenyOut, rule.Target)
		default:
			return bad(fmt.Sprintf("unsupported egress action at rule %d", i))
		}
	}
	if err := validation.ValidateAllowOut(config.AllowOut); err != nil {
		return bad(err.Error())
	}
	if err := validation.ValidateDenyOut(config.DenyOut); err != nil {
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
		return "false"
	}
	return "true"
}
