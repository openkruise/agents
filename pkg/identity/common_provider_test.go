/*
Copyright 2026.

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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// defaultTokenProvider
// ---------------------------------------------------------------------------

func TestNewDefaultTokenProvider(t *testing.T) {
	provider := NewDefaultTokenProvider()
	require.NotNil(t, provider)
	_, ok := provider.(*defaultTokenProvider)
	assert.True(t, ok, "should return *defaultTokenProvider")
}

func TestNewDefaultIdentityProvider(t *testing.T) {
	provider := NewDefaultIdentityProvider()
	require.NotNil(t, provider)
	_, ok := provider.(*defaultTokenProvider)
	assert.True(t, ok, "should return *defaultTokenProvider implementing IdentityProvider")
}

func TestDefaultTokenProvider_IssueToken(t *testing.T) {
	provider := NewDefaultTokenProvider()
	ctx := context.Background()

	resp, err := provider.IssueToken(ctx, TokenRequest{TokenType: TokenTypeAgent})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.NotEmpty(t, resp.RequestID, "RequestID should be a non-empty string")
	assert.NotEmpty(t, resp.AccessToken, "AccessToken should be a non-empty string")

	// Two calls should produce different tokens.
	resp2, err := provider.IssueToken(ctx, TokenRequest{TokenType: TokenTypeAgent})
	require.NoError(t, err)
	assert.NotEqual(t, resp.AccessToken, resp2.AccessToken, "each call should produce a unique token")
}

func TestDefaultTokenProvider_PropagateSecurityToken(t *testing.T) {
	provider := NewDefaultIdentityProvider()
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	err := provider.PropagateSecurityToken(context.Background(), sbx, &TokenResponse{AccessToken: "tok"})
	assert.NoError(t, err, "PropagateSecurityToken should be a no-op")
}

// ---------------------------------------------------------------------------
// fallbackIdentityProvider
// ---------------------------------------------------------------------------

// mockIdentityProvider is a simple mock for IdentityProvider used in fallback tests.
type mockIdentityProvider struct {
	issueResp      *TokenResponse
	issueErr       error
	propagateErr   error
	caBundleResp   *GetProxyCABundleResponse
	caBundleErr    error
}

func (m *mockIdentityProvider) IssueToken(_ context.Context, _ TokenRequest) (*TokenResponse, error) {
	return m.issueResp, m.issueErr
}

func (m *mockIdentityProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
	return m.propagateErr
}

func (m *mockIdentityProvider) GetProxyCABundle(_ context.Context, _ GetProxyCABundleRequest) (*GetProxyCABundleResponse, error) {
	if m.caBundleResp != nil || m.caBundleErr != nil {
		return m.caBundleResp, m.caBundleErr
	}
	return &GetProxyCABundleResponse{}, nil
}

func TestNewFallbackIdentityProvider(t *testing.T) {
	primary := &mockIdentityProvider{}
	p := NewFallbackIdentityProvider(primary)
	require.NotNil(t, p)
	fp, ok := p.(*fallbackIdentityProvider)
	require.True(t, ok)
	assert.Equal(t, primary, fp.primary)
	assert.NotNil(t, fp.fallback, "fallback should be the default provider")
}

func TestFallbackIdentityProvider_IssueToken_PrimarySuccess(t *testing.T) {
	expected := &TokenResponse{RequestID: "req-1", AccessToken: "primary-token"}
	primary := &mockIdentityProvider{issueResp: expected}
	p := NewFallbackIdentityProvider(primary)

	resp, err := p.IssueToken(context.Background(), TokenRequest{})
	require.NoError(t, err)
	assert.Equal(t, expected, resp, "should return primary provider's response")
}

func TestFallbackIdentityProvider_IssueToken_PrimaryError_FallbackToDefault(t *testing.T) {
	primary := &mockIdentityProvider{issueErr: fmt.Errorf("primary down")}
	p := NewFallbackIdentityProvider(primary)

	resp, err := p.IssueToken(context.Background(), TokenRequest{})
	require.NoError(t, err, "fallback should not return error")
	require.NotNil(t, resp)
	assert.NotEmpty(t, resp.AccessToken, "fallback should produce a UUID token")
}

func TestFallbackIdentityProvider_PropagateSecurityToken_DelegatesToPrimary(t *testing.T) {
	primary := &mockIdentityProvider{propagateErr: fmt.Errorf("propagation failed")}
	p := NewFallbackIdentityProvider(primary)

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	err := p.PropagateSecurityToken(context.Background(), sbx, &TokenResponse{AccessToken: "tok"})
	assert.Error(t, err, "PropagateSecurityToken should return primary's error directly")
	assert.Contains(t, err.Error(), "propagation failed")
}

// ---------------------------------------------------------------------------
// GetProxyCABundle tests
// ---------------------------------------------------------------------------

func TestDefaultTokenProvider_GetProxyCABundle(t *testing.T) {
	tests := []struct {
		name        string
		req         GetProxyCABundleRequest
		expectError string
	}{
		{
			name: "returns empty response with default request",
			req:  GetProxyCABundleRequest{},
		},
		{
			name: "returns empty response with IncludeSystemCA true",
			req:  GetProxyCABundleRequest{IncludeSystemCA: true},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider := NewDefaultIdentityProvider()
			resp, err := provider.GetProxyCABundle(context.Background(), tt.req)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)
				assert.Empty(t, resp.CABundle, "default provider should return empty CABundle")
				assert.Empty(t, resp.RequestID, "default provider should return empty RequestID")
			}
		})
	}
}

func TestFallbackIdentityProvider_GetProxyCABundle(t *testing.T) {
	tests := []struct {
		name        string
		primary     *mockIdentityProvider
		expectError string
		checkResp   func(t *testing.T, resp *GetProxyCABundleResponse)
	}{
		{
			name: "primary success returns primary response",
			primary: &mockIdentityProvider{
				caBundleResp: &GetProxyCABundleResponse{
					RequestID: "req-ca-1",
					CABundle:  "-----BEGIN CERTIFICATE-----\nMIIB...\n-----END CERTIFICATE-----",
				},
			},
			checkResp: func(t *testing.T, resp *GetProxyCABundleResponse) {
				assert.Equal(t, "req-ca-1", resp.RequestID)
				assert.Contains(t, resp.CABundle, "BEGIN CERTIFICATE")
			},
		},
		{
			name: "primary failure falls back to default empty response",
			primary: &mockIdentityProvider{
				caBundleErr: fmt.Errorf("CA service unavailable"),
			},
			checkResp: func(t *testing.T, resp *GetProxyCABundleResponse) {
				assert.Empty(t, resp.CABundle, "fallback should return empty CABundle")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := NewFallbackIdentityProvider(tt.primary)
			resp, err := p.GetProxyCABundle(context.Background(), GetProxyCABundleRequest{})

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				require.NotNil(t, resp)
			}

			if tt.checkResp != nil {
				tt.checkResp(t, resp)
			}
		})
	}
}
