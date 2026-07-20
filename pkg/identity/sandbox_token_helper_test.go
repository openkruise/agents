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
// arguments passed to IssueToken and to deterministically control the returned
// TokenResponse / error. Only the IssueToken path is exercised by
// IssueSandboxToken; PropagateSecurityToken is implemented as a no-op.
type fakeIdentityProvider struct {
	gotSbx *agentsv1alpha1.Sandbox
	called int

	resp *TokenResponse
	err  error
}

func (f *fakeIdentityProvider) IssueToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, _ TokenKind) (*TokenResponse, error) {
	f.gotSbx = sbx
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
// forward the sandbox to the provider unchanged and return the provider's
// response together with a nil error. Building the TokenRequest is left
// entirely to the provider.
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
		},
	}

	gotResp, err := IssueSandboxToken(context.Background(), sbx)
	require.NoError(t, err)
	require.NotNil(t, gotResp)
	assert.Same(t, wantResp, gotResp, "response must be returned as-is from the provider")
	assert.Equal(t, 1, fake.called, "underlying provider must be called exactly once")
	assert.Same(t, sbx, fake.gotSbx, "sandbox pointer must be forwarded unchanged to the provider")
}

// kindCapturingProvider is an IdentityProvider stub that additionally records
// the TokenKind it was invoked with, so tests can assert that
// IssueSandboxAccessToken selects TokenKindAccessToken rather than the
// ID-token kind. PropagateSecurityToken is a no-op; only the issuance path is
// exercised here.
type kindCapturingProvider struct {
	gotSbx  *agentsv1alpha1.Sandbox
	gotKind TokenKind
	called  int

	resp *TokenResponse
	err  error
}

func (p *kindCapturingProvider) IssueToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, kind TokenKind) (*TokenResponse, error) {
	p.gotSbx = sbx
	p.gotKind = kind
	p.called++
	return p.resp, p.err
}

func (p *kindCapturingProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
	return nil
}

var _ IdentityProvider = (*kindCapturingProvider)(nil)

// TestIssueSandboxAccessToken exercises the access-token twin of
// IssueSandboxToken. It shares the same registered IdentityProvider but must
// select TokenKindAccessToken and, on failure, wrap the provider error with the
// distinct "failed to issue access token" prefix while preserving the cause via
// errors.Is. Like its twin, it must forward the sandbox pointer unchanged, call
// the provider exactly once, return the response as-is on success, and return a
// nil response on error so callers never persist a zero-value token.
func TestIssueSandboxAccessToken(t *testing.T) {
	wantResp := &TokenResponse{
		RequestID:             "req-access",
		AccessToken:           "access-tok",
		SandboxClientID:       "client-access",
		AccessTokenExpiration: "2099-01-01T00:00:00Z",
	}
	rootErr := errors.New("identity provider unavailable")

	tests := []struct {
		name        string
		fake        *kindCapturingProvider
		wantResp    *TokenResponse
		expectError string
		wantCause   error
	}{
		{
			name:     "success returns provider response as-is",
			fake:     &kindCapturingProvider{resp: wantResp},
			wantResp: wantResp,
		},
		{
			name:        "provider error is wrapped with access-token prefix",
			fake:        &kindCapturingProvider{err: rootErr},
			expectError: "failed to issue access token",
			wantCause:   rootErr,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			saved := provider
			RegisterProvider(tt.fake)
			t.Cleanup(func() { RegisterProvider(saved) })

			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx-access",
					Namespace: "ns-access",
					UID:       types.UID("uid-access"),
				},
			}

			gotResp, err := IssueSandboxAccessToken(context.Background(), sbx)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Nil(t, gotResp, "response must be nil on error to prevent persisting a zero-value token")
				assert.Contains(t, err.Error(), tt.expectError,
					"wrap message must remain stable for downstream phase classification")
				assert.True(t, errors.Is(err, tt.wantCause),
					"wrapped error must preserve the original cause via errors.Is")
			} else {
				require.NoError(t, err)
				require.NotNil(t, gotResp)
				assert.Same(t, tt.wantResp, gotResp, "response must be returned as-is from the provider")
			}

			assert.Equal(t, 1, tt.fake.called, "underlying provider must be called exactly once")
			assert.Same(t, sbx, tt.fake.gotSbx, "sandbox pointer must be forwarded unchanged to the provider")
			assert.Equal(t, TokenKindAccessToken, tt.fake.gotKind,
				"IssueSandboxAccessToken must select the access-token kind, not the ID-token kind")
		})
	}
}

