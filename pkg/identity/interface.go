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
//   - GetProxyCABundle: no-op, returns an empty bundle.
//
// Enterprise deployment (secureIdentityProvider):
//   - IssueToken: calls HTTPS identity provider service with default fallback.
//   - PropagateSecurityToken: executes registered propagators (e.g., write credential files).
//   - GetProxyCABundle: fetches the proxy CA bundle from the identity provider service.
type IdentityProvider interface {
	// IssueToken generates an access token for the given token request.
	IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error)

	// PropagateSecurityToken executes post-token processing after a token is issued,
	// such as writing credentials into the sandbox runtime.
	PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error

	// GetProxyCABundle fetches the CA bundle used to verify the proxy server's TLS certificate.
	// Community default returns an empty bundle (no-op); enterprise implementations call the
	// identity provider service via the GetProxyCABundle action.
	GetProxyCABundle(ctx context.Context, req *GetProxyCABundleRequest) (*GetProxyCABundleResponse, error)
}

// TokenProvider is the low-level interface for issuing sandbox access tokens.
// Implementations can provide default random tokens or identity-aware tokens
// from an external identity provider service.
type TokenProvider interface {
	// IssueToken generates an access token for the given token request.
	// The context can carry deadlines and cancellation signals.
	IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error)
}
