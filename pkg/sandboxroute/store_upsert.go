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
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishLocked(s.upsertLocked(route))
}

func (s *Store) upsertLocked(route Route) MutationResult {
	if err := route.Validate(); err != nil {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}
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
		sameUID := current.route.UID == route.UID
		sameID := current.route.ID == route.ID
		// Equal versions are idempotent only when UID and ID match.
		// Changing either requires a newer version.
		switch {
		// Ignore older events and equal-version attempts to remap the same UID to another ID.
		case comparison < 0, comparison == 0 && sameUID && !sameID:
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		// Equal versions with different UIDs are ambiguous, so quarantine and verify the object directly.
		case comparison == 0 && !sameUID:
			current.generation = s.nextGenerationLocked()
			current.quarantined = true
			s.recordByObject[key] = current
			s.recomputeActiveViewLocked()
			return ambiguousResourceVersionResult(key, current.generation)
		}
	}

	// Without a current record, reject events that do not clearly follow the recorded deletion.
	if !hasCurrent && hasDeletion {
		comparison, err := resourceversion.CompareResourceVersion(incomingResourceVersion, deletion.resourceVersion)
		// Reject malformed versions defensively even though Route validation ran earlier.
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		switch {
		// An event older than the deletion is stale and must not resurrect the object.
		case comparison < 0:
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		// An event at the deletion version has ambiguous ordering and requires a direct read.
		case comparison == 0:
			deletion.generation = s.nextGenerationLocked()
			deletion.confirmationQueued = true
			s.deletionByObject[key] = deletion
			return ambiguousResourceVersionResult(key, deletion.generation)
		}
	}

	preserveQuarantine := hasCurrent && current.route.UID == route.UID &&
		current.route.ID == route.ID && current.quarantined

	// Install the source record, rebuild the derived view, and classify the result.
	s.installRouteLocked(key, route, preserveQuarantine)
	s.recomputeActiveViewLocked()
	// Report any derived collision for the target ID.
	if s.idCollidedLocked(route.ID) {
		return MutationResult{
			Result:         EventResultCollision,
			Reason:         ReasonIDCollision,
			RepairRequests: s.repairRequestsForIDLocked(route.ID),
		}
	}
	return MutationResult{Result: EventResultApplied}
}

func ambiguousResourceVersionResult(key types.NamespacedName, generation uint64) MutationResult {
	return MutationResult{
		Result:         EventResultRepairRequired,
		Reason:         ReasonAmbiguousResourceVersion,
		RepairRequests: []RepairRequest{{ObjectKey: key, Generation: generation}},
	}
}

func (s *Store) installRouteLocked(
	key types.NamespacedName,
	route Route,
	preserveQuarantine bool,
) {
	generation := s.nextGenerationLocked()
	delete(s.deletionByObject, key)
	s.recordByObject[key] = routeRecord{
		route:       route,
		generation:  generation,
		quarantined: preserveQuarantine,
	}
}
