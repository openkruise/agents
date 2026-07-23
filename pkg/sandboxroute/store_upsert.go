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

// Upsert applies a route event.
func (s *Store) Upsert(route Route) MutationResult {
	route, err := AdmitRoute(route)
	if err != nil {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upsertLocked(route)
}

func (s *Store) upsertLocked(route Route) MutationResult {
	key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
	incomingResourceVersion := route.ResourceVersion
	current, hasCurrent := s.recordByObject[key]
	deletion, hasDeletion := s.deletionByObject[key]

	// Fence the incoming route against authoritative ObjectKey state.
	// An existing record is the primary ordering fence for this ObjectKey.
	if hasCurrent {
		comparison, err := resourceversion.CompareResourceVersion(incomingResourceVersion, current.route.ResourceVersion)
		// Reject malformed versions defensively even though Route validation ran earlier.
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		if comparison < 0 {
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		}
		if comparison == 0 {
			sameUID := current.route.UID == route.UID
			sameID := current.route.ID == route.ID
			// Equal versions are idempotent only for an exact same-incarnation replay.
			// Remapping ID at the same version is stale; a different UID violates the
			// trusted ObjectKey-to-UID correspondence and is ignored.
			// Same UID+ID with a changed payload falls through and installs.
			switch {
			case sameUID && sameID && route == current.route:
				return MutationResult{Result: EventResultApplied}
			case sameUID && !sameID:
				return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
			case !sameUID:
				return MutationResult{Result: EventResultIgnored, Reason: ReasonIdentityMismatch}
			}
		}
	}

	// Without a current record, reject events that do not clearly follow the recorded deletion.
	if !hasCurrent && hasDeletion {
		comparison, err := resourceversion.CompareResourceVersion(incomingResourceVersion, deletion.resourceVersion)
		// Reject malformed versions defensively even though Route validation ran earlier.
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		if comparison < 0 {
			// Older than the deletion must not resurrect the object.
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		}
		if comparison == 0 {
			// Same version as the deletion is ambiguous; confirm with a direct read.
			deletion.generation = s.nextGenerationLocked()
			deletion.confirmationQueued = true
			s.deletionByObject[key] = deletion
			return deletionFenceRepairResult(key, deletion.generation)
		}
	}

	// Install the source record and its active ID index entry.
	s.installRouteLocked(key, route)
	return MutationResult{Result: EventResultApplied}
}

func deletionFenceRepairResult(key types.NamespacedName, generation uint64) MutationResult {
	return MutationResult{
		Result:         EventResultRepairRequired,
		Reason:         ReasonAmbiguousResourceVersion,
		RepairRequests: []RepairRequest{{ObjectKey: key, Generation: generation}},
	}
}

func (s *Store) installRouteLocked(
	key types.NamespacedName,
	route Route,
) {
	generation := s.nextGenerationLocked()
	// Remove the stale ID index only when this ObjectKey moves to a different ID.
	if current, exists := s.recordByObject[key]; exists && current.route.ID != route.ID {
		s.deactivateRouteLocked(key, current.route.ID)
	}
	delete(s.deletionByObject, key)
	s.recordByObject[key] = routeRecord{
		route:      route,
		generation: generation,
	}
	s.activeKeyByID[route.ID] = key
}
