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

func (s *Store) idHasOwnerLocked(id string) bool {
	for _, record := range s.recordByObject {
		if record.route.ID == id {
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
	for _, record := range s.recordByObject {
		claims[record.route.ID] = append(claims[record.route.ID], record.route)
		if record.quarantined {
			forcedCollisions[record.route.ID] = struct{}{}
		}
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
	metrics.SetSandboxRouteCollisionRecords(len(s.collisionsByID))
}
