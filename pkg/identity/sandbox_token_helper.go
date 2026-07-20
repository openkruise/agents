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
	"fmt"
	"strings"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// IsIDTokenRequested reports whether the sandbox opts into the ID token
// (identity provider) issuance path.
//
// The opt-in signal is the presence of a non-empty
// "security.agents.kruise.io/agent-name" annotation on the sandbox: setting
// this annotation expresses the user's intent to bind the sandbox to a logical
// agent identity, which is the precondition for the identity provider to mint a
// security token. A nil sandbox, a sandbox without Annotations, or one whose
// value for that key is empty all collapse to "not requested", letting callers
// short-circuit the issuance path without paying any provider cost.
//
// The check is intentionally annotation-only and value-presence-only: it does
// NOT validate the value against any naming convention, since the identity
// provider is the authoritative source of truth for agent-name semantics.
func IsIDTokenRequested(sbx *agentsv1alpha1.Sandbox) bool {
	if sbx == nil {
		return false
	}
	return sbx.GetAnnotations()[AnnotationAgentName] != ""
}

// IsAccessTokenRequested reports whether the sandbox opts into access-token
// issuance for reaching the sandbox through the gateway.
//
// The opt-in signal is a "security.agents.kruise.io/enable-jwt-auth" annotation
// whose value equals exactly "true". Unlike IsIDTokenRequested (which
// treats any non-empty value as opt-in because the value carries a meaningful
// agent name), this predicate is a strict boolean toggle: a nil sandbox, a
// missing annotation, or any value other than "true" all collapse to "not
// requested", letting callers short-circuit the issuance path without paying
// any issuer cost.
func IsAccessTokenRequested(sbx *agentsv1alpha1.Sandbox) bool {
	if sbx == nil {
		return false
	}
	return sbx.GetAnnotations()[AnnotationEnableJwtAuth] == agentsv1alpha1.True
}

// ExtractSecurityMetadata returns a map containing only the sandbox annotations
// whose keys are prefixed with SecurityMetadataPrefix. Providers that want to
// include security metadata in token issuance requests should call this helper
// instead of re-implementing the prefix filter.
//
// A nil sandbox results in a nil map. The returned map is never nil when the
// sandbox is non-nil, even if no matching annotations exist, so providers can
// safely iterate over it.
func ExtractSecurityMetadata(sbx *agentsv1alpha1.Sandbox) map[string]string {
	if sbx == nil {
		return nil
	}
	return ExtractSecurityMetadataFromMap(sbx.GetAnnotations())
}

// ExtractSecurityMetadataFromMap returns a new map containing only the entries
// of in whose keys are prefixed with SecurityMetadataPrefix. It is the single
// source of truth for the security-prefix filter, shared by the
// Sandbox-annotations path (ExtractSecurityMetadata) and caller-supplied inputs
// such as the E2B API request. The returned map is never nil, so callers can
// safely iterate over it even when no entry matches.
func ExtractSecurityMetadataFromMap(in map[string]string) map[string]string {
	metadata := make(map[string]string)
	for k, v := range in {
		if strings.HasPrefix(k, SecurityMetadataPrefix) {
			metadata[k] = v
		}
	}
	return metadata
}

// IssueSandboxToken issues a security token for the given sandbox using the
// registered identity provider.
//
// It forwards the sandbox object verbatim to the provider. The provider owns
// the composition of the concrete wire request (SandboxInfo projection,
// security metadata, token type): it derives everything it needs directly from
// the sandbox object. The community baseline therefore carries no
// request-shaping policy here, and enterprise providers assemble exactly the
// atomic request their backend expects.
//
// The function is intentionally side-effect free: it does NOT mutate the
// sandbox object or persist the response. Callers are responsible for
// persisting the returned TokenResponse into the appropriate place
// (e.g. ClaimSandboxOptions, sandbox annotations, or runtime credentials).
func IssueSandboxToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox) (*TokenResponse, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "action", "IssueSandboxToken")
	start := time.Now()

	tokenResp, err := IssueToken(ctx, sbx, TokenKindIDToken)
	cost := time.Since(start)
	if err != nil {
		log.Error(err, "failed to issue sandbox security token", "cost", cost)
		return nil, fmt.Errorf("failed to issue security token: %w", err)
	}
	log.Info("sandbox security token issued", "cost", cost)
	return tokenResp, nil
}

// IssueSandboxAccessToken mints the access token used to reach the given
// sandbox through the sandbox gateway, using the registered IdentityProvider.
//
// It reuses the same IdentityProvider as IssueSandboxToken, selecting the
// access-token kind via TokenKindAccessToken. The provider owns the composition
// of the concrete wire request (subject, audience, validity, SandboxInfo
// projection) exactly like it does for security tokens; the minted token is
// returned in TokenResponse.AccessToken.
//
// The function is intentionally side-effect free: it does NOT mutate the
// sandbox object or persist the response. Callers decide where to carry the
// returned token (e.g. a transient in-memory field on the claimed sandbox),
// deliberately keeping the token off the sandbox annotations.
func IssueSandboxAccessToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox) (*TokenResponse, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "action", "IssueSandboxAccessToken")
	start := time.Now()

	accessResp, err := IssueToken(ctx, sbx, TokenKindAccessToken)
	cost := time.Since(start)
	if err != nil {
		log.Error(err, "failed to issue sandbox access token", "cost", cost)
		return nil, fmt.Errorf("failed to issue access token: %w", err)
	}
	log.Info("sandbox access token issued", "cost", cost)
	return accessResp, nil
}

