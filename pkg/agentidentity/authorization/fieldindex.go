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

package authorization

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	securityv1alpha1 "github.com/openkruise/agents/api/security/v1alpha1"
)

// IndexFieldAgentIdentityRef names the field index Authorize's own List call
// depends on, over AgentRoleBinding's spec.agentIdentityRef.name.
//
// Without it, Authorize would have to list every AgentRoleBinding in the
// namespace and filter in Go on every call, which scales with how many
// bindings the namespace holds rather than with how many name this one
// identity. The index turns that into a direct, informer-cache-backed lookup
// scoped to the identity being asked about, with no caching layer of its own
// needed on top: an indexed read is already as current as the informer cache
// itself, the same freshness the unindexed List already had.
const IndexFieldAgentIdentityRef = "spec.agentIdentityRef.name"

// agentIdentityRefIndexFunc extracts the index key from an AgentRoleBinding.
func agentIdentityRefIndexFunc(obj client.Object) []string {
	binding, ok := obj.(*securityv1alpha1.AgentRoleBinding)
	if !ok || binding.Spec.AgentIdentityRef.Name == "" {
		return nil
	}
	return []string{binding.Spec.AgentIdentityRef.Name}
}

// SetupFieldIndex registers IndexFieldAgentIdentityRef on c.
//
// c must be the same cache the Authorizer reading through it will list
// AgentRoleBindings from; registering it on an unrelated cache indexes
// nothing Authorize will ever query. Call this once, while the manager
// owning c is starting, before serving requests: a field index has to exist
// before anything lists by it, and List returns an error naming the missing
// index rather than silently falling back to an unindexed scan, so a caller
// that forgets this fails loudly instead of just paying for the scan again.
func SetupFieldIndex(ctx context.Context, c cache.Cache) error {
	if err := c.IndexField(ctx, &securityv1alpha1.AgentRoleBinding{},
		IndexFieldAgentIdentityRef, agentIdentityRefIndexFunc); err != nil {
		return fmt.Errorf("index AgentRoleBinding by %s: %w", IndexFieldAgentIdentityRef, err)
	}
	return nil
}
