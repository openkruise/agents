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
				SecurityMetadataPrefix + "tenant":  "t1",
				SecurityMetadataPrefix + "project": "p1",
				"app":                              "demo",        // non-security label, must be filtered out
				"kubernetes.io/managed-by":         "sandbox-mgr", // non-security label, must be filtered out
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

	// Only labels prefixed with SecurityMetadataPrefix must flow into Metadata.
	assert.Equal(t, map[string]string{
		SecurityMetadataPrefix + "tenant":  "t1",
		SecurityMetadataPrefix + "project": "p1",
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
// every label that is not under identity.SecurityMetadataPrefix, even when label
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
				"agents.kruise.io/team":          "infra", // shares root domain but lacks "security." prefix
				"security-fake.agents.kruise.io": "no",    // close-but-not-equal prefix
				"x-security.agents.kruise.io/y":  "no",    // prefix is not at the start
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

// propagatingFakeProvider is a stand-alone IdentityProvider whose IssueToken is
// intentionally inert (it must NEVER be invoked by PropagateSandboxToken) and
// whose PropagateSecurityToken is fully programmable — including a call counter
// so tests can pin down "exactly one delegation per call".
type propagatingFakeProvider struct {
	gotSandbox *agentsv1alpha1.Sandbox
	gotResp    *TokenResponse
	calls      int
	issueCalls int

	err error
}

func (p *propagatingFakeProvider) IssueToken(_ context.Context, _ TokenRequest) (*TokenResponse, error) {
	p.issueCalls++
	return nil, nil
}

func (p *propagatingFakeProvider) PropagateSecurityToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, resp *TokenResponse) error {
	p.calls++
	p.gotSandbox = sbx
	p.gotResp = resp
	return p.err
}

var _ IdentityProvider = (*propagatingFakeProvider)(nil)

// TestPropagateSandboxToken locks down the contract that callers (claim flow
// and the security-token refresh controller) rely on:
//   - the helper delegates to the registered IdentityProvider exactly once;
//   - on success it returns nil and never touches the issuance path;
//   - on provider failure it surfaces the underlying error VERBATIM (no
//     wrapping with fmt.Errorf), so callers can keep matching against stable
//     error strings or unwrap with errors.Is.
func TestPropagateSandboxToken(t *testing.T) {
	tests := []struct {
		name        string
		fake        *propagatingFakeProvider
		tokenResp   *TokenResponse
		expectError string
	}{
		{
			name: "success delegates to provider and returns nil",
			fake: &propagatingFakeProvider{},
			tokenResp: &TokenResponse{
				AccessToken:           "tok",
				AccessTokenExpiration: "2099-12-31T23:59:59Z",
			},
		},
		{
			name: "provider error is returned verbatim without wrapping",
			fake: &propagatingFakeProvider{
				err: errors.New("write to runtime failed"),
			},
			tokenResp:   &TokenResponse{AccessToken: "tok"},
			expectError: "write to runtime failed",
		},
		{
			name:      "nil tokenResp still reaches the provider for it to decide",
			fake:      &propagatingFakeProvider{},
			tokenResp: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := provider
			RegisterProvider(tt.fake)
			t.Cleanup(func() { RegisterProvider(saved) })

			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx-propagate",
					Namespace: "default",
					UID:       types.UID("uid-propagate"),
				},
			}

			err := PropagateSandboxToken(context.Background(), sbx, tt.tokenResp)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.EqualError(t, err, tt.expectError,
					"helper must surface the provider error VERBATIM (no wrapping)")
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, 1, tt.fake.calls,
				"helper must invoke provider.PropagateSecurityToken exactly once regardless of outcome")
			assert.Equal(t, 0, tt.fake.issueCalls,
				"helper must not touch the issuance path")
			assert.Same(t, sbx, tt.fake.gotSandbox,
				"helper must forward the original sandbox pointer without copying")
			assert.Same(t, tt.tokenResp, tt.fake.gotResp,
				"helper must forward the original TokenResponse pointer without copying")
		})
	}
}

// TestIsIdentityProviderRequested verifies the opt-in predicate that gates the
// identity provider issuance path. The contract is: a sandbox opts in iff its
// Labels carry a non-empty value under LabelAgentName; every other shape
// (nil sandbox, missing Labels map, absent key, empty value, near-miss key)
// must collapse to false so callers can safely short-circuit.
func TestIsIdentityProviderRequested(t *testing.T) {
	tests := []struct {
		name   string
		sbx    *agentsv1alpha1.Sandbox
		want   bool
		reason string
	}{
		{
			name:   "nil sandbox returns false",
			sbx:    nil,
			want:   false,
			reason: "a nil sandbox must never trigger the provider path",
		},
		{
			name: "sandbox without Labels map returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
			},
			want:   false,
			reason: "GetLabels() on a sandbox without ObjectMeta.Labels yields a nil map; lookup must be false",
		},
		{
			name: "empty Labels map returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Labels:    map[string]string{},
				},
			},
			want:   false,
			reason: "an explicitly empty map carries no opt-in signal",
		},
		{
			name: "agent-name label absent returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Labels: map[string]string{
						"app":                             "demo",
						SecurityMetadataPrefix + "tenant": "t1",
					},
				},
			},
			want:   false,
			reason: "other security-prefixed labels must not opt the sandbox into the provider path",
		},
		{
			name: "agent-name label present but empty returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Labels: map[string]string{
						LabelAgentName: "",
					},
				},
			},
			want:   false,
			reason: "empty value carries no agent identity; opt-in must require a non-empty value",
		},
		{
			name: "near-miss key with same prefix returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Labels: map[string]string{
						SecurityMetadataPrefix + "agent-name-suffix": "foo",
						"agent-name": "bar",
					},
				},
			},
			want:   false,
			reason: "the predicate matches the FQ key exactly; near-miss keys must not trigger the path",
		},
		{
			name: "agent-name label present with value returns true",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Labels: map[string]string{
						LabelAgentName: "my-agent",
					},
				},
			},
			want:   true,
			reason: "the canonical opt-in: a non-empty agent-name value",
		},
		{
			name: "agent-name label coexisting with unrelated labels returns true",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Labels: map[string]string{
						LabelAgentName:                     "agent-x",
						SecurityMetadataPrefix + "tenant": "t1",
						"app":                             "demo",
					},
				},
			},
			want:   true,
			reason: "presence of other labels must not interfere with the opt-in decision",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsIdentityProviderRequested(tt.sbx), tt.reason)
		})
	}
}
