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

package secureprovider

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// Traffic access token issuance policy defaults. They shape the JWT the
// remote identity provider mints for reaching the sandbox through the
// gateway. They are intentionally named constants so operators can tune them
// without hunting through request-construction code:
//   - the audience must match what the sandbox gateway validates;
//   - the validity is the requested token lifetime.
const (
	defaultTrafficTokenAudience = "sandbox-gateway"
	defaultTrafficTokenValidity = time.Hour * 24 * 365 * 100
)

// trafficAccessTokenRequest is the request payload for the
// ActionIssueTrafficAccessToken ("GetIdentityAccessToken") action.
// Unlike the ID-token request (identity.TokenRequest), it carries no TokenType;
// instead it supplies the JWT-shaping fields the gateway access token needs:
// subject (jwt "sub"), audience (jwt "aud") and the requested token validity.
type trafficAccessTokenRequest struct {
	// Sandbox is the sandbox workload projection. Optional so principal paths
	// that omit the workload object do not send a bare/invalid sandbox block.
	Sandbox *identity.SandboxInfo `json:"sandbox,omitempty"`

	// Metadata carries additional key-value pairs (e.g. role) the identity
	// provider may embed in the token for fine-grained authorization.
	Metadata map[string]string `json:"metadata,omitempty"`

	// Subject is the value used for the JWT "sub" claim. Required.
	Subject string `json:"subject"`

	// Audience is the value used for the JWT "aud" claim. Required.
	Audience string `json:"audience"`

	// TokenValidity is the requested token lifetime in Go time.Duration string
	// form (e.g. "1h"). Required.
	TokenValidity string `json:"tokenValidity"`
}

// trafficAccessTokenResponse is the response payload returned by the remote
// identity provider for the ActionIssueTrafficAccessToken action. Its shape
// differs from identity.TokenResponse (the token lands in "token", not
// "accessToken"), so IssueTrafficAccessToken maps it explicitly.
type trafficAccessTokenResponse struct {
	// RequestID is the upstream-assigned correlation id, surfaced in logs.
	RequestID string `json:"requestId"`

	// Token is the minted gateway access token (a JWT).
	Token string `json:"token"`

	// ExpiresAt is the token expiration time in RFC3339 format.
	ExpiresAt string `json:"expiresAt"`

	// ExpiresIn is the token lifetime in seconds. Redundant with ExpiresAt and
	// currently unused by the mapping; kept so the field is decoded rather than
	// silently dropped.
	ExpiresIn int64 `json:"expiresIn"`
}

// buildTrafficAccessTokenRequest assembles the gateway access-token request from
// the sandbox object. The sandbox projection and security metadata are shared
// with the ID-token path (sandboxInfoFromSandbox / ExtractSecurityMetadata),
// while subject/audience come from the identity provider policy defaults. validity is
// the lifetime resolved by sandbox-manager for this issuance; a non-positive
// value means the caller expressed no preference and the identity provider default
// applies. A nil sbx yields a bare request carrying only the policy fields so
// refresh / principal paths that omit the workload object do not panic.
func buildTrafficAccessTokenRequest(sbx *agentsv1alpha1.Sandbox, validity time.Duration) trafficAccessTokenRequest {
	if validity <= 0 {
		validity = defaultTrafficTokenValidity
	}
	req := trafficAccessTokenRequest{
		Audience:      defaultTrafficTokenAudience,
		TokenValidity: validity.String(),
	}
	if sbx == nil {
		return req
	}
	info := sandboxInfoFromSandbox(sbx)
	req.Sandbox = info
	req.Metadata = identity.ExtractSecurityMetadata(sbx)
	req.Subject = fmt.Sprintf("e2b:controlplane:%s", info.SandboxID)
	return req
}

// IssueTrafficAccessToken mints the gateway access token via the
// ActionIssueTrafficAccessToken action and maps the distinct
// response shape onto the shared identity.TokenResponse: the JWT lands in
// AccessToken and expiresAt (already RFC3339) in AccessTokenExpiration.
func (p *identityProviderTokenProvider) IssueTrafficAccessToken(ctx context.Context, req trafficAccessTokenRequest) (*identity.TokenResponse, error) {
	logger := klog.FromContext(ctx)
	start := time.Now()

	var resp trafficAccessTokenResponse
	if err := p.client.Invoke(ctx, ActionIssueGenericAccessToken, req, &resp); err != nil {
		return nil, err
	}
	if resp.Token == "" {
		return nil, fmt.Errorf("identity provider returned empty traffic access token")
	}

	logger.Info("traffic access token issued from identity provider",
		"requestId", resp.RequestID, "expiresAt", resp.ExpiresAt, "cost", time.Since(start))
	return &identity.TokenResponse{
		RequestID:             resp.RequestID,
		AccessToken:           resp.Token,
		AccessTokenExpiration: resp.ExpiresAt,
	}, nil
}
