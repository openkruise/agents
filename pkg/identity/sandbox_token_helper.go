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

// IssueSandboxToken issues a security token for the given sandbox using the
// registered identity provider.
//
// It collects all sandbox labels prefixed with utils.SecurityMetadataPrefix as
// request metadata, builds a TokenRequest of type TokenTypeAgent, and delegates
// to the package-level IssueToken entry. The returned cost reflects the total
// duration spent issuing the token (including metadata collection), which
// callers can record on their own metrics structures.
//
// The function is intentionally side-effect free: it does NOT mutate the
// sandbox object or persist the response. Callers are responsible for
// persisting the returned TokenResponse into the appropriate place
// (e.g. ClaimSandboxOptions, sandbox annotations, or runtime credentials).
func IssueSandboxToken(ctx context.Context, sbx *agentsv1alpha1.Sandbox) (*TokenResponse, time.Duration, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx), "action", "IssueSandboxToken")
	start := time.Now()

	metadata := make(map[string]string)
	for k, v := range sbx.GetLabels() {
		if strings.HasPrefix(k, SecurityMetadataPrefix) {
			metadata[k] = v
		}
	}

	tokenResp, err := IssueToken(ctx, TokenRequest{
		TokenType: TokenTypeAgent,
		Sandbox: &SandboxInfo{
			PodName:      sbx.Name,
			PodNamespace: sbx.Namespace,
			SandboxID:    fmt.Sprintf("%s/%s/%s", sbx.Namespace, sbx.Name, sbx.UID),
			SandboxName:  sbx.Name,
			SandboxUID:   string(sbx.UID),
		},
		Metadata: metadata,
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
