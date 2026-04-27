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

	"github.com/google/uuid"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// uuidTokenProvider is the default community implementation that generates
// random UUID tokens without contacting any external service.
// It implements IdentityProvider with no-op propagation.
type uuidTokenProvider struct{}

// NewUUIDTokenProvider creates a TokenProvider that generates random UUID-based tokens.
// This is the default fallback used when no external identity provider is configured.
func NewUUIDTokenProvider() TokenProvider {
	return &uuidTokenProvider{}
}

// NewUUIDIdentityProvider creates an IdentityProvider with UUID-based token issuance
// and no-op security token propagation. This is the community default.
func NewUUIDIdentityProvider() IdentityProvider {
	return &uuidTokenProvider{}
}

func (u *uuidTokenProvider) IssueToken(_ context.Context, _ TokenRequest) (*TokenResponse, error) {
	return &TokenResponse{
		RequestID:   uuid.NewString(),
		AccessToken: uuid.NewString(),
	}, nil
}

// PropagateSecurityToken is a no-op for the UUID provider.
// Community mode has no propagators registered.
func (u *uuidTokenProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
	return nil
}

// fallbackTokenProvider wraps a primary TokenProvider and falls back to the
// UUID-based provider when the primary one returns an error. This ensures that
// sandbox claim is never blocked by an external identity provider outage.
type fallbackTokenProvider struct {
	primary  TokenProvider
	fallback TokenProvider
}

// NewFallbackTokenProvider creates a TokenProvider that delegates to the primary provider
// and automatically falls back to UUID-based token generation on any error.
func NewFallbackTokenProvider(primary TokenProvider) TokenProvider {
	return &fallbackTokenProvider{
		primary:  primary,
		fallback: NewUUIDTokenProvider(),
	}
}

func (f *fallbackTokenProvider) IssueToken(ctx context.Context, req TokenRequest) (*TokenResponse, error) {
	logger := klog.FromContext(ctx)
	resp, err := f.primary.IssueToken(ctx, req)
	if err != nil {
		logger.Error(err, "primary token provider failed, falling back to UUID token provider")
		return f.fallback.IssueToken(ctx, req)
	}
	return resp, nil
}
