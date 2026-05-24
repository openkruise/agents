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

	"github.com/google/uuid"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// defaultTokenProvider is the default community implementation that generates
// random tokens without contacting any external identity provider service.
// It implements IdentityProvider with no-op propagation.
type defaultTokenProvider struct{}

// NewDefaultTokenProvider creates a TokenProvider that generates random tokens.
// This is the default strategy used when no identity provider service is configured.
func NewDefaultTokenProvider() TokenProvider {
	return &defaultTokenProvider{}
}

// NewDefaultIdentityProvider creates an IdentityProvider with default token issuance
// and no-op security token propagation. This is the community default.
func NewDefaultIdentityProvider() IdentityProvider {
	return &defaultTokenProvider{}
}

func (u *defaultTokenProvider) IssueToken(_ context.Context, _ TokenRequest) (*TokenResponse, error) {
	return &TokenResponse{
		RequestID:   uuid.NewString(),
		AccessToken: uuid.NewString(),
	}, nil
}

// PropagateSecurityToken is a no-op for the default provider.
// Community mode has no propagators registered.
func (u *defaultTokenProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
	return nil
}

// GetProxyCABundle is a no-op for the default provider and returns an empty bundle.
// Community mode does not contact any external identity provider service to fetch CA certs;
// callers should treat an empty CABundle as "use the host's system CA pool".
func (u *defaultTokenProvider) GetProxyCABundle(_ context.Context, _ *GetProxyCABundleRequest) (*GetProxyCABundleResponse, error) {
	return &GetProxyCABundleResponse{}, nil
}

// fallbackIdentityProvider wraps a primary IdentityProvider and falls back to the
// community default provider when the primary IssueToken returns an error.
// This ensures that sandbox claim is never blocked by an external identity provider outage.
//
// For PropagateSecurityToken, errors are returned directly without fallback,
// since the community default propagation is a no-op and degrading to it would
// silently lose important token propagation work.
type fallbackIdentityProvider struct {
	primary  IdentityProvider
	fallback IdentityProvider
}

// NewFallbackIdentityProvider creates an IdentityProvider that delegates to the primary provider
// and automatically falls back to UUID-based token generation on IssueToken error.
func NewFallbackIdentityProvider(primary IdentityProvider) IdentityProvider {
	return &fallbackIdentityProvider{
		primary:  primary,
		fallback: NewDefaultIdentityProvider(),
	}
}

func (f *fallbackIdentityProvider) IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	logger := klog.FromContext(ctx)
	resp, err := f.primary.IssueToken(ctx, req)
	if err != nil {
		logger.Error(err, "primary identity provider failed, falling back to UUID token provider")
		return f.fallback.IssueToken(ctx, req)
	}
	return resp, nil
}

func (f *fallbackIdentityProvider) PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error {
	return f.primary.PropagateSecurityToken(ctx, sbx, tokenResp)
}

// GetProxyCABundle delegates directly to the primary provider without falling back to the
// community default. The community no-op would silently return an empty bundle, which would
// mask a real outage of the identity provider service for callers that depend on the bundle.
func (f *fallbackIdentityProvider) GetProxyCABundle(ctx context.Context, req *GetProxyCABundleRequest) (*GetProxyCABundleResponse, error) {
	return f.primary.GetProxyCABundle(ctx, req)
}
