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

import "k8s.io/apimachinery/pkg/types"

// UpsertFull applies an ObjectKey-backed route event.
func (s *Store) UpsertFull(route Route) MutationResult {
	if !hasExpectedShape(route, ShapeFull) {
		return s.recordWithoutMutation(OperationUpsert, ShapeFull, EventResultInvalid, ReasonInvalidRoute)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key, _ := route.ObjectKey()
	current, hasCurrent := s.fullByObject[key]
	if hasCurrent {
		comparison := CompareResourceVersions(current.route.ResourceVersion, route.ResourceVersion)
		switch {
		case current.route.UID == route.UID && current.route.ID == route.ID:
			if comparison != ResourceVersionEqual && comparison != ResourceVersionNewer {
				return s.finishLocked(OperationUpsert, ShapeFull, EventResultIgnored, ReasonStaleResourceVersion, nil)
			}
		case current.route.UID == route.UID:
			if comparison != ResourceVersionNewer {
				return s.finishLocked(OperationUpsert, ShapeFull, EventResultIgnored, ReasonStaleResourceVersion, nil)
			}
		default:
			switch comparison {
			case ResourceVersionNewer:
			case ResourceVersionOlder:
				return s.finishLocked(OperationUpsert, ShapeFull, EventResultIgnored, ReasonStaleResourceVersion, nil)
			default:
				current.generation = s.nextGenerationLocked()
				current.quarantined = true
				s.fullByObject[key] = current
				s.recomputeActiveViewLocked()
				request := RepairRequest{ObjectKey: key, Generation: current.generation}
				return s.finishLocked(OperationUpsert, ShapeFull, EventResultRepairRequired, ReasonAmbiguousResourceVersion, []RepairRequest{request})
			}
		}
	}

	deletion, hasDeletion := s.deletionByObject[key]
	if !hasCurrent && hasDeletion {
		comparison := CompareResourceVersions(deletion.resourceVersion, route.ResourceVersion)
		switch comparison {
		case ResourceVersionNewer:
		case ResourceVersionOlder:
			return s.finishLocked(OperationUpsert, ShapeFull, EventResultIgnored, ReasonStaleResourceVersion, nil)
		default:
			deletion.generation = s.nextGenerationLocked()
			deletion.confirmationQueued = true
			s.deletionByObject[key] = deletion
			request := RepairRequest{ObjectKey: key, Generation: deletion.generation}
			return s.finishLocked(OperationUpsert, ShapeFull, EventResultRepairRequired, ReasonAmbiguousResourceVersion, []RepairRequest{request})
		}
	}

	if compatibility, exists := s.compatByUID[route.UID]; exists &&
		!equalOrNewer(compatibility.route.ResourceVersion, route.ResourceVersion) {
		return s.finishLocked(OperationUpsert, ShapeFull, EventResultIgnored, ReasonStaleResourceVersion, nil)
	}

	targetCompatibility := s.compatibilityClaimsLocked(route.ID, route.UID)
	supersedeCompatibility := len(targetCompatibility) > 0 && allStrictlyOlder(targetCompatibility, route.ResourceVersion)
	compatibilityCollision := len(targetCompatibility) > 0 && !supersedeCompatibility
	displacedUID := types.UID("")
	if hasCurrent && current.route.UID != route.UID {
		displacedUID = current.route.UID
	}
	preserveQuarantine := hasCurrent && current.route.UID == route.UID &&
		current.route.ID == route.ID && current.quarantined
	s.installFullLocked(key, route, supersedeCompatibility, preserveQuarantine)
	displacedRequests := s.refreshQuarantinedUIDClaimsLocked(displacedUID)
	uidCollisionRequests := s.quarantineUIDClaimsLocked(route.UID, true)
	s.recomputeActiveViewLocked()
	if len(uidCollisionRequests) > 0 {
		return s.finishLocked(
			OperationUpsert,
			ShapeFull,
			EventResultCollision,
			ReasonUIDCollision,
			deduplicateRepairRequests(append(displacedRequests, uidCollisionRequests...)),
		)
	}
	if compatibilityCollision || s.idCollidedLocked(route.ID) {
		requests := append(displacedRequests, s.repairRequestsForIDLocked(route.ID)...)
		return s.finishLocked(
			OperationUpsert,
			ShapeFull,
			EventResultCollision,
			ReasonIDCollision,
			deduplicateRepairRequests(requests),
		)
	}
	return s.finishLocked(OperationUpsert, ShapeFull, EventResultApplied, ReasonNone, displacedRequests)
}

// UpsertIDOnly applies a compatibility route event without an ObjectKey.
func (s *Store) UpsertIDOnly(route Route) MutationResult {
	if !hasExpectedShape(route, ShapeIDOnly) {
		return s.recordWithoutMutation(OperationUpsert, ShapeIDOnly, EventResultInvalid, ReasonInvalidRoute)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.idHasDeletionFenceLocked(route.ID) {
		return s.finishLocked(OperationUpsert, ShapeIDOnly, EventResultIgnored, ReasonDeletionFence, nil)
	}
	if _, retired := s.retiredByUID[route.UID]; retired {
		return s.finishLocked(OperationUpsert, ShapeIDOnly, EventResultIgnored, ReasonRetiredUID, nil)
	}
	if s.uidHasFullOwnerLocked(route.UID) || s.idHasFullOwnerLocked(route.ID) {
		return s.finishLocked(OperationUpsert, ShapeIDOnly, EventResultIgnored, ReasonDominatedByFull, nil)
	}
	if current, exists := s.compatByUID[route.UID]; exists {
		if current.route.ID != route.ID {
			return s.finishLocked(OperationUpsert, ShapeIDOnly, EventResultCollision, ReasonUIDCollision, nil)
		}
		if !equalOrNewer(current.route.ResourceVersion, route.ResourceVersion) {
			return s.finishLocked(OperationUpsert, ShapeIDOnly, EventResultIgnored, ReasonStaleResourceVersion, nil)
		}
	}

	s.compatByUID[route.UID] = routeRecord{
		route:        route,
		generation:   s.nextGenerationLocked(),
		lastObserved: s.now(),
	}
	s.recomputeActiveViewLocked()
	if s.idCollidedLocked(route.ID) {
		return s.finishLocked(OperationUpsert, ShapeIDOnly, EventResultCollision, ReasonIDCollision, nil)
	}
	return s.finishLocked(OperationUpsert, ShapeIDOnly, EventResultApplied, ReasonNone, nil)
}

func (s *Store) installFullLocked(
	key types.NamespacedName,
	route Route,
	supersedeTargetCompatibility bool,
	preserveQuarantine bool,
) {
	now := s.now()
	generation := s.nextGenerationLocked()
	if current, exists := s.fullByObject[key]; exists && current.route.UID != route.UID {
		s.removeFullUIDOwnerLocked(current.route.UID, key)
		if !s.uidHasFullOwnerLocked(current.route.UID) {
			s.retiredByUID[current.route.UID] = retiredFence{
				uid:             current.route.UID,
				id:              current.route.ID,
				resourceVersion: current.route.ResourceVersion,
				generation:      generation,
				createdAt:       now,
			}
		}
	}
	delete(s.compatByUID, route.UID)
	delete(s.retiredByUID, route.UID)
	delete(s.deletionByObject, key)

	if supersedeTargetCompatibility {
		for uid, record := range s.compatByUID {
			if record.route.ID != route.ID {
				continue
			}
			delete(s.compatByUID, uid)
			s.retiredByUID[uid] = retiredFence{
				uid:             uid,
				id:              record.route.ID,
				resourceVersion: record.route.ResourceVersion,
				generation:      generation,
				createdAt:       now,
			}
		}
	}
	s.fullByObject[key] = routeRecord{
		route:       route,
		generation:  generation,
		quarantined: preserveQuarantine,
	}
	s.addFullUIDOwnerLocked(route.UID, key)
}

func allStrictlyOlder(records []routeRecord, incomingResourceVersion string) bool {
	if len(records) == 0 {
		return false
	}
	for _, record := range records {
		if CompareResourceVersions(record.route.ResourceVersion, incomingResourceVersion) != ResourceVersionNewer {
			return false
		}
	}
	return true
}
