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

package sandbox_manager

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/identity"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	session "github.com/openkruise/agents/pkg/servingsession"
)

func TestSandboxManagerRefreshServingTrafficAccessTokenRequiresCaller(t *testing.T) {
	manager := &SandboxManager{}
	token, err := manager.RefreshServingTrafficAccessToken(context.Background(), RefreshServingTrafficAccessTokenOptions{ServingID: "serving-a"})
	require.Equal(t, managererrors.ErrorBadRequest, managererrors.GetErrCode(err))
	require.Empty(t, token.Token)
}

// The optional issuer boundary needs a recording provider to verify the exact
// authorized identity passed to an external implementation.
type servingTokenTestProvider struct {
	identity.IdentityProvider
	scope    identity.ServingTokenScope
	opts     identity.TokenOptions
	response *identity.TokenResponse
	err      error
	calls    int
}

func (p *servingTokenTestProvider) IssueServingToken(ctx context.Context, scope identity.ServingTokenScope, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	p.calls++
	p.scope, p.opts = scope, opts
	if _, ok := ctx.Deadline(); !ok {
		return nil, errors.New("missing issuance deadline")
	}
	return p.response, p.err
}

type servingTokenTestBackend struct {
	session.ServingSessionBackend
	namespace, id string
	serving       session.Serving
	err           error
}

func (b *servingTokenTestBackend) GetServing(_ context.Context, namespace, id string) (session.Serving, error) {
	b.namespace, b.id = namespace, id
	return b.serving, b.err
}

func TestSandboxManagerRefreshServingTrafficAccessTokenScopeAndFailures(t *testing.T) {
	t.Cleanup(func() { identity.RegisterProvider(identity.NewDefaultIdentityProvider()) })
	expiry := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	tests := []struct {
		name                   string
		response               *identity.TokenResponse
		providerErr, lookupErr error
		wantCode               managererrors.ErrorCode
		wantCalls              int
	}{
		{name: "authorized scope", response: &identity.TokenResponse{AccessToken: "serving-token", AccessTokenExpiration: expiry.Format(time.RFC3339)}, wantCode: managererrors.ErrorUnknown, wantCalls: 1},
		{name: "lookup stops issuance", lookupErr: session.ErrServingNotFound, wantCode: managererrors.ErrorNotFound},
		{name: "provider failure", providerErr: errors.New("issuer failed"), wantCode: managererrors.ErrorUnavailable, wantCalls: 1},
		{name: "nil response", wantCode: managererrors.ErrorUnavailable, wantCalls: 1},
		{name: "empty token", response: &identity.TokenResponse{AccessTokenExpiration: expiry.Format(time.RFC3339)}, wantCode: managererrors.ErrorUnavailable, wantCalls: 1},
		{name: "bad expiry", response: &identity.TokenResponse{AccessToken: "token", AccessTokenExpiration: "invalid"}, wantCode: managererrors.ErrorUnavailable, wantCalls: 1},
		{name: "expired token", response: &identity.TokenResponse{AccessToken: "token", AccessTokenExpiration: "2000-01-01T00:00:00Z"}, wantCode: managererrors.ErrorUnavailable, wantCalls: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backend := &servingTokenTestBackend{serving: session.Serving{Namespace: "team-a", ID: "serving-a", UID: "uid-a"}, err: tt.lookupErr}
			manager := &SandboxManager{servingBackend: backend, trafficTokenOptions: identity.TokenOptions{RequestedValidity: 10 * time.Minute}}
			provider := &servingTokenTestProvider{response: tt.response, err: tt.providerErr}
			identity.RegisterProvider(provider)
			token, err := manager.RefreshServingTrafficAccessToken(t.Context(), RefreshServingTrafficAccessTokenOptions{Namespace: "team-a", ServingID: "serving-a", User: "caller-key"})
			require.Equal(t, tt.wantCode, managererrors.GetErrCode(err))
			require.Equal(t, tt.wantCalls, provider.calls)
			require.Equal(t, "team-a", backend.namespace)
			require.Equal(t, "serving-a", backend.id)
			if tt.wantCalls > 0 {
				require.Equal(t, identity.ServingTokenScope{ServingID: "serving-a", ServingUID: "uid-a", Subject: "caller-key"}, provider.scope)
				require.Equal(t, identity.TokenOptions{Kind: identity.TokenKindAccessToken, RequestedValidity: 10 * time.Minute}, provider.opts)
			}
			if err == nil {
				require.Equal(t, "serving-token", token.Token)
				require.Equal(t, expiry, token.Expiration)
			} else {
				require.Empty(t, token.Token)
			}
		})
	}
}

func TestSandboxManagerRefreshServingTrafficAccessTokenCanceledBeforeLookup(t *testing.T) {
	manager := &SandboxManager{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	token, err := manager.RefreshServingTrafficAccessToken(ctx, RefreshServingTrafficAccessTokenOptions{Namespace: "team-a", ServingID: "serving-a", User: "caller-key"})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, managererrors.ErrorUnavailable, managererrors.GetErrCode(err))
	require.Empty(t, token.Token)
}
