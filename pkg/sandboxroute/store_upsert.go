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

// Upsert installs a Route only when its resource version is strictly newer
// than the ObjectKey's current record or deletion fence.
func (s *Store) Upsert(route Route) MutationResult {
	route, err := AdmitRoute(route)
	if err != nil {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
	currentResourceVersion := ""
	if current, exists := s.recordByObject[key]; exists {
		currentResourceVersion = current.ResourceVersion
	} else {
		currentResourceVersion = s.deletionByObject[key]
	}
	if currentResourceVersion != "" {
		comparison, err := resourceversion.CompareResourceVersion(route.ResourceVersion, currentResourceVersion)
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		if comparison <= 0 {
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		}
	}

	s.installRouteLocked(key, route)
	return MutationResult{Result: EventResultApplied}
}

func (s *Store) installRouteLocked(key types.NamespacedName, route Route) {
	if current, exists := s.recordByObject[key]; exists && current.ID != route.ID {
		s.deactivateRouteLocked(key, current.ID)
	}
	delete(s.deletionByObject, key)
	s.recordByObject[key] = route
	s.activeKeyByID[route.ID] = key
}
