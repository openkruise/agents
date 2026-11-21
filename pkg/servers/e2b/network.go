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

package e2b

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/network"
)

// maxNetworkEntriesPerList caps entries per allowOut/denyOut list to prevent
// oversized TrafficPolicy CRs from exhausting apiserver resources.
const maxNetworkEntriesPerList = 100

const e2bNetworkAnnotation = agentsv1alpha1.InternalPrefix + "e2b-network"

type e2bNetworkPolicy struct {
	AllowInternetAccess *bool                       `json:"allowInternetAccess,omitempty"`
	Network             models.SandboxNetworkConfig `json:"network"`
}

// Store one normalized header assignment set per domain, matching the existing E2B validation.
func normalizeNetworkAnnotationRules(rules map[string][]models.SandboxNetworkRule) (map[string][]models.SandboxNetworkRule, error) {
	normalized := make(map[string][]models.SandboxNetworkRule, len(rules))
	for domain, operations := range rules {
		headers, err := translateDomainTransforms(domain, operations)
		if err != nil {
			return nil, err
		}
		if len(headers) == 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(domain, "."))
		if len(validation.IsDNS1123Subdomain(strings.TrimPrefix(name, "*."))) != 0 {
			return nil, fmt.Errorf("network.rules: invalid transform domain %q", domain)
		}
		if _, found := normalized[name]; found {
			return nil, fmt.Errorf("network.rules: duplicate transform domain %q", domain)
		}
		values := make(map[string]string, len(headers))
		for _, header := range headers {
			values[header.Name] = header.Value
		}
		normalized[name] = []models.SandboxNetworkRule{{Transform: &models.SandboxNetworkTransform{Headers: values}}}
	}
	return normalized, nil
}

func describeNetwork(ctx context.Context, sbx infra.Sandbox) (*models.SandboxNetworkConfig, error) {
	if raw, found := sbx.GetAnnotations()[e2bNetworkAnnotation]; found {
		var policy e2bNetworkPolicy
		if err := json.Unmarshal([]byte(raw), &policy); err != nil {
			return nil, fmt.Errorf("decode E2B network annotation: %w", err)
		}
		if len(policy.Network.AllowOut) == 0 && len(policy.Network.DenyOut) == 0 {
			return nil, nil
		}
		return &models.SandboxNetworkConfig{AllowOut: policy.Network.AllowOut, DenyOut: policy.Network.DenyOut}, nil
	}
	config, err := sbx.SelectNetworkPolicy(ctx)
	if err != nil || config == nil {
		return nil, err
	}
	return &models.SandboxNetworkConfig{AllowOut: config.AllowOut, DenyOut: config.DenyOut}, nil
}

// validateAllowOut checks that allowOut entries are valid CIDR, IP, or FQDN.
// Wildcard domains are not supported.
func validateAllowOut(allowOut []string) error {
	if len(allowOut) > maxNetworkEntriesPerList {
		return fmt.Errorf("allowOut list exceeds maximum of %d entries", maxNetworkEntriesPerList)
	}
	for _, entry := range allowOut {
		if strings.Contains(entry, "*") {
			return fmt.Errorf("invalid allowOut entry: %q wildcard domains are not supported, use a concrete domain instead", entry)
		}
		if !network.IsCIDROrIP(entry) && !network.IsFQDN(entry) {
			return fmt.Errorf("invalid allowOut entry: %q is not a valid CIDR, IP, or domain", entry)
		}
	}
	return nil
}

// validateDenyOut checks that all denyOut entries are valid CIDR or bare IP addresses.
func validateDenyOut(denyOut []string) error {
	if len(denyOut) > maxNetworkEntriesPerList {
		return fmt.Errorf("denyOut list exceeds maximum of %d entries", maxNetworkEntriesPerList)
	}
	for _, entry := range denyOut {
		if !network.IsCIDROrIP(entry) {
			return fmt.Errorf("domains are not supported in denyOut: %q is not a valid CIDR or IP address", entry)
		}
	}
	return nil
}

