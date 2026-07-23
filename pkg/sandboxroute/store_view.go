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

// Get returns the unique active route for id.
func (s *Store) Get(id string) (Route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, exists := s.activeKeyByID[id]
	if !exists {
		return Route{}, false
	}
	record, exists := s.recordByObject[key]
	if !exists || record.route.ID != id {
		return Route{}, false
	}
	return record.route, true
}

// GetByObjectKey returns the authoritative Route snapshot for key.
// The returned Route contains its access token and must not be serialized for logging.
func (s *Store) GetByObjectKey(key types.NamespacedName) (Route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, exists := s.recordByObject[key]
	if !exists {
		return Route{}, false
	}
	return record.route, true
}

// List returns active routes sorted by ID.
func (s *Store) List() []Route {
	s.mu.RLock()
	defer s.mu.RUnlock()
	routes := make([]Route, 0, len(s.activeKeyByID))
	for id, key := range s.activeKeyByID {
		record, exists := s.recordByObject[key]
		if !exists || record.route.ID != id {
			continue
		}
		routes = append(routes, record.route)
	}
	sort.Slice(routes, func(i, j int) bool {
		return routes[i].ID < routes[j].ID
	})
	return routes
}

func (s *Store) deactivateRouteLocked(key types.NamespacedName, id string) {
	if activeKey, exists := s.activeKeyByID[id]; exists && activeKey == key {
		delete(s.activeKeyByID, id)
	}
}
