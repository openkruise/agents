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
	// IssueToken generates an access token for the given token request.
	// The sbx parameter carries the sandbox workload metadata; it may be nil in
	// future principal-token paths, so implementations must guard against nil.
	// The claim parameter is the SandboxClaim that triggered the issuance when
	// called from the SandboxClaim controller, and nil for refresh or E2B paths.
	// Implementations that need extra context (e.g. storage-auth annotations)
	// should read it directly from sbx.GetAnnotations() rather than relying on
	// caller-side metadata hooks.
	IssueToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, claim *agentsv1alpha1.SandboxClaim, req TokenRequest) (*TokenResponse, error)

	// PropagateSecurityToken executes post-token processing after a token is issued,
	// such as writing credentials into the sandbox runtime.
	PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error
}
