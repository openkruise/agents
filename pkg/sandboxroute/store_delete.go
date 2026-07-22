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

package sandboxroute

import (
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/resourceversion"
)

// DeleteAuthoritativeByObjectKey removes the current local ObjectKey incarnation.
func (s *Store) DeleteAuthoritativeByObjectKey(key types.NamespacedName) MutationResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishLocked(s.deleteAuthoritativeByObjectKeyLocked(key))
}

func (s *Store) deleteAuthoritativeByObjectKeyLocked(key types.NamespacedName) MutationResult {
	if key.Namespace == "" || key.Name == "" {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidObjectKey}
	}

	if current, exists := s.recordByObject[key]; exists {
		return s.deleteRecordLocked(key, current, current.route.ResourceVersion)
	}
	return MutationResult{Result: EventResultIgnored, Reason: ReasonAbsent}
}

// DeleteConditionally removes a route when identity and resource-version fences match.
func (s *Store) DeleteConditionally(route Route) MutationResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishLocked(s.deleteConditionallyLocked(route))
}

func (s *Store) deleteConditionallyLocked(route Route) MutationResult {
	if err := route.Validate(); err != nil {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}
	return s.deleteRouteConditionallyLocked(route)
}

func (s *Store) deleteRouteConditionallyLocked(route Route) MutationResult {
	key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
	if current, exists := s.recordByObject[key]; exists {
		if current.route.ID != route.ID || current.route.UID != route.UID {
			return MutationResult{Result: EventResultIgnored, Reason: ReasonIdentityMismatch}
		}
		comparison, err := resourceversion.CompareResourceVersion(
			route.ResourceVersion,
			current.route.ResourceVersion,
		)
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		if comparison < 0 {
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		}
		return s.deleteRecordLocked(key, current, route.ResourceVersion)
	}

	if s.idHasOwnerLocked(route.ID) {
		return MutationResult{Result: EventResultIgnored, Reason: ReasonIdentityMismatch}
	}
	return MutationResult{Result: EventResultIgnored, Reason: ReasonAbsent}
}

func (s *Store) deleteRecordLocked(
	key types.NamespacedName,
	current routeRecord,
	fenceResourceVersion string,
) MutationResult {
	generation := s.nextGenerationLocked()
	if err := s.installDeletionFenceLocked(key, fenceResourceVersion, generation, false); err != nil {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}
	delete(s.recordByObject, key)
	s.recomputeActiveViewLocked()
	return MutationResult{Result: EventResultApplied}
}

func (s *Store) installDeletionFenceLocked(
	key types.NamespacedName,
	fenceResourceVersion string,
	generation uint64,
	confirmed bool,
) error {
	now := s.now()
	deletion := deletionFence{
		resourceVersion:    fenceResourceVersion,
		generation:         generation,
		createdAt:          now,
		confirmationQueued: confirmed,
		confirmed:          confirmed,
	}
	if existing, exists := s.deletionByObject[key]; exists {
		comparison, err := resourceversion.CompareResourceVersion(
			fenceResourceVersion,
			existing.resourceVersion,
		)
		if err != nil {
			return err
		}
		if comparison <= 0 {
			deletion.resourceVersion = existing.resourceVersion
			deletion.createdAt = existing.createdAt
			deletion.confirmed = existing.confirmed || confirmed
			deletion.confirmationQueued = existing.confirmationQueued || confirmed
		}
	}
	s.deletionByObject[key] = deletion
	return nil
}
