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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// IsIdentityProviderRequested reports whether the sandbox opts into the
// identity provider issuance path.
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
func IsIdentityProviderRequested(sbx *agentsv1alpha1.Sandbox) bool {
	if sbx == nil {
		return false
	}
	return sbx.GetAnnotations()[AnnotationAgentName] != ""
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
// It builds a TokenRequest of type TokenTypeAgent, forwarding the sandbox and
// claim objects verbatim to the provider. Metadata extraction is intentionally
// left to the provider: implementations that need security metadata can call
// ExtractSecurityMetadata(sbx), and those that need storage-auth or other
// annotations can read them directly from sbx.GetAnnotations().
//
// The claim parameter may be nil for non-CRD issuance paths (e.g. token refresh
// or the E2B API). IdentityProvider implementations must handle a nil claim
// gracefully.
//
// The function is intentionally side-effect free: it does NOT mutate the
// sandbox object or persist the response. Callers are responsible for
// persisting the returned TokenResponse into the appropriate place
// (e.g. ClaimSandboxOptions, sandbox annotations, or runtime credentials).
func IssueSandboxToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox, claim *agentsv1alpha1.SandboxClaim) (*TokenResponse, time.Duration, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "action", "IssueSandboxToken")
	start := time.Now()

	tokenResp, err := IssueToken(ctx, sbx, claim, TokenRequest{
		TokenType: TokenTypeAgent,
		Sandbox: &SandboxInfo{
			PodName:      sbx.Name,
			PodNamespace: sbx.Namespace,
			SandboxID:    fmt.Sprintf("%s/%s/%s", sbx.Namespace, sbx.Name, sbx.UID),
			SandboxName:  sbx.Name,
			SandboxUID:   string(sbx.UID),
		},
	})
	cost := time.Since(start)
	if err != nil {
		log.Error(err, "failed to issue sandbox security token", "cost", cost)
		return nil, cost, fmt.Errorf("failed to issue security token: %w", err)
	}
	log.Info("sandbox security token issued", "cost", cost)
	return tokenResp, cost, nil
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
