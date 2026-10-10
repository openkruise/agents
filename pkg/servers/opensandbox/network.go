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
	"net/netip"
	"strings"

	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/apimachinery/pkg/util/validation"
)

const networkActionDeny = "deny"

func resolveEgressPolicy(input *NetworkPolicy) (*infra.EgressPolicy, *web.ApiError) {
	if input == nil {
		return nil, nil
	}
	bad := func(message string) (*infra.EgressPolicy, *web.ApiError) {
		return nil, &web.ApiError{Code: http.StatusBadRequest, Message: message}
	}
	action := strings.ToLower(strings.TrimSpace(input.DefaultAction))
	if action == "" {
		action = networkActionDeny
	}
	if action != "allow" && action != networkActionDeny {
		return bad("unsupported networkPolicy defaultAction")
	}
	// Two native rules are reserved for DNS infrastructure and the terminal default.
	if len(input.Egress) > 48 {
		return bad("networkPolicy supports at most 48 egress rules")
	}
	result := &infra.EgressPolicy{DefaultAction: action}
	for i, raw := range input.Egress {
		var rule struct {
			Action string `json:"action"`
			Target string `json:"target"`
		}
		if err := json.Unmarshal(raw, &rule); err != nil {
			return bad(fmt.Sprintf("invalid egress rule %d: %v", i, err))
		}
		rule.Action = strings.ToLower(strings.TrimSpace(rule.Action))
		if rule.Action == "" {
			rule.Action = networkActionDeny
		}
		if rule.Action != "allow" && rule.Action != networkActionDeny {
			return bad(fmt.Sprintf("unsupported egress action at rule %d", i))
		}
		target := strings.TrimSpace(rule.Target)
		mapped := infra.EgressRule{Action: rule.Action}
		if ip, err := netip.ParseAddr(target); err == nil {
			mapped.CIDR = netip.PrefixFrom(ip, ip.BitLen()).String()
		} else if cidr, err := netip.ParsePrefix(target); err == nil {
			mapped.CIDR = cidr.Masked().String()
		} else {
			// TrafficPolicy FQDN peers are resolved to addresses by Agentio. Wildcard
			// names cannot be resolved that way and must never silently lose enforcement.
			if strings.Contains(target, "*") {
				return bad("networkPolicy wildcard domains are not supported by the native TrafficPolicy backend")
			}
			target = strings.TrimSuffix(strings.ToLower(target), ".")
			if len(validation.IsDNS1123Subdomain(target)) != 0 {
				return bad(fmt.Sprintf("invalid egress target at rule %d", i))
			}
			mapped.FQDN = target
		}
		result.Rules = append(result.Rules, mapped)
	}
	return result, nil
}
