/*
Copyright 2025.

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

package identityprovider

import (
	"context"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// SecurityTokenPropagator is a function that propagates an issued security token
// into the sandbox runtime after a successful token issuance.
// It is invoked only when a security token has been successfully issued via the identity provider.
//
// Parameters:
//   - ctx: The context carrying logging and cancellation.
//   - sbx: The claimed sandbox Kubernetes object (used to derive runtime URL, access token, etc.).
//   - tokenResp: The issued token response to be written into the sandbox runtime.
//
// Community default: No propagators registered — this is a no-op.
// Internal deployment: Register propagators via RegisterSecurityTokenPropagator() to inject
// tokens into the sandbox runtime (e.g., write credential files via RunCommand).
type SecurityTokenPropagator func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error

// securityTokenPropagators holds all registered propagator functions.
// Internal packages register handlers here during init() via RegisterSecurityTokenPropagator().
// These are consumed by initSecureProvider() when creating the secureIdentityProvider.
// Community code does not register any handlers — the slice stays empty.
//
// IMPORTANT: This slice MUST only be modified during init() phase via RegisterSecurityTokenPropagator().
// It is NOT safe to modify at runtime due to concurrent reads from multiple goroutines.
var securityTokenPropagators []SecurityTokenPropagator

// RegisterSecurityTokenPropagator appends a propagator to the global registry.
// Internal packages call this during init() to register token processing handlers
// (e.g., WriteSecurityTokenToRuntime). The registered propagators are incorporated into
// the secureIdentityProvider when initSecureProvider() runs.
func RegisterSecurityTokenPropagator(propagator SecurityTokenPropagator) {
	securityTokenPropagators = append(securityTokenPropagators, propagator)
	klog.Infof("security token propagator registered, total: %d", len(securityTokenPropagators))
}

// SecurityTokenPropagatorCount returns the number of registered security token propagators.
func SecurityTokenPropagatorCount() int {
	return len(securityTokenPropagators)
}
