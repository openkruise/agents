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

package identity

import (
	"context"
	"errors"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

// SecurityTokenPropagator is a function that propagates an issued security token
// into the sandbox runtime after a successful token issuance.
// It is invoked only when a security token has been successfully issued via the identity provider.
//
// Parameters:
//   - ctx: The context carrying logging and cancellation.
//   - sbx: The claimed sandbox Kubernetes object (used to derive runtime URL, access token, etc.).
//   - tokenResp: The issued token response to be written into the sandbox runtime.
//   - rtOpts: The transport the caller resolved for this sandbox (see
//     runtime.TransportOptionsFor). A propagator that reaches the sandbox runtime
//     must forward it to the runtime client (e.g. as the trailing argument of
//     WriteFileWithRuntime or ChmodFileOnRuntime) so the credential is delivered
//     over the same transport the rest of the flow uses; an empty slice keeps the
//     legacy plaintext path.
//
// Community default: No propagators registered — this is a no-op.
// Enterprise deployment: Register propagators via RegisterSecurityTokenPropagator() to inject
// tokens into the sandbox runtime (e.g., write credential files via RunCommand).
type SecurityTokenPropagator func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse,
	rtOpts ...agentsruntime.Option) error

// securityTokenPropagators holds all registered propagator functions.
// Enterprise packages register handlers here during init() via RegisterSecurityTokenPropagator().
// These are consumed by initSecureProvider() when creating the secureIdentityProvider.
// Community code does not register any handlers — the slice stays empty.
//
// IMPORTANT: This slice MUST only be modified during init() phase via RegisterSecurityTokenPropagator().
// It is NOT safe to modify at runtime due to concurrent reads from multiple goroutines.
var securityTokenPropagators []SecurityTokenPropagator

// RegisterSecurityTokenPropagator appends a propagator to the global registry.
// Enterprise packages call this during init() to register token processing handlers
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

// SecurityTokenCleaner is a function that removes a security token a propagator
// previously delivered into the sandbox runtime. It is the counterpart of
// SecurityTokenPropagator: whatever a propagator writes on issuance, the matching
// cleaner removes before the sandbox stops belonging to the claim it was issued
// for.
//
// Parameters:
//   - ctx: The context carrying logging and cancellation.
//   - sbx: The sandbox whose propagated credential is being removed. It still
//     carries the claim-time annotations at this point, so a cleaner can derive
//     the same runtime URL and access token its propagator used.
//   - rtOpts: The transport the caller resolved for this sandbox, forwarded on the
//     same terms as SecurityTokenPropagator (e.g. as the trailing argument of
//     RemovePathWithRuntime); an empty slice keeps the legacy plaintext path.
//
// A cleaner runs while the sandbox runtime is still reachable. It must treat an
// already absent credential as success, because it can be invoked after a partial
// propagation or after a runtime restart dropped the file.
//
// Community default: No cleaners registered — this is a no-op.
type SecurityTokenCleaner func(ctx context.Context, sbx *agentsv1alpha1.Sandbox,
	rtOpts ...agentsruntime.Option) error

// securityTokenCleaners holds all registered cleaner functions.
//
// IMPORTANT: This slice MUST only be modified during init() phase via
// RegisterSecurityTokenCleaner, matching securityTokenPropagators. It is NOT safe
// to modify at runtime due to concurrent reads from multiple goroutines.
var securityTokenCleaners []SecurityTokenCleaner

// RegisterSecurityTokenCleaner appends a cleaner to the global registry. A package
// that registers a propagator registers its cleaner here, so the write and the
// removal stay defined together.
func RegisterSecurityTokenCleaner(cleaner SecurityTokenCleaner) {
	securityTokenCleaners = append(securityTokenCleaners, cleaner)
	klog.Infof("security token cleaner registered, total: %d", len(securityTokenCleaners))
}

// SecurityTokenCleanerCount returns the number of registered security token cleaners.
func SecurityTokenCleanerCount() int {
	return len(securityTokenCleaners)
}

// CleanupSecurityToken runs every registered cleaner for the sandbox and joins
// their failures, so one cleaner failing still gives the others their turn at the
// credential they own. With no cleaners registered it returns nil without touching
// the sandbox, which is the community path.
//
// Callers gate on IsIDTokenRequested before invoking this, mirroring the issuance
// call sites, so a sandbox that never opted into an ID token is never contacted.
func CleanupSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox,
	rtOpts ...agentsruntime.Option) error {
	if len(securityTokenCleaners) == 0 {
		return nil
	}
	var errs []error
	for _, cleaner := range securityTokenCleaners {
		if err := cleaner(ctx, sbx, rtOpts...); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
