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

package identityprovider

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
// uuidTokenProvider
// ---------------------------------------------------------------------------

func TestNewUUIDTokenProvider(t *testing.T) {
	provider := NewUUIDTokenProvider()
	require.NotNil(t, provider)
	_, ok := provider.(*uuidTokenProvider)
	assert.True(t, ok, "should return *uuidTokenProvider")
}

func TestNewUUIDIdentityProvider(t *testing.T) {
	provider := NewUUIDIdentityProvider()
	require.NotNil(t, provider)
	_, ok := provider.(*uuidTokenProvider)
	assert.True(t, ok, "should return *uuidTokenProvider implementing IdentityProvider")
}

func TestUUIDTokenProvider_IssueToken(t *testing.T) {
	provider := NewUUIDTokenProvider()
	ctx := context.Background()

	resp, err := provider.IssueToken(ctx, TokenRequest{TokenType: TokenTypeAgent})
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.NotEmpty(t, resp.RequestID, "RequestID should be a non-empty UUID")
	assert.NotEmpty(t, resp.AccessToken, "AccessToken should be a non-empty UUID")

	// Two calls should produce different tokens.
	resp2, err := provider.IssueToken(ctx, TokenRequest{TokenType: TokenTypeAgent})
	require.NoError(t, err)
	assert.NotEqual(t, resp.AccessToken, resp2.AccessToken, "each call should produce a unique token")
}

func TestUUIDTokenProvider_PropagateSecurityToken(t *testing.T) {
	provider := NewUUIDIdentityProvider()
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
	}
	err := provider.PropagateSecurityToken(context.Background(), sbx, &TokenResponse{AccessToken: "tok"})
	assert.NoError(t, err, "PropagateSecurityToken should be a no-op")
}

// ---------------------------------------------------------------------------
// fallbackTokenProvider
// ---------------------------------------------------------------------------

// mockTokenProvider is a simple mock for TokenProvider used in fallback tests.
type mockTokenProvider struct {
	resp *TokenResponse
	err  error
}

func (m *mockTokenProvider) IssueToken(_ context.Context, _ TokenRequest) (*TokenResponse, error) {
	return m.resp, m.err
}

func TestNewFallbackTokenProvider(t *testing.T) {
	primary := &mockTokenProvider{}
	provider := NewFallbackTokenProvider(primary)
	require.NotNil(t, provider)
	fp, ok := provider.(*fallbackTokenProvider)
	require.True(t, ok)
	assert.Equal(t, primary, fp.primary)
	assert.NotNil(t, fp.fallback, "fallback should be a UUID provider")
}

func TestFallbackTokenProvider_PrimarySuccess(t *testing.T) {
	expected := &TokenResponse{RequestID: "req-1", AccessToken: "primary-token"}
	primary := &mockTokenProvider{resp: expected}
	provider := NewFallbackTokenProvider(primary)

	resp, err := provider.IssueToken(context.Background(), TokenRequest{})
	require.NoError(t, err)
	assert.Equal(t, expected, resp, "should return primary provider's response")
}

func TestFallbackTokenProvider_PrimaryError_FallbackToUUID(t *testing.T) {
	primary := &mockTokenProvider{err: fmt.Errorf("primary down")}
	provider := NewFallbackTokenProvider(primary)

	resp, err := provider.IssueToken(context.Background(), TokenRequest{})
	require.NoError(t, err, "fallback should not return error")
	require.NotNil(t, resp)
	assert.NotEmpty(t, resp.AccessToken, "fallback should produce a UUID token")
}
