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

// IdentityProvider is the unified interface for sandbox identity management.
// It combines token issuance with post-token security propagation.
//
// Community default (defaultTokenProvider):
//   - IssueToken: generates random tokens using the default strategy.
//   - PropagateSecurityToken: no-op (no propagators registered).
//
// Enterprise deployment (secureIdentityProvider):
//   - IssueToken: calls HTTPS identity provider service; errors are surfaced
//     directly (no silent degradation to UUID).
//   - PropagateSecurityToken: executes registered propagators (e.g., write credential files).
type IdentityProvider interface {
	// IssueToken generates a token of the given kind for the sandbox.
	// The sbx parameter carries the sandbox workload metadata; it may be nil in
	// future principal-token paths, so implementations must guard against nil.
	//
	// kind selects which token to mint: TokenKindIDToken for the ID token
	// propagated into the sandbox as a credential, and TokenKindAccessToken for
	// the JWT used to reach the sandbox through the sandbox gateway. Both kinds
	// return a TokenResponse; callers read the minted token from AccessToken.
	//
	// Implementations own the composition of the concrete wire request: they
	// derive the SandboxInfo projection and any security metadata directly from
	// sbx (e.g. sbx.GetAnnotations() for storage-auth) rather than receiving a
	// pre-built TokenRequest. This keeps the community baseline free of
	// enterprise-only request-shaping policy while letting each provider assemble
	// exactly the atomic request its backend expects.
	IssueToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, kind TokenKind) (*TokenResponse, error)

	// PropagateSecurityToken executes post-token processing after a token is issued,
	// such as writing credentials into the sandbox runtime.
	PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error
}
