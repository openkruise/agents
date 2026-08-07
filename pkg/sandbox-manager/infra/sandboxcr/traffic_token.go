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

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
)

// IssueTrafficAccessToken issues a token between two authoritative Sandbox
// reads. A token minted across a lifecycle or ownership transition is discarded.
func (i *Infra) IssueTrafficAccessToken(ctx context.Context, opts infra.IssueTrafficAccessTokenOptions) (infra.TrafficAccessToken, error) {
	if opts.SandboxID == "" {
		return infra.TrafficAccessToken{}, fmt.Errorf("sandbox ID is required")
	}
	if opts.Validate == nil {
		return infra.TrafficAccessToken{}, fmt.Errorf("traffic token validator is required")
	}
	if i.APIReader == nil {
		return infra.TrafficAccessToken{}, fmt.Errorf("API reader is not configured")
	}

	lookup, err := i.lookupSandbox(ctx, infra.GetSandboxOptions{Namespace: opts.Namespace, SandboxID: opts.SandboxID})
	if err != nil {
		return infra.TrafficAccessToken{}, err
	}
	key := client.ObjectKeyFromObject(lookup.sandbox)
	before, err := i.getTrafficTokenSandbox(ctx, key, opts.SandboxID)
	if err != nil {
		return infra.TrafficAccessToken{}, err
	}
	beforeSandbox := AsSandbox(before, i.Cache)
	if err := opts.Validate(beforeSandbox); err != nil {
		return infra.TrafficAccessToken{}, err
	}

	response, err := identity.IssueSandboxAccessToken(ctx, before, opts.TokenOptions)
	if err != nil {
		return infra.TrafficAccessToken{}, err
	}

	after, err := i.getTrafficTokenSandbox(ctx, key, opts.SandboxID)
	if err != nil {
		return infra.TrafficAccessToken{}, err
	}
	if before.UID != after.UID {
		return infra.TrafficAccessToken{}, fmt.Errorf("sandbox changed while issuing traffic access token")
	}
	if before.Annotations[v1alpha1.AnnotationOwner] != after.Annotations[v1alpha1.AnnotationOwner] {
		return infra.TrafficAccessToken{}, fmt.Errorf("sandbox owner changed while issuing traffic access token")
	}
	if before.Status.Phase != after.Status.Phase {
		return infra.TrafficAccessToken{}, fmt.Errorf("sandbox state changed while issuing traffic access token")
	}
	if err := opts.Validate(AsSandbox(after, i.Cache)); err != nil {
		return infra.TrafficAccessToken{}, err
	}

	expiresAt, err := time.Parse(time.RFC3339, response.AccessTokenExpiration)
	if err != nil {
		return infra.TrafficAccessToken{}, fmt.Errorf("parse validated traffic access token expiration: %w", err)
	}
	return infra.TrafficAccessToken{Token: response.AccessToken, Expiration: expiresAt}, nil
}

func (i *Infra) getTrafficTokenSandbox(ctx context.Context, key client.ObjectKey, sandboxID string) (*v1alpha1.Sandbox, error) {
	sandbox, err := i.getSandboxFromAPIReader(ctx, key, sandboxID)
	if err != nil {
		return nil, err
	}
	if utils.GetSandboxID(sandbox) != sandboxID {
		return nil, fmt.Errorf("sandbox identity changed while issuing traffic access token")
	}
	if !sandbox.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("sandbox is being deleted")
	}
	if !identity.IsAccessTokenRequested(sandbox) {
		return nil, fmt.Errorf("sandbox does not enable traffic JWT authentication")
	}
	return sandbox, nil
}
