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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// IdentityProvider is the unified interface for sandbox identity management.
// It combines token issuance with post-token security propagation.
//
// Community default (uuidTokenProvider):
//   - IssueToken: generates random UUID tokens.
//   - PropagateSecurityToken: no-op (no propagators registered).
//
// Internal deployment (secureIdentityProvider):
//   - IssueToken: calls external HTTPS identity provider with UUID fallback.
//   - PropagateSecurityToken: executes registered propagators (e.g., write credential files).
type IdentityProvider interface {
	// IssueToken generates an access token for the given token request.
	IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error)

	// PropagateSecurityToken executes post-token processing after a token is issued,
	// such as writing credentials into the sandbox runtime.
	PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error
}

// TokenProvider is the low-level interface for issuing sandbox access tokens.
// Implementations can provide simple UUID-based tokens or identity-aware tokens
// from an external identity provider service.
type TokenProvider interface {
	// IssueToken generates an access token for the given token request.
	// The context can carry deadlines and cancellation signals.
	IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error)
}
