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

// Delete removes a route when its ObjectKey, UID, and resource-version fences match.
func (s *Store) Delete(route Route) MutationResult {
	route, err := AdmitRoute(route)
	if err != nil {
		return MutationResult{
			Result: EventResultInvalid,
			Reason: ReasonInvalidRoute,
		}
	}

	key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
	s.mu.Lock()
	defer s.mu.Unlock()

	current, exists := s.recordByObject[key]
	if !exists {
		return MutationResult{
			Result: EventResultIgnored,
			Reason: ReasonAbsent,
		}
	}
	if current.route.UID != route.UID {
		return MutationResult{
			Result: EventResultIgnored,
			Reason: ReasonIdentityMismatch,
		}
	}

	comparison, err := resourceversion.CompareResourceVersion(
		route.ResourceVersion,
		current.route.ResourceVersion,
	)
	if err != nil {
		return MutationResult{
			Result: EventResultInvalid,
			Reason: ReasonInvalidRoute,
		}
	}
	if comparison < 0 {
		return MutationResult{
			Result: EventResultIgnored,
			Reason: ReasonStaleResourceVersion,
		}
	}
	s.deleteRecordLocked(key, current, route.ResourceVersion, false)
	return MutationResult{Result: EventResultApplied}
}

func (s *Store) deleteRecordLocked(
	key types.NamespacedName,
	current routeRecord,
	fenceResourceVersion string,
	confirmed bool,
) {
	generation := s.nextGenerationLocked()
	// A record and deletion fence cannot coexist: every route install removes
	// the fence before it stores the authoritative record for this ObjectKey.
	s.deletionByObject[key] = deletionFence{
		resourceVersion:    fenceResourceVersion,
		generation:         generation,
		createdAt:          s.now(),
		confirmationQueued: confirmed,
		confirmed:          confirmed,
	}
	s.deactivateRouteLocked(key, current.route.ID)
	delete(s.recordByObject, key)
}
