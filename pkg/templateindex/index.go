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

// Package templateindex contains read-only ownership and index primitives for
// template control views. It has no controller gates, API or Manager dependency.
package templateindex

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/openkruise/agents/api/v1alpha1"
)

const (
	ClaimOwner  = "templateClaimOwnerUID"
	TemplateRef = "sandboxSetTemplateRef"
)

// ClaimOwnerReference identifies a version carrying an actual Claim controller
// reference. Callers still confirm the current Claim UID in their own backend.
// The returned reference is shared read-only metadata and must not be mutated.
func ClaimOwnerReference(template *api.SandboxTemplate) *metav1.OwnerReference {
	if template == nil {
		return nil
	}
	owner := metav1.GetControllerOfNoCopy(template)
	if owner == nil || owner.APIVersion != api.GroupVersion.String() || owner.Kind != "SandboxTemplateClaim" || owner.Name == "" || owner.UID == "" {
		return nil
	}
	return owner
}

// VersionOwnerReference identifies an object whose controller reference names a
// SandboxTemplate version. Callers still confirm namespace, name and UID against
// the actual version in their own backend. The returned reference is shared
// read-only metadata and must not be mutated.
func VersionOwnerReference(obj metav1.Object) *metav1.OwnerReference {
	if obj == nil {
		return nil
	}
	owner := metav1.GetControllerOfNoCopy(obj)
	if owner == nil || owner.APIVersion != api.GroupVersion.String() || owner.Kind != "SandboxTemplate" || owner.Name == "" || owner.UID == "" {
		return nil
	}
	return owner
}

// ClaimOwnerUID indexes only concrete versions owned by Claims in this API.
func ClaimOwnerUID(obj client.Object) []string {
	template, ok := obj.(*api.SandboxTemplate)
	if !ok {
		return nil
	}
	owner := ClaimOwnerReference(template)
	if owner == nil {
		return nil
	}
	return []string{string(owner.UID)}
}

// TemplateRefName extracts the declared reference; GVK/default/owner checks
// belong to the consumer, so even unlabeled external references are indexed.
func TemplateRefName(obj client.Object) []string {
	pool, ok := obj.(*api.SandboxSet)
	if !ok || pool == nil || pool.Spec.TemplateRef == nil || pool.Spec.TemplateRef.Name == "" {
		return nil
	}
	return []string{pool.Spec.TemplateRef.Name}
}

// Register installs indexes on this specific control view. There is deliberately
// no process-global once: every independent cache must register and report errors.
func Register(ctx context.Context, indexer client.FieldIndexer) error {
	if indexer == nil {
		return fmt.Errorf("template field indexer is required")
	}
	entries := []struct {
		object  client.Object
		name    string
		extract client.IndexerFunc
	}{
		{&api.SandboxTemplate{}, ClaimOwner, ClaimOwnerUID},
		{&api.SandboxSet{}, TemplateRef, TemplateRefName},
	}
	for _, entry := range entries {
		if err := indexer.IndexField(ctx, entry.object, entry.name, entry.extract); err != nil {
			return fmt.Errorf("register template index %q on %T: %w", entry.name, entry.object, err)
		}
	}
	return nil
}