// TestIssueSandboxAccessToken_DefaultProviderIntegration sanity-checks the
// helper against the real defaultTokenProvider (no fake) to ensure the
// package-level provider wiring works and the community default mints a
// non-empty, effectively non-expiring access token.
func TestIssueSandboxAccessToken_DefaultProviderIntegration(t *testing.T) {
	saved := provider
	RegisterProvider(NewDefaultIdentityProvider())
	t.Cleanup(func() { RegisterProvider(saved) })

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-access-default",
			Namespace: "ns-default",
			UID:       types.UID("uid-access-default"),
		},
	}

	resp, err := IssueSandboxAccessToken(context.Background(), sbx)
	require.NoError(t, err)
	require.NotNil(t, resp)
	assert.NotEmpty(t, resp.AccessToken, "default provider must mint a non-empty access token")
	assert.NotEmpty(t, resp.RequestID, "default provider must mint a non-empty request id")
	assert.NotEmpty(t, resp.AccessTokenExpiration, "default provider must stamp an expiration on the access token")
}

// TestExtractSecurityMetadata verifies the prefix filter used to collect
// security-prefixed annotations into a metadata map. Providers call this helper
// when they want to include security metadata in token issuance requests.
func TestExtractSecurityMetadata(t *testing.T) {
	const tenantKey = SecurityMetadataPrefix + "tenant"
	const projectKey = SecurityMetadataPrefix + "project"

	tests := []struct {
		name    string
		sbx     *agentsv1alpha1.Sandbox
		want    map[string]string
		wantNil bool
	}{
		{
			name: "nil sandbox returns nil",
			sbx:  nil,
			want: nil,
		},
		{
			name: "sandbox without annotations returns empty map",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
			},
			want: map[string]string{},
		},
		{
			name: "only security-prefixed annotations are collected",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Annotations: map[string]string{
						tenantKey:  "t1",
						projectKey: "p1",
						"app":      "demo",
					},
				},
			},
			want: map[string]string{
				tenantKey:  "t1",
				projectKey: "p1",
			},
		},
		{
			name: "near-miss keys are rejected",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Annotations: map[string]string{
						"agents.kruise.io/team":          "infra",
						"security-fake.agents.kruise.io": "no",
						"x-security.agents.kruise.io/y":  "no",
					},
				},
			},
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractSecurityMetadata(tt.sbx)
			if tt.want == nil {
				assert.Nil(t, got)
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got)
		})
	}
}

// TestExtractSecurityMetadataFromMap verifies the prefix filter that backs both
// ExtractSecurityMetadata and the caller-supplied E2B input path. The returned
// map must be non-nil even when no entry matches, and must contain only
// security-prefixed keys.
func TestExtractSecurityMetadataFromMap(t *testing.T) {
	const agentKey = SecurityMetadataPrefix + "agent-name"
	const tenantKey = SecurityMetadataPrefix + "tenant"

	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{
			name: "nil map returns empty map",
			in:   nil,
			want: map[string]string{},
		},
		{
			name: "only security-prefixed keys are collected",
			in: map[string]string{
				agentKey:  "agent-a",
				tenantKey: "t1",
				"app":     "demo",
			},
			want: map[string]string{
				agentKey:  "agent-a",
				tenantKey: "t1",
			},
		},
		{
			name: "near-miss keys are rejected",
			in: map[string]string{
				"agents.kruise.io/team":         "infra",
				"x-security.agents.kruise.io/y": "no",
			},
			want: map[string]string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractSecurityMetadataFromMap(tt.in)
			require.NotNil(t, got, "returned map must never be nil")
			assert.Equal(t, tt.want, got)
		})
	}
}

// annotationReadingProvider is an IdentityProvider that extracts a specific
// annotation from the sandbox and records it so tests can verify the sandbox
// object forwarded to the provider is complete and readable.
type annotationReadingProvider struct {
	storageAuthKey string
	gotValue       string
}

func (p *annotationReadingProvider) IssueToken(_ context.Context, sbx *agentsv1alpha1.Sandbox, _ TokenKind) (*TokenResponse, error) {
	p.gotValue = sbx.GetAnnotations()[p.storageAuthKey]
	return &TokenResponse{AccessToken: "tok"}, nil
}

