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

// Package core implements the side-effecting steps of the security-token
// refresh controller (issue, propagate, patch). It is split out from the
// reconciler so reconciliation logic stays focused on policy decisions
// (timing, requeue) and the core can be replaced with a stub in unit tests.
package core

import (
	"context"
	"fmt"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

// Refresher performs the actual security-token refresh on a single sandbox.
//
// Implementations must guarantee the following ordering invariants:
//  1. Issue a new token via identity.IssueSandboxToken.
//  2. Propagate the new token to the runtime via identity.PropagateSandboxToken.
//  3. Only after a successful propagation, patch the sandbox annotation
//     identity.AgentKeyTokenRefreshStatus with the new expiration.
//
// If any earlier step fails, the annotation MUST NOT be updated, so the next
// reconcile keeps trying with the same expiration window.
type Refresher interface {
	// Refresh issues, propagates and persists a new security token for the given sandbox.
	// On success it returns the new TokenResponse so the caller can decide the next
	// requeue interval based on the freshly issued AccessTokenExpiration.
	Refresh(ctx context.Context, sbx *agentsv1alpha1.Sandbox) (*identity.TokenResponse, error)
}

// defaultRefresher implements Refresher against a real controller-runtime client.
// It is the production implementation used by the security-token-refresh controller.
type defaultRefresher struct {
	client client.Client
}

// NewDefaultRefresher constructs the production Refresher backed by the given
// controller-runtime client. The client is used to patch the sandbox annotation
// once the new token has been propagated successfully.
func NewDefaultRefresher(c client.Client) Refresher {
	return &defaultRefresher{client: c}
}

// Refresh executes the issue → propagate → patch sequence described on Refresher.
// Errors are returned to the caller so controller-runtime can apply its rate-limited
// retry policy on the workqueue; partial side-effects (e.g. propagation succeeded
// but patch failed) are surfaced explicitly so the next reconcile can converge.
func (r *defaultRefresher) Refresh(ctx context.Context, sbx *agentsv1alpha1.Sandbox) (*identity.TokenResponse, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))

	tokenResp, _, err := identity.IssueSandboxToken(ctx, sbx)
	if err != nil {
		return nil, fmt.Errorf("issue token: %w", err)
	}

	if err := identity.PropagateSandboxToken(ctx, sbx, tokenResp); err != nil {
		return nil, fmt.Errorf("propagate token: %w", err)
	}

	if err := r.patchAnnotation(ctx, sbx, tokenResp); err != nil {
		return nil, fmt.Errorf("patch token-status annotation: %w", err)
	}

	log.Info("security token refreshed", "expiration", tokenResp.AccessTokenExpiration)
	return tokenResp, nil
}

// patchAnnotation persists the freshly issued token's expiration into the sandbox
// annotation identity.AgentKeyTokenRefreshStatus using a MergeFrom patch so concurrent
// updates on unrelated fields are not stomped.
func (r *defaultRefresher) patchAnnotation(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *identity.TokenResponse) error {
	raw, err := identity.EncodeTokenRefreshStatus(identity.BuildTokenRefreshStatus(tokenResp))
	if err != nil {
		return err
	}
	// MergeFrom only reads the base to compute the diff, so the base does not
	// need its own DeepCopy; cloning `updated` alone is enough to keep the
	// caller's object untouched.
	updated := sbx.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = make(map[string]string, 1)
	}
	updated.Annotations[identity.AgentKeyTokenRefreshStatus] = raw
	return r.client.Patch(ctx, updated, client.MergeFrom(sbx))
}
