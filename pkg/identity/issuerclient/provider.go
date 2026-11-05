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

// Package issuerclient implements identity.IdentityProvider by asking the
// agent-identity-provider service to mint agent tokens.
//
// Issuance is deliberately remote rather than signed in the calling process:
// the service is the only component that holds the signing key, re-reads the
// Sandbox for a request, and decides token lifetime. Callers therefore hold no
// signing material and cannot name an identity to mint for.
package issuerclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/identity/agenttoken"
	"github.com/openkruise/agents/pkg/peersecurity"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

const (
	// defaultTimeout bounds one issuance request. Issuance happens on the claim
	// and resume paths, so a stalled service must surface as an error rather
	// than hold those paths open.
	defaultTimeout = 10 * time.Second

	// maxResponseBytes bounds the response body. The response is one token and
	// a timestamp.
	maxResponseBytes = 1 << 20
)

// Options are the operator-supplied settings for reaching the identity
// provider service.
type Options struct {
	// Endpoint is the absolute HTTPS base URL of the identity provider service.
	// The issuance path is appended to it.
	Endpoint string

	// Bundle supplies the server CA and the client certificate. Both are
	// required: the service only mints for callers that present a verified
	// client certificate, and the caller must be able to verify the service.
	Bundle *peersecurity.TLSBundle

	// Timeout bounds one issuance request. Zero uses defaultTimeout.
	Timeout time.Duration
}

// Enabled reports whether an endpoint was configured at all.
func (o Options) Enabled() bool {
	return o.Endpoint != ""
}

// Provider implements identity.IdentityProvider against the identity provider
// service.
type Provider struct {
	endpoint string
	client   *http.Client

	// fallback serves the token kinds the service does not issue yet. Gateway
	// access tokens have no community issuance endpoint, so delegating keeps the
	// gateway path behaving exactly as it does with the default provider.
	fallback identity.IdentityProvider
}

// New validates opts and returns a Provider.
func New(opts Options) (*Provider, error) {
	if opts.Endpoint == "" {
		return nil, errors.New("issuerclient: endpoint is required")
	}
	parsed, err := url.Parse(opts.Endpoint)
	if err != nil || !parsed.IsAbs() || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("issuerclient: endpoint %q must be an absolute HTTPS URL", opts.Endpoint)
	}
	if opts.Bundle == nil {
		return nil, errors.New("issuerclient: a TLS bundle is required: issuance authenticates the caller with a client certificate")
	}
	rootCAs, clientCert, err := opts.Bundle.Parsed()
	if err != nil {
		return nil, fmt.Errorf("issuerclient: %w", err)
	}
	if clientCert == nil {
		return nil, errors.New("issuerclient: the TLS bundle has no client certificate, which the issuance endpoint requires")
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      rootCAs,
		Certificates: []tls.Certificate{*clientCert},
	}

	return &Provider{
		endpoint: strings.TrimSuffix(opts.Endpoint, "/") + agenttoken.Path,
		client:   &http.Client{Timeout: timeout, Transport: transport},
		fallback: identity.NewDefaultIdentityProvider(),
	}, nil
}

// IssueToken mints the requested token. Agent (ID) tokens are issued by the
// service; every other kind is delegated so this provider changes nothing about
// paths the community has not designed an issuance endpoint for.
func (p *Provider) IssueToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox,
	opts identity.TokenOptions) (*identity.TokenResponse, error) {
	switch opts.Kind {
	case identity.TokenKindIDToken, "":
		return p.issueAgentToken(ctx, sbx)
	case identity.TokenKindAccessToken:
		return p.fallback.IssueToken(ctx, sbx, opts)
	default:
		return nil, fmt.Errorf("issuerclient: unsupported token kind %q", opts.Kind)
	}
}

func (p *Provider) issueAgentToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox) (*identity.TokenResponse, error) {
	if sbx == nil {
		return nil, errors.New("issuerclient: sandbox is required to issue an agent token")
	}
	if sbx.UID == "" {
		return nil, fmt.Errorf("issuerclient: sandbox %s/%s has no UID", sbx.Namespace, sbx.Name)
	}

	body, err := json.Marshal(agenttoken.Request{
		Namespace:   sbx.Namespace,
		SandboxName: sbx.Name,
		SandboxUID:  string(sbx.UID),
	})
	if err != nil {
		return nil, fmt.Errorf("issuerclient: encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("issuerclient: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("issuerclient: request agent token for sandbox %s/%s: %w", sbx.Namespace, sbx.Name, err)
	}
	defer func() {
		// The body is drained below; a close error after a complete response does
		// not change the outcome, and the token is never placed anywhere a
		// failed close could leak it.
		_ = resp.Body.Close()
	}()

	payload, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("issuerclient: read response for sandbox %s/%s: %w", sbx.Namespace, sbx.Name, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, responseError(sbx, resp.Status, payload)
	}

	var out agenttoken.Response
	if err := json.Unmarshal(payload, &out); err != nil {
		return nil, fmt.Errorf("issuerclient: decode response for sandbox %s/%s: %w", sbx.Namespace, sbx.Name, err)
	}
	if out.AgentToken == "" {
		return nil, fmt.Errorf("issuerclient: identity provider returned an empty agent token for sandbox %s/%s",
			sbx.Namespace, sbx.Name)
	}
	return &identity.TokenResponse{
		RequestID:             out.RequestID,
		AccessToken:           out.AgentToken,
		AccessTokenExpiration: out.ExpiresAt,
	}, nil
}

// responseError turns a refusal body into an error that names the reason the
// service reported. The message carries no token material by contract.
func responseError(sbx *agentsv1alpha1.Sandbox, status string, payload []byte) error {
	var body agenttoken.Error
	if err := json.Unmarshal(payload, &body); err == nil && body.Code != "" {
		return fmt.Errorf("issuerclient: issue agent token for sandbox %s/%s: %s (%s): %s",
			sbx.Namespace, sbx.Name, body.Code, status, body.Message)
	}
	return fmt.Errorf("issuerclient: issue agent token for sandbox %s/%s: unexpected status %s",
		sbx.Namespace, sbx.Name, status)
}

// PropagateSecurityToken keeps the community default behaviour: no propagators
// are registered, so issuance leaves writing the credential to the runtime to
// whatever the caller already does.
func (p *Provider) PropagateSecurityToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox,
	tokenResp *identity.TokenResponse, rtOpts ...agentsruntime.Option) error {
	return p.fallback.PropagateSecurityToken(ctx, sbx, tokenResp, rtOpts...)
}
