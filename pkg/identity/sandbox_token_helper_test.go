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
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// fakeIdentityProvider is a minimal IdentityProvider stub used to capture the
// TokenRequest passed to IssueToken and to deterministically control the
// returned TokenResponse / error. Only the IssueToken path is exercised by
// IssueSandboxToken; PropagateSecurityToken is implemented as a no-op.
type fakeIdentityProvider struct {
	gotReq TokenRequest
	called int

	resp *TokenResponse
	err  error
}

func (f *fakeIdentityProvider) IssueToken(_ context.Context, req TokenRequest) (*TokenResponse, error) {
	f.gotReq = req
	f.called++
	return f.resp, f.err
}

func (f *fakeIdentityProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
	return nil
}

// withFakeProvider swaps the package-level provider with the given fake for the
// duration of the test, restoring the original on cleanup.
func withFakeProvider(t *testing.T, fake *fakeIdentityProvider) {
	t.Helper()
	saved := provider
	RegisterProvider(fake)
	t.Cleanup(func() { RegisterProvider(saved) })
}

// TestIssueSandboxToken_Success exercises the happy path: the helper must
// project the sandbox identity into the TokenRequest, propagate security-prefixed
// labels into Metadata, and return the provider's response unchanged together
// with a non-negative cost and a nil error.
func TestIssueSandboxToken_Success(t *testing.T) {
	wantResp := &TokenResponse{
		RequestID:             "req-1",
		AccessToken:           "tok-1",
		SandboxClientID:       "client-1",
		AccessTokenExpiration: "2099-01-01T00:00:00Z",
	}
	fake := &fakeIdentityProvider{resp: wantResp}
	withFakeProvider(t, fake)

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-a",
			Namespace: "ns-a",
			UID:       types.UID("uid-a"),
			Labels: map[string]string{
				utils.SecurityMetadataPrefix + "tenant":  "t1",
				utils.SecurityMetadataPrefix + "project": "p1",
				"app":                                    "demo",         // non-security label, must be filtered out
				"kubernetes.io/managed-by":               "sandbox-mgr",  // non-security label, must be filtered out
			},
		},
	}

	gotResp, cost, err := IssueSandboxToken(context.Background(), sbx)
	require.NoError(t, err)
	require.NotNil(t, gotResp)
	assert.Same(t, wantResp, gotResp, "response must be returned as-is from the provider")
	assert.GreaterOrEqual(t, int64(cost), int64(0), "cost should be non-negative")
	assert.Equal(t, 1, fake.called, "underlying provider must be called exactly once")

	// Verify the TokenRequest projection.
	gotReq := fake.gotReq
	assert.Equal(t, TokenTypeAgent, gotReq.TokenType, "TokenType must be Agent for sandbox issuance")
	require.NotNil(t, gotReq.Sandbox)
	assert.Equal(t, "sbx-a", gotReq.Sandbox.PodName)
	assert.Equal(t, "ns-a", gotReq.Sandbox.PodNamespace)
	assert.Equal(t, "ns-a/sbx-a/uid-a", gotReq.Sandbox.SandboxID,
		"SandboxID must follow the canonical namespace/name/uid layout")
	assert.Equal(t, "sbx-a", gotReq.Sandbox.SandboxName)
	assert.Equal(t, "uid-a", gotReq.Sandbox.SandboxUID)

	// Only labels prefixed with utils.SecurityMetadataPrefix must flow into Metadata.
	assert.Equal(t, map[string]string{
		utils.SecurityMetadataPrefix + "tenant":  "t1",
		utils.SecurityMetadataPrefix + "project": "p1",
	}, gotReq.Metadata)
}

