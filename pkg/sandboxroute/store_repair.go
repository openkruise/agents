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

	"k8s.io/apimachinery/pkg/types"
)

// ApplyAuthoritativeRepair applies a scoped direct-read observation only when
// the affected ObjectKey record or fence still has the requested generation.
func (s *Store) ApplyAuthoritativeRepair(
	request RepairRequest,
	observation AuthoritativeObservation,
) MutationResult {
	if observation.Present {
		route, err := AdmitRoute(observation.Route)
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		observation.Route = route
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.applyAuthoritativeRepairLocked(request, observation)
}

func (s *Store) applyAuthoritativeRepairLocked(
	request RepairRequest,
	observation AuthoritativeObservation,
) MutationResult {
	if request.ObjectKey.Namespace == "" || request.ObjectKey.Name == "" || request.Generation == 0 {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidObjectKey}
	}
	if observation.Present {
		key := types.NamespacedName{
			Namespace: observation.Route.Namespace,
			Name:      observation.Route.Name,
		}
		if key != request.ObjectKey {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonIdentityMismatch}
		}
	}

	generation, exists := s.affectedGenerationLocked(request.ObjectKey)
	if !exists || generation != request.Generation {
		return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleRepairGeneration}
	}
	if !observation.Present {
		return s.applyAuthoritativeAbsenceLocked(request.ObjectKey)
	}
	return s.applyAuthoritativePresenceLocked(request.ObjectKey, observation.Route)
}

// Maintenance returns deletion-fence confirmations that must be observed
// through the Repairer direct-read callback.
func (s *Store) Maintenance() []RepairRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	requests := make([]RepairRequest, 0)
	for key, fence := range s.deletionByObject {
		if now.Sub(fence.createdAt) < s.deletionFenceDelay {
			continue
		}
		if fence.confirmed {
			delete(s.deletionByObject, key)
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
	sortRepairRequests(requests)
	return requests
}

func (s *Store) applyAuthoritativePresenceLocked(
	key types.NamespacedName,
	route Route,
) MutationResult {
	s.installRouteLocked(key, route)
	return MutationResult{
		Result: EventResultApplied,
		Reason: ReasonAuthoritativePresent,
	}
}

func (s *Store) applyAuthoritativeAbsenceLocked(key types.NamespacedName) MutationResult {
	now := s.now()
	if current, exists := s.recordByObject[key]; exists {
		s.deleteRecordLocked(key, current, current.route.ResourceVersion, true)
		return MutationResult{
			Result: EventResultApplied,
			Reason: ReasonAuthoritativeAbsent,
		}
	}

	fence := s.deletionByObject[key]
	if now.Sub(fence.createdAt) >= s.deletionFenceDelay {
		delete(s.deletionByObject, key)
		s.nextGenerationLocked()
		return MutationResult{Result: EventResultApplied, Reason: ReasonAuthoritativeAbsent}
	}
	fence.confirmed = true
	fence.confirmationQueued = true
	fence.generation = s.nextGenerationLocked()
	s.deletionByObject[key] = fence
	return MutationResult{Result: EventResultApplied, Reason: ReasonAuthoritativeAbsent}
}

func (s *Store) nextGenerationLocked() uint64 {
	s.generation++
	return s.generation
}

func (s *Store) affectedGenerationLocked(key types.NamespacedName) (uint64, bool) {
	if record, exists := s.recordByObject[key]; exists {
		return record.generation, true
	}
	if fence, exists := s.deletionByObject[key]; exists {
		return fence.generation, true
	}
	return 0, false
}

func sortRepairRequests(requests []RepairRequest) {
	sort.Slice(requests, func(i, j int) bool {
		if requests[i].ObjectKey.Namespace == requests[j].ObjectKey.Namespace {
			return requests[i].ObjectKey.Name < requests[j].ObjectKey.Name
		}
		return requests[i].ObjectKey.Namespace < requests[j].ObjectKey.Namespace
	})
}
