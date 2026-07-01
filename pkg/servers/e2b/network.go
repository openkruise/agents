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
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

type sandboxNetworkState struct {
	AllowInternetAccess bool                   `json:"allowInternetAccess"`
	Network             *models.SandboxNetwork `json:"network,omitempty"`
}

func networkStateFromCreateRequest(request models.NewSandboxRequest) sandboxNetworkState {
	state := sandboxNetworkState{AllowInternetAccess: true}
	if request.AllowInternetAccess != nil {
		state.AllowInternetAccess = *request.AllowInternetAccess
	}
	if request.Network != nil {
		network := *request.Network
		state.Network = &network
	}
	if state.Network != nil && state.Network.AllowPublicTraffic == nil {
		allowPublicTraffic := true
		state.Network.AllowPublicTraffic = &allowPublicTraffic
	}
	return state
}

func marshalSandboxNetworkState(state sandboxNetworkState) (string, error) {
	raw, err := json.Marshal(state)
	return string(raw), err
}

func unmarshalSandboxNetworkState(raw string) (sandboxNetworkState, error) {
	state := sandboxNetworkState{AllowInternetAccess: true}
	if raw == "" {
		return state, nil
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return sandboxNetworkState{AllowInternetAccess: true}, err
	}
	return state, nil
}

func validateSandboxNetwork(network *models.SandboxNetwork) *web.ApiError {
	if network == nil {
		return nil
	}
	for _, target := range network.AllowOut {
		if err := validateNetworkTarget(target, true); err != nil {
			return &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("invalid allowOut target %q: %v", target, err)}
		}
	}
	for _, target := range network.DenyOut {
		if _, err := netip.ParsePrefix(target); err != nil {
			if _, addrErr := netip.ParseAddr(target); addrErr != nil {
				return &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("invalid denyOut target %q: expected an IP address or CIDR", target)}
			}
		}
	}
	for target := range network.Rules {
		if err := validateNetworkTarget(target, true); err != nil {
			return &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("invalid rules target %q: %v", target, err)}
		}
	}
	return nil
}

func validateNetworkTarget(target string, allowDomain bool) error {
	if target == "" {
		return fmt.Errorf("target cannot be empty")
	}
	if _, err := netip.ParsePrefix(target); err == nil {
		return nil
	}
	if _, err := netip.ParseAddr(target); err == nil {
		return nil
	}
	if !allowDomain {
		return fmt.Errorf("expected an IP address or CIDR")
	}
	domain := strings.TrimPrefix(target, "*.")
	if errs := validation.IsDNS1123Subdomain(domain); len(errs) != 0 {
		return fmt.Errorf("expected an IP address, CIDR, domain, or wildcard domain")
	}
	return nil
}

// UpdateSandboxNetwork atomically replaces a sandbox's mutable egress settings.
func (sc *Controller) UpdateSandboxNetwork(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	var request models.UpdateSandboxNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	requestedNetwork := &models.SandboxNetwork{AllowOut: request.AllowOut, DenyOut: request.DenyOut, Rules: request.Rules}
	if apiErr := validateSandboxNetwork(requestedNetwork); apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}

	sbx, apiErr := sc.getSandboxOfUser(r.Context(), r.PathValue("sandboxID"), liveSandboxStates)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	state, _ := sbx.GetState()
	if state != agentsv1alpha1.SandboxStateRunning {
		return web.ApiResponse[struct{}]{}, &web.ApiError{Code: http.StatusConflict, Message: fmt.Sprintf("sandbox %s is not running", sbx.GetName())}
	}

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		current := &agentsv1alpha1.Sandbox{}
		key := client.ObjectKey{Namespace: sbx.GetNamespace(), Name: sbx.GetName()}
		if err := sc.cache.GetAPIReader().Get(r.Context(), key, current); err != nil {
			return err
		}
		state, err := unmarshalSandboxNetworkState(current.Annotations[agentsv1alpha1.AnnotationNetworkConfig])
		if err != nil {
			return err
		}
		if state.Network == nil {
			state.Network = &models.SandboxNetwork{}
		}
		state.Network.AllowOut = request.AllowOut
		state.Network.DenyOut = request.DenyOut
		state.Network.Rules = request.Rules
		if request.AllowInternetAccess != nil {
			state.AllowInternetAccess = *request.AllowInternetAccess
		}
		raw, err := marshalSandboxNetworkState(state)
		if err != nil {
			return err
		}
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[agentsv1alpha1.AnnotationNetworkConfig] = raw
		return sc.cache.GetClient().Update(r.Context(), current)
	})
	if err != nil {
		code := http.StatusInternalServerError
		if apierrors.IsNotFound(err) {
			code = http.StatusNotFound
		} else if apierrors.IsConflict(err) {
			code = http.StatusConflict
		}
		return web.ApiResponse[struct{}]{}, &web.ApiError{Code: code, Message: fmt.Sprintf("failed to update sandbox network: %v", err)}
	}
	return web.ApiResponse[struct{}]{Code: http.StatusNoContent}, nil
}
