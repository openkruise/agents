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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// provider is the global IdentityProvider instance.
//
// Community default: defaultTokenProvider with random token generation and no-op propagation.
// Enterprise deployment: Override by calling RegisterProvider() during init() phase to replace
// the default with a custom implementation.
//
// Only one provider exists at a time. RegisterProvider overwrites the previous one.
//
// IMPORTANT: This variable MUST only be set during init() phase via RegisterProvider().
// It is NOT safe to modify at runtime due to concurrent access from multiple goroutines.
var provider IdentityProvider = NewDefaultIdentityProvider()

// RegisterProvider registers a custom IdentityProvider implementation, overriding
// the community default. This should be called during init() or application startup.
//
// The registered provider is used as-is; no automatic fallback wrapping is applied.
// If the provider's IssueToken fails, the error is propagated to callers so they
// can decide how to handle the failure (e.g., return a retriable error). Callers
// must never silently degrade to a meaningless UUID token, since such a token
// carries no identity and cannot be honored as a credential by the runtime.
func RegisterProvider(p IdentityProvider) {
	provider = p
}

// IssueToken delegates to the registered provider to generate a token of the
// given kind.
func IssueToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, kind TokenKind) (*TokenResponse, error) {
	return provider.IssueToken(ctx, sbx, kind)
}

// PropagateSecurityToken delegates to the registered provider to execute
// post-token processing (e.g., writing credentials into the sandbox runtime).
func PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error {
	return provider.PropagateSecurityToken(ctx, sbx, tokenResp)
}
