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
	"strings"
	"time"

	"github.com/google/uuid"
)

// SessionServiceTokenScope identifies a SessionService incarnation and the authenticated caller.
// It is independent of any Sandbox delivery or transport protocol.
type SessionServiceTokenScope struct {
	SessionServiceID  string
	SessionServiceUID string
	// Subject is the authenticated caller, not the resource-bound JWT subject.
	Subject string
}

// SessionServiceTokenSubject identifies the control-plane SessionService principal. The signed
// typed sessionService claims sessionServiceId and sessionServiceUid bind the resource incarnation.
func SessionServiceTokenSubject(sessionServiceID string) (string, error) {
	if sessionServiceID == "" {
		return "", errors.New("SessionService ID is required")
	}
	if strings.Contains(sessionServiceID, ":") {
		return "", errors.New("SessionService ID must not contain a colon")
	}
	return "sessionservice:controlplane:" + sessionServiceID, nil
}

// SessionServiceTokenIssuer is an optional capability of the startup-registered provider.
type SessionServiceTokenIssuer interface {
	IssueSessionServiceToken(context.Context, SessionServiceTokenScope, TokenOptions) (*TokenResponse, error)
}

// ErrSessionServiceTokenUnsupported means the registered provider cannot issue SessionService tokens.
var ErrSessionServiceTokenUnsupported = errors.New("identity provider does not support SessionService tokens")

// IssueSessionServiceToken delegates exclusively to the registered provider.
func IssueSessionServiceToken(ctx context.Context, scope SessionServiceTokenScope, opts TokenOptions) (*TokenResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	issuer, ok := provider.(SessionServiceTokenIssuer)
	if !ok {
		return nil, ErrSessionServiceTokenUnsupported
	}
	return issuer.IssueSessionServiceToken(ctx, scope, opts)
}

// IssueSessionServiceToken returns the community default random placeholder token.
// Like Sandbox issuance, this default requires a configured signing provider for JWT verification.
func (u *defaultTokenProvider) IssueSessionServiceToken(ctx context.Context, _ SessionServiceTokenScope, opts TokenOptions) (*TokenResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	validity := defaultAccessTokenLifetime
	if opts.RequestedValidity > 0 {
		validity = opts.RequestedValidity
	}
	return &TokenResponse{
		RequestID:             uuid.NewString(),
		AccessToken:           uuid.NewString(),
		AccessTokenExpiration: time.Now().Add(validity).Format(time.RFC3339),
	}, nil
}