func (p *annotationReadingProvider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *TokenResponse) error {
	return nil
}

var _ IdentityProvider = (*annotationReadingProvider)(nil)

// TestIssueSandboxToken_ProviderCanReadSandboxAnnotations verifies that the
// sandbox pointer forwarded to IdentityProvider carries annotations injected by
// upstream callers (e.g. storage-auth metadata for agent-identity storage mounts).
// This replaces the legacy ExtractStorageAuthMetadata hook: providers now read
// annotations directly from the sandbox object instead of receiving a second
// metadata map.
func TestIssueSandboxToken_ProviderCanReadSandboxAnnotations(t *testing.T) {
	const storageAuthAnnotationKey = SecurityMetadataPrefix + "storage-auth"

	saved := provider
	reader := &annotationReadingProvider{storageAuthKey: storageAuthAnnotationKey}
	RegisterProvider(reader)
	t.Cleanup(func() { RegisterProvider(saved) })

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sbx-annotation",
			Namespace: "ns",
			UID:       types.UID("uid-annotation"),
			Annotations: map[string]string{
				storageAuthAnnotationKey: `[{"credentialProviderName":"my-provider"}]`,
			},
		},
	}

	_, err := IssueSandboxToken(context.Background(), sbx)
	require.NoError(t, err)
	assert.Equal(t, `[{"credentialProviderName":"my-provider"}]`, reader.gotValue,
		"provider must be able to read annotations directly from the forwarded sandbox")
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

	gotResp, err := IssueSandboxToken(context.Background(), sbx)
	require.Error(t, err)
	assert.Nil(t, gotResp, "response must be nil on error to prevent persisting a zero-value token")

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

	resp, err := IssueSandboxToken(context.Background(), sbx)
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

func (p *propagatingFakeProvider) IssueToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ TokenKind) (*TokenResponse, error) {
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

// TestIsIDTokenRequested verifies the opt-in predicate that gates the
// identity provider issuance path. The contract is: a sandbox opts in iff its
// Annotations carry a non-empty value under AnnotationAgentName; every other
// shape (nil sandbox, missing Annotations map, absent key, empty value,
// near-miss key) must collapse to false so callers can safely short-circuit.
func TestIsIDTokenRequested(t *testing.T) {
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
			name: "sandbox without Annotations map returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Name: "sbx", Namespace: "ns"},
			},
			want:   false,
			reason: "GetAnnotations() on a sandbox without ObjectMeta.Annotations yields a nil map; lookup must be false",
		},
		{
			name: "empty Annotations map returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "sbx",
					Namespace:   "ns",
					Annotations: map[string]string{},
				},
			},
			want:   false,
			reason: "an explicitly empty map carries no opt-in signal",
		},
		{
			name: "agent-name annotation absent returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Annotations: map[string]string{
						"app":                             "demo",
						SecurityMetadataPrefix + "tenant": "t1",
					},
				},
			},
			want:   false,
			reason: "other security-prefixed annotations must not opt the sandbox into the provider path",
		},
		{
			name: "agent-name annotation present but empty returns false",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Annotations: map[string]string{
						AnnotationAgentName: "",
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
					Annotations: map[string]string{
						SecurityMetadataPrefix + "agent-name-suffix": "foo",
						"agent-name": "bar",
					},
				},
			},
			want:   false,
			reason: "the predicate matches the FQ key exactly; near-miss keys must not trigger the path",
		},
		{
			name: "agent-name annotation present with value returns true",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Annotations: map[string]string{
						AnnotationAgentName: "my-agent",
					},
				},
			},
			want:   true,
			reason: "the canonical opt-in: a non-empty agent-name value",
		},
		{
			name: "agent-name annotation coexisting with unrelated annotations returns true",
			sbx: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "sbx",
					Namespace: "ns",
					Annotations: map[string]string{
						AnnotationAgentName:               "agent-x",
						SecurityMetadataPrefix + "tenant": "t1",
						"app":                             "demo",
					},
				},
			},
			want:   true,
			reason: "presence of other annotations must not interfere with the opt-in decision",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsIDTokenRequested(tt.sbx), tt.reason)
		})
	}
}
