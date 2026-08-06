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
	"time"

	"github.com/google/uuid"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// defaultAccessTokenLifetime is the lifetime the community default provider
// stamps on a TokenKindAccessToken placeholder token. It is intentionally very
// long (~100 years) so the random token behaves as effectively non-expiring:
// the default provider has no signing backend that could rotate it, and
// enterprise deployments override the provider entirely.
const defaultAccessTokenLifetime = 100 * 365 * 24 * time.Hour

// defaultTokenProvider is the default community implementation that generates
// random tokens without contacting any external identity provider service.
// It implements IdentityProvider with no-op propagation.
type defaultTokenProvider struct{}

// NewDefaultIdentityProvider creates an IdentityProvider with default token issuance
// and no-op security token propagation. This is the community default.
func NewDefaultIdentityProvider() IdentityProvider {
	return &defaultTokenProvider{}
}

// IssueToken mints a random placeholder token. For TokenKindAccessToken the
// token is stamped with a very long validity so it behaves as effectively
// non-expiring; every other kind uses the short default lifetime. The community
// default has no signing backend, so the value carries no real credential
// semantics: enterprise deployments register a gateway-backed provider instead.
func (u *defaultTokenProvider) IssueToken(_ context.Context, _ *agentsv1alpha1.Sandbox, kind TokenKind) (*TokenResponse, error) {
	expiry := time.Minute
	if kind == TokenKindAccessToken {
		expiry = defaultAccessTokenLifetime
	}
	return &TokenResponse{
		RequestID:             uuid.NewString(),
		AccessToken:           uuid.NewString(),
		SandboxClientID:       uuid.NewString(),
		AccessTokenExpiration: time.Now().Add(expiry).Format(time.RFC3339),
	}, nil
}

// PropagateSecurityToken is a no-op for the default provider.
// Community mode has no propagators registered.
func (u *defaultTokenProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
	return nil
}
