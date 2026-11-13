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
	"time"

	"github.com/openkruise/agents/pkg/identity"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// RefreshServingTrafficAccessTokenOptions identifies the caller and Serving for an
// explicit traffic-token refresh.
type RefreshServingTrafficAccessTokenOptions struct {
	Namespace string
	ServingID string
	User      string
}

// RefreshServingTrafficAccessToken issues a new token for the authorized Serving
// without mutating the Serving, its member Sandboxes or Sessions.
func (m *SandboxManager) RefreshServingTrafficAccessToken(ctx context.Context, opts RefreshServingTrafficAccessTokenOptions) (infra.TrafficAccessToken, error) {
	if opts.User == "" {
		return infra.TrafficAccessToken{}, managererrors.NewError(managererrors.ErrorBadRequest, "user is required")
	}
	if opts.ServingID == "" {
		return infra.TrafficAccessToken{}, managererrors.NewError(managererrors.ErrorBadRequest, "serving ID is required")
	}
	issueCtx, cancel := context.WithTimeout(ctx, defaultTrafficTokenIssueTimeout)
	defer cancel()
	if err := issueCtx.Err(); err != nil {
		return infra.TrafficAccessToken{}, managererrors.WrapError(managererrors.ErrorUnavailable, err, "Serving token request canceled")
	}
	serving, err := m.GetServing(issueCtx, opts.Namespace, opts.ServingID)
	if err != nil {
		return infra.TrafficAccessToken{}, err
	}
	response, err := identity.IssueServingToken(issueCtx, identity.ServingTokenScope{
		ServingID:  serving.ID,
		ServingUID: serving.UID,
		Subject:    opts.User,
	}, identity.TokenOptions{
		Kind:              identity.TokenKindAccessToken,
		RequestedValidity: m.trafficTokenOptions.RequestedValidity,
	})
	if err != nil {
		return infra.TrafficAccessToken{}, managererrors.WrapError(managererrors.ErrorUnavailable, err, "failed to issue Serving traffic access token")
	}
	if err := issueCtx.Err(); err != nil {
		return infra.TrafficAccessToken{}, managererrors.WrapError(managererrors.ErrorUnavailable, err, "Serving token request canceled")
	}
	if response == nil || response.AccessToken == "" {
		return infra.TrafficAccessToken{}, managererrors.NewError(managererrors.ErrorUnavailable, "Serving token issuer returned an empty token")
	}
	expiration, err := time.Parse(time.RFC3339, response.AccessTokenExpiration)
	if err != nil {
		return infra.TrafficAccessToken{}, managererrors.WrapError(managererrors.ErrorUnavailable, err, "Serving token issuer returned an invalid expiration")
	}
	if !expiration.After(time.Now()) {
		return infra.TrafficAccessToken{}, managererrors.NewError(managererrors.ErrorUnavailable, "Serving token issuer returned an expired token")
	}
	return infra.TrafficAccessToken{Token: response.AccessToken, Expiration: expiration}, nil
}