// validateAndBuildNetworkConfig is the single entry point for validating raw
// network parameters and producing a normalized SandboxNetworkConfig ready for CR creation.
func validateAndBuildNetworkConfig(netConfig *models.SandboxNetworkConfig) (*models.SandboxNetworkConfig, error) {
	if netConfig == nil {
		return nil, nil
	}

	// Reject unsupported upstream top-level fields before the empty-config
	// early return: an egressProxy-only request must 400, not silently
	// become "no network config".
	if err := rejectUnsupportedNetworkFeatures(netConfig.EgressProxy, netConfig.MaskRequestHost); err != nil {
		return nil, err
	}

	// Step 1: Return nil if no network rules are needed. Rules carries L7
	// security rules and is handled separately, so its presence keeps the config.
	if len(netConfig.AllowOut) == 0 && len(netConfig.DenyOut) == 0 && len(netConfig.Rules) == 0 {
		return nil, nil
	}

	// Step 1: Validate allowOut — entries must be CIDR, IP, or FQDN
	if err := validateAllowOut(netConfig.AllowOut); err != nil {
		return nil, err
	}

	// Step 2: Validate denyOut — domains are not supported in deny lists
	if err := validateDenyOut(netConfig.DenyOut); err != nil {
		return nil, err
	}

	return netConfig, nil
}

// UpdateSandboxNetwork replaces the sandbox's network rules with the new configuration.
func (sc *Controller) UpdateSandboxNetwork(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	sandboxID := r.PathValue("sandboxID")

	var req models.SandboxNetworkUpdateConfig
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Failed to decode request body: %v", err),
		}
	}

	// Validate and build the network config in one step, before resolving
	// the L7 replacement, so the L4 lists are checked first.
	netConfig, err := validateAndBuildNetworkConfig(&models.SandboxNetworkConfig{
		AllowOut: req.AllowOut,
		DenyOut:  req.DenyOut,
	})
	if err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}

	// Resolve the L7 rule replacement before touching any resource so a
	// validation failure returns 400 with nothing written. Nil rules keep
	// the existing chain; an explicit empty object clears it.
	rulesJSON, rulesPresent, err := resolveSecurityRulesUpdate(&req)
	if err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}

	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID, liveSandboxStates)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	if _, hasProfile := sbx.GetAnnotations()[securityProfileAnnotation]; hasProfile {
		return web.ApiResponse[struct{}]{}, withSandboxResourceContext(&web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Cannot update E2B network for a sandbox using %q", securityProfileAnnotation),
		}, sbx)
	}

	if _, annotated := sbx.GetAnnotations()[e2bNetworkAnnotation]; annotated {
		rules, err := normalizeNetworkAnnotationRules(req.Rules)
		if err != nil {
			return web.ApiResponse[struct{}]{}, &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
		}
		policy := e2bNetworkPolicy{
			AllowInternetAccess: req.AllowInternetAccess,
			Network: models.SandboxNetworkConfig{
				AllowOut: req.AllowOut,
				DenyOut:  req.DenyOut,
				Rules:    rules,
			},
		}
		err = sbx.UpdateAnnotations(ctx, func(annotations map[string]string) error {
			if !rulesPresent {
				var current e2bNetworkPolicy
				if err := json.Unmarshal([]byte(annotations[e2bNetworkAnnotation]), &current); err != nil {
					return fmt.Errorf("decode E2B network annotation: %w", err)
				}
				policy.Network.Rules = current.Network.Rules
			}
			raw, err := json.Marshal(policy)
			if err != nil {
				return err
			}
			annotations[e2bNetworkAnnotation] = string(raw)
			if rulesPresent {
				delete(annotations, agentsv1alpha1.AnnotationSecurityRules)
			}
			return nil
		})
		if err != nil {
			log.Error(err, "failed to update E2B network annotation")
			return web.ApiResponse[struct{}]{}, withSandboxResourceContext(&web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: "Failed to update sandbox network configuration",
			}, sbx)
		}
		return web.ApiResponse[struct{}]{Code: http.StatusNoContent}, nil
	}

	// Replace the rule chain before widening L4: a sandbox must never reach
	// a newly allowed target without its new rules already persisted. The
	// data plane converges on both writes independently, exactly as at
	// creation.
	if rulesPresent {
		if err := sbx.UpdateSecurityRules(ctx, rulesJSON); err != nil {
			log.Error(err, "failed to update security rules")
			return web.ApiResponse[struct{}]{}, withSandboxResourceContext(&web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: fmt.Sprintf("Failed to update security rules: %v", err),
			}, sbx)
		}
	}

	var cfg infra.SandboxNetworkConfig
	if netConfig != nil {
		cfg = infra.SandboxNetworkConfig{
			AllowOut: netConfig.AllowOut,
			DenyOut:  netConfig.DenyOut,
		}
	}
	if err := sbx.UpdateNetworkPolicy(ctx, cfg); err != nil {
		log.Error(err, "failed to reconcile network CRs")
		return web.ApiResponse[struct{}]{}, withSandboxResourceContext(&web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to update network: %v", err),
		}, sbx)
	}

	log.Info("sandbox network updated", "sandboxID", sandboxID)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
