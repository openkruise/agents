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
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// ApplyAuthoritativeRepair applies a scoped direct-read observation only when
// the affected ObjectKey record or fence still has the requested generation.
func (s *Store) ApplyAuthoritativeRepair(
	request RepairRequest,
	observation AuthoritativeObservation,
) MutationResult {
	if request.ObjectKey.Namespace == "" || request.ObjectKey.Name == "" || request.Generation == 0 {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidObjectKey}
	}
	if observation.Present {
		if !hasExpectedShape(observation.Route, ShapeFull) {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		key, _ := observation.Route.ObjectKey()
		if key != request.ObjectKey {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonIdentityMismatch}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	generation, exists := s.affectedGenerationLocked(request.ObjectKey)
	if !exists || generation != request.Generation {
		return s.finishRepairLocked(EventResultIgnored, ReasonStaleRepairGeneration, nil)
	}
	if !observation.Present {
		return s.applyAuthoritativeAbsenceLocked(request.ObjectKey)
	}
	return s.applyAuthoritativePresenceLocked(request.ObjectKey, observation.Route)
}

// Maintenance expires compatibility-only state and returns deletion-fence
// confirmations that must be observed through the Repairer direct-read callback.
func (s *Store) Maintenance() []RepairRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	changedView, requests := s.expireCompatibilityLocked(now)
	for key, fence := range s.deletionByObject {
		if now.Sub(fence.createdAt) < s.drainWindow {
			continue
		}
		if fence.confirmed {
			delete(s.deletionByObject, key)
			s.pruneRetiredUIDLocked(fence.uid, now)
			continue
		}
		if fence.confirmationQueued {
			continue
		}
		fence.generation = s.nextGenerationLocked()
		fence.confirmationQueued = true
		s.deletionByObject[key] = fence
		requests = append(requests, RepairRequest{ObjectKey: key, Generation: fence.generation})
	}

	for uid, fence := range s.retiredByUID {
		if now.Sub(fence.createdAt) >= s.drainWindow && !s.uidHasDeletionFenceLocked(uid) {
			delete(s.retiredByUID, uid)
		}
	}
	if changedView {
		s.nextGenerationLocked()
		s.recomputeActiveViewLocked()
	}
	requests = deduplicateRepairRequests(requests)
	s.setRecordMetricsLocked()
	return requests
}

func (s *Store) applyAuthoritativePresenceLocked(
	key types.NamespacedName,
	route Route,
) MutationResult {
	displacedUID := types.UID("")
	if current, exists := s.fullByObject[key]; exists && current.route.UID != route.UID {
		displacedUID = current.route.UID
	}
	_, alreadyOwned := s.fullByUID[route.UID][key]
	s.installFullLocked(key, route, true, false)
	displacedRequests := s.refreshQuarantinedUIDClaimsLocked(displacedUID)
	uidCollisionRequests := s.quarantineUIDClaimsLocked(route.UID, !alreadyOwned)
	s.recomputeActiveViewLocked()
	if len(uidCollisionRequests) > 0 {
		if alreadyOwned {
			uidCollisionRequests = nil
		}
		requests := deduplicateRepairRequests(append(displacedRequests, uidCollisionRequests...))
		return s.finishRepairLocked(EventResultCollision, ReasonUIDCollision, requests)
	}
	if s.idCollidedLocked(route.ID) {
		return s.finishRepairLocked(EventResultCollision, ReasonIDCollision, displacedRequests)
	}
	return s.finishRepairLocked(EventResultApplied, ReasonAuthoritativePresent, displacedRequests)
}

