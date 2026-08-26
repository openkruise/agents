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

package sandboxcr

import (
	"context"
	"fmt"
	"time"

	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

// IssueTrafficAccessToken issues a token for the current cached Sandbox
// observation. The signed token binds the Sandbox ID and UID, which the gateway
// checks against the selected route.
func (i *Infra) IssueTrafficAccessToken(ctx context.Context, opts infra.IssueTrafficAccessTokenOptions) (infra.TrafficAccessToken, error) {
	if opts.SandboxID == "" {
		return infra.TrafficAccessToken{}, fmt.Errorf("sandbox ID is required")
	}
	if opts.Validate == nil {
		return infra.TrafficAccessToken{}, fmt.Errorf("traffic token validator is required")
	}
	lookup, err := i.lookupSandbox(ctx, infra.GetSandboxOptions{Namespace: opts.Namespace, SandboxID: opts.SandboxID})
	if err != nil {
		return infra.TrafficAccessToken{}, wrapGetSandboxError(err)
	}
	if err := opts.Validate(AsSandbox(lookup.sandbox, i.Cache)); err != nil {
		return infra.TrafficAccessToken{}, err
	}

	response, err := identity.IssueSandboxAccessToken(ctx, lookup.sandbox, opts.TokenOptions)
	if err != nil {
		return infra.TrafficAccessToken{}, err
	}

	expiresAt, err := time.Parse(time.RFC3339, response.AccessTokenExpiration)
	if err != nil {
		return infra.TrafficAccessToken{}, fmt.Errorf("parse validated traffic access token expiration: %w", err)
	}
	return infra.TrafficAccessToken{Token: response.AccessToken, Expiration: expiresAt}, nil
}
