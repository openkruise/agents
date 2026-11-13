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

// ServingTokenScope identifies a Serving incarnation and the authenticated caller.
// It is independent of any Sandbox delivery or transport protocol.
type ServingTokenScope struct {
	ServingID  string
	ServingUID string
	// Subject is the authenticated caller, not the resource-bound JWT subject.
	Subject string
}

// ServingTokenSubject identifies the control-plane Serving principal. The signed
// metadata servingId and servingUid separately bind the resource incarnation.
func ServingTokenSubject(servingID string) (string, error) {
	if servingID == "" {
		return "", errors.New("Serving ID is required")
	}
	if strings.Contains(servingID, ":") {
		return "", errors.New("Serving ID must not contain a colon")
	}
	return "serving:controlplane:" + servingID, nil
}

// ServingTokenIssuer is an optional capability of the startup-registered provider.
type ServingTokenIssuer interface {
	IssueServingToken(context.Context, ServingTokenScope, TokenOptions) (*TokenResponse, error)
}

// ErrServingTokenUnsupported means the registered provider cannot issue Serving tokens.
var ErrServingTokenUnsupported = errors.New("identity provider does not support Serving tokens")

// IssueServingToken delegates exclusively to the registered provider.
func IssueServingToken(ctx context.Context, scope ServingTokenScope, opts TokenOptions) (*TokenResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	issuer, ok := provider.(ServingTokenIssuer)
	if !ok {
		return nil, ErrServingTokenUnsupported
	}
	return issuer.IssueServingToken(ctx, scope, opts)
}

// IssueServingToken returns the community default random placeholder token.
// Like Sandbox issuance, this default requires a configured signing provider for JWT verification.
func (u *defaultTokenProvider) IssueServingToken(ctx context.Context, _ ServingTokenScope, opts TokenOptions) (*TokenResponse, error) {
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