func (s *Store) applyAuthoritativeAbsenceLocked(key types.NamespacedName) MutationResult {
	now := s.now()
	if current, exists := s.fullByObject[key]; exists {
		delete(s.fullByObject, key)
		s.removeFullUIDOwnerLocked(current.route.UID, key)
		generation := s.nextGenerationLocked()
		s.installDeletionFencesLocked(key, current.route, current.route.ResourceVersion, generation, true)
		requests := s.refreshQuarantinedUIDClaimsLocked(current.route.UID)
		s.recomputeActiveViewLocked()
		return s.finishRepairLocked(EventResultApplied, ReasonAuthoritativeAbsent, requests)
	}

	fence := s.deletionByObject[key]
	if now.Sub(fence.createdAt) >= s.drainWindow {
		delete(s.deletionByObject, key)
		s.pruneRetiredUIDLocked(fence.uid, now)
		s.nextGenerationLocked()
		return s.finishRepairLocked(EventResultApplied, ReasonAuthoritativeAbsent, nil)
	}
	fence.confirmed = true
	fence.confirmationQueued = true
	fence.generation = s.nextGenerationLocked()
	s.deletionByObject[key] = fence
	return s.finishRepairLocked(EventResultApplied, ReasonAuthoritativeAbsent, nil)
}

func (s *Store) expireCompatibilityLocked(now time.Time) (bool, []RepairRequest) {
	claimsByID := make(map[string][]types.UID)
	for uid, record := range s.compatByUID {
		claimsByID[record.route.ID] = append(claimsByID[record.route.ID], uid)
	}
	changed := false
	requests := make([]RepairRequest, 0)
	for id, uids := range claimsByID {
		allExpired := true
		for _, uid := range uids {
			if now.Sub(s.compatByUID[uid].lastObserved) < s.drainWindow {
				allExpired = false
				break
			}
		}
		if len(uids) > 1 && !allExpired {
			continue
		}
		wasCollided := s.idCollidedLocked(id)
		removed := false
		for _, uid := range uids {
			if allExpired || now.Sub(s.compatByUID[uid].lastObserved) >= s.drainWindow {
				delete(s.compatByUID, uid)
				changed = true
				removed = true
			}
		}
		if removed && wasCollided {
			requests = append(requests, s.quarantineFullIDClaimsLocked(id)...)
		}
	}
	return changed, deduplicateRepairRequests(requests)
}

func (s *Store) nextGenerationLocked() uint64 {
	s.generation++
	return s.generation
}

func (s *Store) affectedGenerationLocked(key types.NamespacedName) (uint64, bool) {
	if record, exists := s.fullByObject[key]; exists {
		return record.generation, true
	}
	if fence, exists := s.deletionByObject[key]; exists {
		return fence.generation, true
	}
	return 0, false
}

func (s *Store) repairRequestsForIDLocked(id string) []RepairRequest {
	requests := make([]RepairRequest, 0)
	for key, record := range s.fullByObject {
		if record.route.ID == id {
			requests = append(requests, RepairRequest{ObjectKey: key, Generation: record.generation})
		}
	}
	sortRepairRequests(requests)
	return requests
}

func sortRepairRequests(requests []RepairRequest) {
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].ObjectKey.Namespace == requests[j].ObjectKey.Namespace {
			return requests[i].ObjectKey.Name < requests[j].ObjectKey.Name
		}
		return requests[i].ObjectKey.Namespace < requests[j].ObjectKey.Namespace
	})
}

func deduplicateRepairRequests(requests []RepairRequest) []RepairRequest {
	newestByKey := make(map[types.NamespacedName]RepairRequest, len(requests))
	for _, request := range requests {
		current, exists := newestByKey[request.ObjectKey]
		if !exists || current.Generation < request.Generation {
			newestByKey[request.ObjectKey] = request
		}
	}
	deduplicated := make([]RepairRequest, 0, len(newestByKey))
	for _, request := range newestByKey {
		deduplicated = append(deduplicated, request)
	}
	sortRepairRequests(deduplicated)
	return deduplicated
}

func (s *Store) uidHasDeletionFenceLocked(uid types.UID) bool {
	for _, fence := range s.deletionByObject {
		if fence.uid == uid {
			return true
		}
	}
	return false
}

func (s *Store) pruneRetiredUIDLocked(uid types.UID, now time.Time) {
	fence, exists := s.retiredByUID[uid]
	if !exists || now.Sub(fence.createdAt) < s.drainWindow || s.uidHasDeletionFenceLocked(uid) {
		return
	}
	delete(s.retiredByUID, uid)
}