// PropagateSandboxToken propagates the freshly issued security token to the
// runtime side via the registered SecurityTokenPropagators. It is the symmetric
// twin of IssueSandboxToken: callers obtain a TokenResponse first (issue) and
// then push it into the runtime (propagate).
//
// The function intentionally has no side-effect on the sandbox object — it
// only delegates to PropagateSecurityToken while emitting uniform structured
// logs (propagator count, cost) so every call site (claim flow, refresh
// controller, future SDK helpers) shares the same observability surface.
//
// The error returned by the underlying provider is surfaced verbatim so
// callers can decide their own retry / event semantics; this function never
// wraps or rewrites it.
func PropagateSandboxToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "action", "PropagateSandboxToken")
	start := time.Now()
	log.Info("propagating sandbox security token", "propagatorCount", SecurityTokenPropagatorCount())
	if err := PropagateSecurityToken(ctx, sbx, tokenResp); err != nil {
		log.Error(err, "failed to propagate sandbox security token", "cost", time.Since(start))
		return err
	}
	log.Info("sandbox security token propagated", "cost", time.Since(start))
	return nil
}

// ProcessSandboxToken runs the full security-token lifecycle for a sandbox: it
// issues a fresh token, propagates it to the runtime, and only then records the
// new expiration into the AgentKeyTokenRefreshStatus annotation via a MergeFrom
// patch.
//
// It is the single source of truth shared by the claim/clone flows
// (called directly by the sandboxcr reconcilers) and the post-resume
// reinitializer (sandboxcore.reinitSecurityToken), so both paths stay behaviourally
// identical and follow the same issue -> propagate -> record invariant used by
// the security-token-refresh controller. Recording only after a successful
// propagation guarantees a failed delivery never persists a misleading "fresh"
// expiration that would suppress the refresh controller's retry.
//
// Callers gate on IsIDTokenRequested before invoking this function (the
// claim, clone and post-resume paths all do so), so it performs no opt-in check
// itself and always drives the full lifecycle.
//
// Providers assemble their own issuance request from the sandbox annotations,
// so no claim is required and the same lifecycle serves the claim, clone and
// post-resume paths identically. On success the total lifecycle cost
// (issue + propagate + record) is returned so callers can record metrics; on
// failure the returned cost still reflects the elapsed time up to the failing
// phase.
func ProcessSandboxToken(ctx context.Context, c client.Client, sbx *agentsv1alpha1.Sandbox) (time.Duration, error) {
	// Measure the whole issue -> propagate -> record lifecycle so callers record
	// the total cost of the security-token step, not just token issuance.
	start := time.Now()

	// IssueSandboxToken already wraps its failure as
	// "failed to issue security token: %w", so callers can classify the phase.
	tokenResp, err := IssueSandboxToken(ctx, sbx)
	if err != nil {
		return time.Since(start), err
	}

	if err := PropagateSandboxToken(ctx, sbx, tokenResp); err != nil {
		return time.Since(start), fmt.Errorf("failed to propagate security token: %w", err)
	}

	if err := recordTokenRefreshStatus(ctx, c, sbx, tokenResp); err != nil {
		return time.Since(start), fmt.Errorf("failed to record security token refresh status: %w", err)
	}
	return time.Since(start), nil
}

// recordTokenRefreshStatus persists the freshly issued token's expiration into
// the sandbox annotation AgentKeyTokenRefreshStatus via a MergeFrom patch, so
// the security-token-refresh controller re-arms its schedule from the new
// expiry without stomping concurrent updates to unrelated fields.
//
// MergeFrom is lazy: it only reads the base object to compute the diff at Patch
// time, so cloning `updated` alone keeps the caller's object untouched until the
// patch succeeds. On success the annotation is mirrored back onto the caller's
// sandbox in place, so callers holding the same pointer observe the new value
// without an extra apiserver read; on failure the caller's object is left
// unchanged so the caller can decide how to recover.
func recordTokenRefreshStatus(ctx context.Context, c client.Client, sbx *agentsv1alpha1.Sandbox, tokenResp *TokenResponse) error {
	raw, err := EncodeTokenRefreshStatus(BuildTokenRefreshStatus(tokenResp))
	if err != nil {
		return fmt.Errorf("failed to marshal token refresh status: %w", err)
	}
	updated := sbx.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = make(map[string]string, 1)
	}
	updated.Annotations[AgentKeyTokenRefreshStatus] = raw
	if err := c.Patch(ctx, updated, client.MergeFrom(sbx)); err != nil {
		return fmt.Errorf("failed to patch token refresh status annotation: %w", err)
	}
	if sbx.Annotations == nil {
		sbx.Annotations = make(map[string]string, 1)
	}
	sbx.Annotations[AgentKeyTokenRefreshStatus] = raw
	return nil
}
