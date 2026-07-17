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

	"github.com/openkruise/agents/pkg/metrics"
)

// DeleteAuthoritativeByObjectKey removes the current local ObjectKey incarnation.
// When no full record exists, legacyFallbackID may identify compatibility records only.
func (s *Store) DeleteAuthoritativeByObjectKey(
	key types.NamespacedName,
	legacyFallbackID string,
) MutationResult {
	if key.Namespace == "" || key.Name == "" {
		return s.recordWithoutMutation(EventResultInvalid, ReasonInvalidObjectKey)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if current, exists := s.fullByObject[key]; exists {
		return s.deleteFullLocked(key, current, current.route.ResourceVersion)
	}
	if legacyFallbackID == "" {
		return s.finishLocked(EventResultIgnored, ReasonAbsent, nil)
	}

	participants := s.compatibilityClaimsLocked(legacyFallbackID, "")
	switch len(participants) {
	case 0:
		return s.finishLocked(EventResultIgnored, ReasonAbsent, nil)
	case 1:
		metrics.RecordSandboxRouteLegacyFallback(string(s.surface))
		return s.deleteCompatibilityForObjectLocked(
			key,
			participants[0],
			participants[0].route.ResourceVersion,
		)
	default:
		return s.finishLocked(EventResultCollision, ReasonIDCollision, nil)
	}
}

// DeleteFullConditionally removes a full identity only when ObjectKey, ID, UID,
// and resource-version fences match. It may remove an exact compatibility
// participant when no full ObjectKey record exists.
func (s *Store) DeleteFullConditionally(route Route) MutationResult {
	if !hasExpectedShape(route, ShapeFull) {
		return s.recordWithoutMutation(EventResultInvalid, ReasonInvalidRoute)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key, _ := route.ObjectKey()
	if current, exists := s.fullByObject[key]; exists {
		if current.route.ID != route.ID || current.route.UID != route.UID {
			return s.finishLocked(EventResultIgnored, ReasonIdentityMismatch, nil)
		}
		if !equalOrNewer(current.route.ResourceVersion, route.ResourceVersion) {
			return s.finishLocked(EventResultIgnored, ReasonStaleResourceVersion, nil)
		}
		return s.deleteFullLocked(key, current, route.ResourceVersion)
	}

	if compatibility, exists := s.compatByUID[route.UID]; exists {
		if compatibility.route.ID != route.ID {
			return s.finishLocked(EventResultIgnored, ReasonIdentityMismatch, nil)
		}
		if !equalOrNewer(compatibility.route.ResourceVersion, route.ResourceVersion) {
			return s.finishLocked(EventResultIgnored, ReasonStaleResourceVersion, nil)
		}
		return s.deleteCompatibilityForObjectLocked(key, compatibility, route.ResourceVersion)
	}
	if s.uidHasFullOwnerLocked(route.UID) ||
		s.idHasFullOwnerLocked(route.ID) || s.idHasCompatibilityOwnerLocked(route.ID) {
		return s.finishLocked(EventResultIgnored, ReasonIdentityMismatch, nil)
	}
	return s.finishLocked(EventResultIgnored, ReasonAbsent, nil)
}

// DeleteIDOnlyConditionally removes only an exact compatibility identity when
// its delete resource version is equal or newer and comparable.
func (s *Store) DeleteIDOnlyConditionally(route Route) MutationResult {
	if !hasExpectedShape(route, ShapeIDOnly) {
		return s.recordWithoutMutation(EventResultInvalid, ReasonInvalidRoute)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	current, exists := s.compatByUID[route.UID]
	if !exists {
		if s.uidHasFullOwnerLocked(route.UID) || s.idHasFullOwnerLocked(route.ID) {
			return s.finishLocked(EventResultIgnored, ReasonDominatedByFull, nil)
		}
		if s.idHasCompatibilityOwnerLocked(route.ID) {
			return s.finishLocked(EventResultIgnored, ReasonIdentityMismatch, nil)
		}
		return s.finishLocked(EventResultIgnored, ReasonAbsent, nil)
	}
	if current.route.ID != route.ID {
		return s.finishLocked(EventResultIgnored, ReasonIdentityMismatch, nil)
	}
	if !equalOrNewer(current.route.ResourceVersion, route.ResourceVersion) {
		return s.finishLocked(EventResultIgnored, ReasonStaleResourceVersion, nil)
	}

	delete(s.compatByUID, route.UID)
	// Advance the generation to mark this active-view change, mirroring the
	// changedView bump in Maintenance. The value is intentionally unused here
	// since retiredFence no longer carries a generation.
	s.nextGenerationLocked()
	s.retiredByUID[route.UID] = retiredFence{
		createdAt: s.now(),
	}
	s.recomputeActiveViewLocked()
	return s.finishLocked(EventResultApplied, ReasonNone, nil)
}

func (s *Store) deleteFullLocked(
	key types.NamespacedName,
	current routeRecord,
	fenceResourceVersion string,
) MutationResult {
	delete(s.fullByObject, key)
	s.removeFullUIDOwnerLocked(current.route.UID, key)
	generation := s.nextGenerationLocked()
	s.installDeletionFencesLocked(key, current.route, fenceResourceVersion, generation, false)
	requests := s.refreshQuarantinedUIDClaimsLocked(current.route.UID)
	s.recomputeActiveViewLocked()
	return s.finishLocked(EventResultApplied, ReasonNone, requests)
}

func (s *Store) deleteCompatibilityForObjectLocked(
	key types.NamespacedName,
	current routeRecord,
	fenceResourceVersion string,
) MutationResult {
	delete(s.compatByUID, current.route.UID)
	generation := s.nextGenerationLocked()
	s.installDeletionFencesLocked(key, current.route, fenceResourceVersion, generation, false)
	s.recomputeActiveViewLocked()
	return s.finishLocked(EventResultApplied, ReasonNone, nil)
}

func (s *Store) installDeletionFencesLocked(
	key types.NamespacedName,
	deleted Route,
	fenceResourceVersion string,
	generation uint64,
	confirmed bool,
) {
	now := s.now()
	deletion := deletionFence{
		uid:                deleted.UID,
		id:                 deleted.ID,
		resourceVersion:    fenceResourceVersion,
		generation:         generation,
		createdAt:          now,
		confirmationQueued: confirmed,
		confirmed:          confirmed,
	}
	if existing, exists := s.deletionByObject[key]; exists &&
		CompareResourceVersions(existing.resourceVersion, fenceResourceVersion) != ResourceVersionNewer {
		deletion.uid = existing.uid
		deletion.id = existing.id
		deletion.resourceVersion = existing.resourceVersion
		deletion.createdAt = existing.createdAt
		deletion.confirmed = existing.confirmed || confirmed
		deletion.confirmationQueued = existing.confirmationQueued || confirmed
	}
	s.deletionByObject[key] = deletion
	s.retiredByUID[deleted.UID] = retiredFence{
		createdAt: now,
	}
}
