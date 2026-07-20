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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// defaultTokenProvider
// ---------------------------------------------------------------------------

func TestNewDefaultIdentityProvider(t *testing.T) {
	provider := NewDefaultIdentityProvider()
	require.NotNil(t, provider)
	_, ok := provider.(*defaultTokenProvider)
	assert.True(t, ok, "should return *defaultTokenProvider implementing IdentityProvider")
}

func TestDefaultTokenProvider_IssueToken(t *testing.T) {
	provider := NewDefaultIdentityProvider()
	ctx := context.Background()

	resp, err := provider.IssueToken(ctx, nil, TokenKindIDToken)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.NotEmpty(t, resp.RequestID, "RequestID should be a non-empty string")
	assert.NotEmpty(t, resp.AccessToken, "AccessToken should be a non-empty string")

	// Two calls should produce different tokens.
	resp2, err := provider.IssueToken(ctx, nil, TokenKindIDToken)
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
