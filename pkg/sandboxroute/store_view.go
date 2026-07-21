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

	"github.com/openkruise/agents/pkg/metrics"
)

// Get returns the unique active route for id.
func (s *Store) Get(id string) (Route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	route, exists := s.activeByID[id]
	return route, exists
}

// List returns active routes sorted by ID.
func (s *Store) List() []Route {
	s.mu.RLock()
	defer s.mu.RUnlock()
	routes := make([]Route, 0, len(s.activeByID))
	for _, route := range s.activeByID {
		routes = append(routes, route)
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].ID < routes[j].ID
	})
	return routes
}

// Stats returns bounded physical and active-view counts.
func (s *Store) Stats() StoreStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statsLocked()
}

func (s *Store) compatibilityClaimsLocked(id string, excludeUID types.UID) []routeRecord {
	records := make([]routeRecord, 0)
	for uid, record := range s.compatByUID {
		if uid != excludeUID && record.route.ID == id {
			records = append(records, record)
		}
	}
	return records
}

func (s *Store) idHasFullOwnerLocked(id string) bool {
	for _, record := range s.fullByObject {
		if record.route.ID == id {
			return true
		}
	}
	return false
}

func (s *Store) uidHasFullOwnerLocked(uid types.UID) bool {
	for _, record := range s.fullByObject {
		if record.route.UID == uid {
			return true
		}
	}
	return false
}

func (s *Store) uidHasOtherFullOwnerLocked(uid types.UID, excludedKey types.NamespacedName) bool {
	for key, record := range s.fullByObject {
		if key != excludedKey && record.route.UID == uid {
			return true
		}
	}
	return false
}

func (s *Store) quarantineUIDClaimsLocked(uid types.UID, advanceGeneration bool) []RepairRequest {
	owners := make([]types.NamespacedName, 0, 2)
	for key, record := range s.fullByObject {
		if record.route.UID == uid {
			owners = append(owners, key)
		}
	}
	if len(owners) <= 1 {
		return nil
	}
	requests := make([]RepairRequest, 0, len(owners))
	for _, key := range owners {
		record := s.fullByObject[key]
		record.quarantined = true
		if advanceGeneration {
			record.generation = s.nextGenerationLocked()
		}
		s.fullByObject[key] = record
		requests = append(requests, RepairRequest{ObjectKey: key, Generation: record.generation})
	}
	sortRepairRequests(requests)
	return requests
}

func (s *Store) quarantineFullIDClaimsLocked(id string) []RepairRequest {
	requests := make([]RepairRequest, 0)
	for key, record := range s.fullByObject {
		if record.route.ID != id {
			continue
		}
		record.quarantined = true
		record.generation = s.nextGenerationLocked()
		s.fullByObject[key] = record
		requests = append(requests, RepairRequest{ObjectKey: key, Generation: record.generation})
	}
	sortRepairRequests(requests)
	return requests
}

func (s *Store) refreshQuarantinedUIDClaimsLocked(uid types.UID) []RepairRequest {
	requests := make([]RepairRequest, 0)
	for key, record := range s.fullByObject {
		if record.route.UID != uid || !record.quarantined {
			continue
		}
		record.generation = s.nextGenerationLocked()
		s.fullByObject[key] = record
		requests = append(requests, RepairRequest{ObjectKey: key, Generation: record.generation})
	}
	sortRepairRequests(requests)
	return requests
}

func (s *Store) idHasCompatibilityOwnerLocked(id string) bool {
	for _, record := range s.compatByUID {
		if record.route.ID == id {
			return true
		}
	}
	return false
}

func (s *Store) idHasDeletionFenceLocked(id string) bool {
	for _, fence := range s.deletionByObject {
		if fence.id == id {
			return true
		}
	}
	return false
}

func (s *Store) idCollidedLocked(id string) bool {
	_, collided := s.collisionsByID[id]
	return collided
}

func (s *Store) recomputeActiveViewLocked() {
	claims := make(map[string][]Route)
	forcedCollisions := make(map[string]struct{})
	for _, record := range s.fullByObject {
		claims[record.route.ID] = append(claims[record.route.ID], record.route)
		if record.quarantined {
			forcedCollisions[record.route.ID] = struct{}{}
		}
	}
	for _, record := range s.compatByUID {
		if s.idHasDeletionFenceLocked(record.route.ID) {
			continue
		}
		claims[record.route.ID] = append(claims[record.route.ID], record.route)
	}

	s.activeByID = make(map[string]Route, len(claims))
	s.collisionsByID = make(map[string]struct{})
	for id, routes := range claims {
		_, forced := forcedCollisions[id]
		if len(routes) == 1 && !forced {
			s.activeByID[id] = routes[0]
			continue
		}
		s.collisionsByID[id] = struct{}{}
	}
}

func (s *Store) setRecordMetricsLocked() {
	stats := s.statsLocked()
	metrics.SetSandboxRouteRecords(stats.IDOnly, stats.Collision)
}

func (s *Store) statsLocked() StoreStats {
	return StoreStats{
		Full:      len(s.fullByObject),
		IDOnly:    len(s.compatByUID),
		Retired:   len(s.retiredByUID),
		Deletion:  len(s.deletionByObject),
		Collision: len(s.collisionsByID),
		Active:    len(s.activeByID),
	}
}
