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
	issueResp    *TokenResponse
	issueErr     error
	propagateErr error
}

func (m *mockIdentityProvider) IssueToken(_ context.Context, _ TokenRequest) (*TokenResponse, error) {
	return m.issueResp, m.issueErr
}

func (m *mockIdentityProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
	return m.propagateErr
}

func (m *mockIdentityProvider) GetProxyCABundle(_ context.Context, _ *GetProxyCABundleRequest) (*GetProxyCABundleResponse, error) {
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
