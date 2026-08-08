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

// Package identityprovider implements the signed Traffic JWT provider used by
// the sandbox-gateway E2E profile.
package identityprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/utils"
	utilruntime "github.com/openkruise/agents/pkg/utils/runtime"
)

type provider struct {
	endpoint string
	client   *http.Client
}

type issueRequest struct {
	SandboxID      string `json:"sandboxId"`
	SandboxUID     string `json:"sandboxUid"`
	ValiditySecond int64  `json:"validitySeconds"`
}

// New returns an IdentityProvider backed by the JWT E2E issuer.
func New(endpoint string) identity.IdentityProvider {
	return &provider{
		endpoint: strings.TrimRight(endpoint, "/"),
		client:   &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *provider) IssueToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, opts identity.TokenOptions) (*identity.TokenResponse, error) {
	if opts.Kind != identity.TokenKindAccessToken {
		return nil, fmt.Errorf("JWT E2E provider does not support token kind %q", opts.Kind)
	}
	if sbx == nil {
		return nil, fmt.Errorf("JWT E2E provider requires a Sandbox")
	}
	body, err := json.Marshal(issueRequest{
		SandboxID:      utils.GetSandboxID(sbx),
		SandboxUID:     string(sbx.UID),
		ValiditySecond: int64(opts.RequestedValidity / time.Second),
	})
	if err != nil {
		return nil, fmt.Errorf("encode JWT E2E issue request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.endpoint+"/issue", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create JWT E2E issue request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call JWT E2E issuer: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("JWT E2E issuer returned status %d", resp.StatusCode)
	}
	result := &identity.TokenResponse{}
	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return nil, fmt.Errorf("decode JWT E2E issue response: %w", err)
	}
	return result, nil
}

func (p *provider) PropagateSecurityToken(_ context.Context, _ *agentsv1alpha1.Sandbox, _ *identity.TokenResponse, _ ...utilruntime.Option) error {
	return nil
}
