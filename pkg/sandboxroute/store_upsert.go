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

// Upsert applies a route event, dispatching by Route shape.
func (s *Store) Upsert(route Route) MutationResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishLocked(s.upsertLocked(route))
}

func (s *Store) upsertLocked(route Route) MutationResult {
	if err := route.Validate(); err != nil {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}
	shape, _ := route.Shape()
	switch shape {
	case ShapeFull:
		return s.upsertFullLocked(route)
	case ShapeIDOnly:
		return s.upsertIDOnlyLocked(route)
	default:
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}
}

func (s *Store) upsertFullLocked(route Route) MutationResult {
	key, _ := route.ObjectKey()
	current, hasCurrent := s.fullByObject[key]
	if hasCurrent {
		comparison, err := resourceversion.CompareResourceVersion(
			route.ResourceVersion,
			current.route.ResourceVersion,
		)
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		switch {
		case current.route.UID == route.UID && current.route.ID == route.ID:
			if comparison < 0 {
				return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
			}
		case current.route.UID == route.UID:
			if comparison <= 0 {
				return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
			}
		default:
			switch {
			case comparison > 0:
			case comparison < 0:
				return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
			default:
				current.generation = s.nextGenerationLocked()
				current.quarantined = true
				s.fullByObject[key] = current
				s.recomputeActiveViewLocked()
				request := RepairRequest{ObjectKey: key, Generation: current.generation}
				return MutationResult{
					Result:         EventResultRepairRequired,
					Reason:         ReasonAmbiguousResourceVersion,
					RepairRequests: []RepairRequest{request},
				}
			}
		}
	}

	deletion, hasDeletion := s.deletionByObject[key]
	if !hasCurrent && hasDeletion {
		comparison, err := resourceversion.CompareResourceVersion(route.ResourceVersion, deletion.resourceVersion)
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		switch {
		case comparison > 0:
		case comparison < 0:
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		default:
			deletion.generation = s.nextGenerationLocked()
			deletion.confirmationQueued = true
			s.deletionByObject[key] = deletion
			request := RepairRequest{ObjectKey: key, Generation: deletion.generation}
			return MutationResult{
				Result:         EventResultRepairRequired,
				Reason:         ReasonAmbiguousResourceVersion,
				RepairRequests: []RepairRequest{request},
			}
		}
	}

	if compatibility, exists := s.compatByUID[route.UID]; exists {
		comparison, err := resourceversion.CompareResourceVersion(
			route.ResourceVersion,
			compatibility.route.ResourceVersion,
		)
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		if comparison < 0 {
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		}
	}

	targetCompatibility := s.compatibilityClaimsLocked(route.ID, route.UID)
	supersedeCompatibility, err := allStrictlyOlder(targetCompatibility, route.ResourceVersion)
	if err != nil {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}
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
		return MutationResult{
			Result:         EventResultCollision,
			Reason:         ReasonUIDCollision,
			RepairRequests: deduplicateRepairRequests(append(displacedRequests, uidCollisionRequests...)),
		}
	}
	if compatibilityCollision || s.idCollidedLocked(route.ID) {
		requests := append(displacedRequests, s.repairRequestsForIDLocked(route.ID)...)
		return MutationResult{
			Result:         EventResultCollision,
			Reason:         ReasonIDCollision,
			RepairRequests: deduplicateRepairRequests(requests),
		}
	}
	return MutationResult{Result: EventResultApplied, RepairRequests: displacedRequests}
}

func (s *Store) upsertIDOnlyLocked(route Route) MutationResult {
	if s.idHasDeletionFenceLocked(route.ID) {
		return MutationResult{Result: EventResultIgnored, Reason: ReasonDeletionFence}
	}
	if _, retired := s.retiredByUID[route.UID]; retired {
		return MutationResult{Result: EventResultIgnored, Reason: ReasonRetiredUID}
	}
	if s.uidHasFullOwnerLocked(route.UID) || s.idHasFullOwnerLocked(route.ID) {
		return MutationResult{Result: EventResultIgnored, Reason: ReasonDominatedByFull}
	}
	if current, exists := s.compatByUID[route.UID]; exists {
		if current.route.ID != route.ID {
			return MutationResult{Result: EventResultCollision, Reason: ReasonUIDCollision}
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
	}

	s.compatByUID[route.UID] = routeRecord{
		route:        route,
		generation:   s.nextGenerationLocked(),
		lastObserved: s.now(),
	}
	s.recomputeActiveViewLocked()
	if s.idCollidedLocked(route.ID) {
		return MutationResult{Result: EventResultCollision, Reason: ReasonIDCollision}
	}
	return MutationResult{Result: EventResultApplied}
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
		if !s.uidHasOtherFullOwnerLocked(current.route.UID, key) {
			s.retiredByUID[current.route.UID] = retiredFence{
				createdAt: now,
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
				createdAt: now,
			}
		}
	}
	s.fullByObject[key] = routeRecord{
		route:       route,
		generation:  generation,
		quarantined: preserveQuarantine,
	}
}

func allStrictlyOlder(records []routeRecord, incomingResourceVersion string) (bool, error) {
	if len(records) == 0 {
		return false, nil
	}
	for _, record := range records {
		comparison, err := resourceversion.CompareResourceVersion(
			incomingResourceVersion,
			record.route.ResourceVersion,
		)
		if err != nil {
			return false, err
		}
		if comparison <= 0 {
			return false, nil
		}
	}
	return true, nil
}
