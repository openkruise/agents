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

// Delete applies an authoritative deletion. A non-empty resource version may
// establish a fence even when the ObjectKey has no existing Store state.
func (s *Store) Delete(deletion Delete) MutationResult {
	return s.delete(deletion, false)
}

// DeleteIfTracked applies a policy-exclusion deletion only when the ObjectKey
// already has a record or fence. It never creates state for an unseen key.
func (s *Store) DeleteIfTracked(deletion Delete) MutationResult {
	return s.delete(deletion, true)
}

func (s *Store) delete(deletion Delete, trackedOnly bool) MutationResult {
	if reason := deletion.invalidReason(!trackedOnly); reason != ReasonNone {
		return MutationResult{Result: EventResultInvalid, Reason: reason}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	key := deletion.ObjectKey
	current, hasCurrent := s.recordByObject[key]
	fenceResourceVersion, hasFence := s.deletionByObject[key]

	if deletion.ResourceVersion == "" {
		if hasCurrent {
			s.deleteRecordLocked(key, current, current.ResourceVersion)
		}
		return MutationResult{Result: EventResultApplied}
	}

	if trackedOnly && !hasCurrent && !hasFence {
		return MutationResult{Result: EventResultApplied}
	}

	currentResourceVersion := fenceResourceVersion
	if hasCurrent {
		currentResourceVersion = current.ResourceVersion
	}
	if currentResourceVersion != "" {
		comparison, err := resourceversion.CompareResourceVersion(
			deletion.ResourceVersion,
			currentResourceVersion,
		)
		if err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
		if comparison < 0 {
			return MutationResult{Result: EventResultIgnored, Reason: ReasonStaleResourceVersion}
		}
	}

	if hasCurrent {
		s.deleteRecordLocked(key, current, deletion.ResourceVersion)
	} else {
		s.deletionByObject[key] = deletion.ResourceVersion
	}
	return MutationResult{Result: EventResultApplied}
}

func (s *Store) deleteRecordLocked(
	key types.NamespacedName,
	current Route,
	fenceResourceVersion string,
) {
	s.deactivateRouteLocked(key, current.ID)
	delete(s.recordByObject, key)
	s.deletionByObject[key] = fenceResourceVersion
}
