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
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type servingTestProvider struct {
	IdentityProvider
	scope    SessionServiceTokenScope
	opts     TokenOptions
	response *TokenResponse
	err      error
}

func (p *servingTestProvider) IssueSessionServiceToken(_ context.Context, scope SessionServiceTokenScope, opts TokenOptions) (*TokenResponse, error) {
	p.scope, p.opts = scope, opts
	return p.response, p.err
}

func TestIssueSessionServiceToken(t *testing.T) {
	original := provider
	t.Cleanup(func() { RegisterProvider(original) })
	scope := SessionServiceTokenScope{SessionServiceID: "abcde234abcde", SessionServiceUID: "uid-a", Subject: "caller-a"}
	opts := TokenOptions{Kind: TokenKindAccessToken, RequestedValidity: 10 * time.Minute}
	t.Run("delegates scope and options", func(t *testing.T) {
		p := &servingTestProvider{response: &TokenResponse{AccessToken: "provider-token"}}
		RegisterProvider(p)
		response, err := IssueSessionServiceToken(t.Context(), scope, opts)
		require.NoError(t, err)
		require.Equal(t, "provider-token", response.AccessToken)
		require.Equal(t, scope, p.scope)
		require.Equal(t, opts, p.opts)
	})
	t.Run("provider error has no fallback", func(t *testing.T) {
		cause := errors.New("issuer failed")
		RegisterProvider(&servingTestProvider{err: cause})
		response, err := IssueSessionServiceToken(t.Context(), scope, opts)
		require.ErrorIs(t, err, cause)
		require.Nil(t, response)
	})
	t.Run("unsupported has no sandbox fallback", func(t *testing.T) {
		RegisterProvider(struct{ IdentityProvider }{NewDefaultIdentityProvider()})
		response, err := IssueSessionServiceToken(t.Context(), scope, opts)
		require.ErrorIs(t, err, ErrSessionServiceTokenUnsupported)
		require.Nil(t, response)
	})
	t.Run("default random placeholder and expiry", func(t *testing.T) {
		RegisterProvider(NewDefaultIdentityProvider())
		response, err := IssueSessionServiceToken(t.Context(), scope, opts)
		require.NoError(t, err)
		_, err = uuid.Parse(response.AccessToken)
		require.NoError(t, err)
		expiration, err := time.Parse(time.RFC3339, response.AccessTokenExpiration)
		require.NoError(t, err)
		require.WithinDuration(t, time.Now().Add(10*time.Minute), expiration, time.Second)
		next, err := IssueSessionServiceToken(t.Context(), scope, opts)
		require.NoError(t, err)
		require.NotEqual(t, response.AccessToken, next.AccessToken)
	})
	t.Run("cancellation", func(t *testing.T) {
		RegisterProvider(NewDefaultIdentityProvider())
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		response, err := IssueSessionServiceToken(ctx, scope, opts)
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, response)
	})
}

func TestSessionServiceTokenSubject(t *testing.T) {
	for _, tt := range []struct {
		name, id, want string
		invalid        bool
	}{
		{name: "SessionService principal", id: "abcde234abcde", want: "sessionservice:controlplane:abcde234abcde"},
		{name: "different SessionService", id: "serving-b", want: "sessionservice:controlplane:serving-b"},
		{name: "missing ID", invalid: true},
		{name: "delimiter in ID", id: "a:b", invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			subject, err := SessionServiceTokenSubject(tt.id)
			if tt.invalid {
				require.Error(t, err)
				require.Empty(t, subject)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, subject)
			repeated, err := SessionServiceTokenSubject(tt.id)
			require.NoError(t, err)
			require.Equal(t, subject, repeated)
		})
	}
}