// TestIssueSandboxToken_NoLabels guarantees that a sandbox without any labels
// still produces a non-nil (empty) Metadata map. This matters because downstream
// providers may type-assert on a non-nil map and the helper documents that the
// caller does not need to pre-populate Metadata.
func TestIssueSandboxToken_NoLabels(t *testing.T) {
	fake := &fakeIdentityProvider{resp: &TokenResponse{AccessToken: "tok"}}
	withFakeProvider(t, fake)

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-empty",
			Namespace: "ns",
			UID:       types.UID("uid-empty"),
		},
	}

	_, _, err := IssueSandboxToken(context.Background(), sbx)
	require.NoError(t, err)
	require.NotNil(t, fake.gotReq.Metadata, "Metadata must be a non-nil map even when no labels are present")
	assert.Empty(t, fake.gotReq.Metadata)
}

// TestIssueSandboxToken_OnlyNonSecurityLabels verifies the prefix filter rejects
// every label that is not under utils.SecurityMetadataPrefix, even when label
// values look plausible (e.g. share a substring with the prefix).
func TestIssueSandboxToken_OnlyNonSecurityLabels(t *testing.T) {
	fake := &fakeIdentityProvider{resp: &TokenResponse{AccessToken: "tok"}}
	withFakeProvider(t, fake)

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx",
			Namespace: "ns",
			UID:       types.UID("uid"),
			Labels: map[string]string{
				"app":                            "demo",
				"agents.kruise.io/team":          "infra",                      // shares root domain but lacks "security." prefix
				"security-fake.agents.kruise.io": "no",                         // close-but-not-equal prefix
				"x-security.agents.kruise.io/y":  "no",                         // prefix is not at the start
			},
		},
	}

	_, _, err := IssueSandboxToken(context.Background(), sbx)
	require.NoError(t, err)
	assert.Empty(t, fake.gotReq.Metadata, "labels without the SecurityMetadataPrefix must be filtered out")
}

// TestIssueSandboxToken_ProviderError guarantees the helper surfaces provider
// errors wrapped with the documented message and returns a nil response so that
// callers never accidentally persist a stale or zero-value token.
func TestIssueSandboxToken_ProviderError(t *testing.T) {
	rootErr := errors.New("identity provider unavailable")
	fake := &fakeIdentityProvider{err: rootErr}
	withFakeProvider(t, fake)

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-err",
			Namespace: "ns",
			UID:       types.UID("uid-err"),
		},
	}

	gotResp, cost, err := IssueSandboxToken(context.Background(), sbx)
	require.Error(t, err)
	assert.Nil(t, gotResp, "response must be nil on error to prevent persisting a zero-value token")
	assert.GreaterOrEqual(t, int64(cost), int64(0), "cost must still be reported even on failure for metric accounting")

	// Wrap message must remain stable; downstream code matches against this prefix.
	assert.Contains(t, err.Error(), "failed to issue security token")
	assert.True(t, errors.Is(err, rootErr), "wrapped error must preserve the original cause via errors.Is")
}

// TestIssueSandboxToken_DefaultProviderIntegration sanity-checks the helper
// against the real defaultTokenProvider (no fake), to ensure the integration
// with the package-level provider variable is wired correctly when tests do
// not replace the provider explicitly.
func TestIssueSandboxToken_DefaultProviderIntegration(t *testing.T) {
	// Reset to the community default for this test so prior tests cannot leak
	// state via the package-level provider variable.
	saved := provider
	RegisterProvider(NewDefaultIdentityProvider())
	t.Cleanup(func() { RegisterProvider(saved) })

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-default",
			Namespace: "ns-default",
			UID:       types.UID("uid-default"),
		},
	}

	resp, _, err := IssueSandboxToken(context.Background(), sbx)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.NotEmpty(t, resp.AccessToken, "default provider must mint a non-empty access token")
	assert.NotEmpty(t, resp.RequestID, "default provider must mint a non-empty request id")
}

// Compile-time guard: fakeIdentityProvider must satisfy IdentityProvider so it
// is accepted by RegisterProvider.
var _ IdentityProvider = (*fakeIdentityProvider)(nil)
