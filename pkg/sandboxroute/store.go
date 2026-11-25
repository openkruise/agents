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
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/resourceversion"
)

// EventResult identifies the result of a route mutation event.
type EventResult string

const (
	EventResultApplied EventResult = "applied"
	EventResultIgnored EventResult = "ignored"
	EventResultInvalid EventResult = "invalid"
)

// Reason identifies a fixed explanation for a mutation result.
type Reason string

const (
	ReasonInvalidRoute         Reason = "invalid_route"
	ReasonStaleResourceVersion Reason = "stale_resource_version"
	// ReasonIDTakeover marks an applied upsert whose ID was active for another
	// ObjectKey; the incoming route becomes the active lookup result.
	ReasonIDTakeover Reason = "id_takeover"
)

// MutationResult describes the outcome of one Store mutation request.
type MutationResult struct {
	Result EventResult
	Reason Reason
}

const (
	deletionFenceRetention       = 10 * time.Minute
	deletionFenceCleanupInterval = time.Minute
)

type deletionFence struct {
	resourceVersion string
	expiresAt       time.Time
}

// Store owns source records, deletion fences, and an active ID-to-ObjectKey index.
// A record and a deletion fence for the same ObjectKey never coexist.
// Supported producers must supply IDs that are unique across ObjectKeys.
// A duplicate-ID upsert defensively takes over the active lookup while the
// displaced record remains as its ObjectKey RV watermark. Deleting the takeover
// leaves the ID inactive until a newer displaced-ObjectKey observation arrives,
// so stale events cannot revive it.
type Store struct {
	mu                       sync.RWMutex
	recordByObject           map[types.NamespacedName]Route
	deletionByObject         map[types.NamespacedName]deletionFence
	activeKeyByID            map[string]types.NamespacedName
	nextDeletionFenceCleanup time.Time
}

// NewStore creates an empty Store.
func NewStore() *Store {
	return &Store{
		recordByObject:   make(map[types.NamespacedName]Route),
		deletionByObject: make(map[types.NamespacedName]deletionFence),
		activeKeyByID:    make(map[string]types.NamespacedName),
	}
}

// Upsert installs a Route only when its resource version is strictly newer
// than the ObjectKey's current record or deletion fence.
func (s *Store) Upsert(route Route) MutationResult {
	if err := route.validate(); err != nil {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupDeletionFencesLocked(time.Now())

	key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
	currentResourceVersion := ""
	if current, exists := s.recordByObject[key]; exists {
		currentResourceVersion = current.ResourceVersion
	} else {
		currentResourceVersion = s.deletionByObject[key].resourceVersion
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

	result := MutationResult{Result: EventResultApplied}
	if s.installRouteLocked(key, route) {
		result.Reason = ReasonIDTakeover
	}
	return result
}

// Delete removes one ObjectKey. A non-empty resource version may establish a
// fence even without a prior record; an empty one removes the current record
// using its RV as the fence and is a no-op when no record exists.
func (s *Store) Delete(route Route) MutationResult {
	key, ok := route.ObjectKey()
	if !ok {
		return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
	}
	if route.ResourceVersion != "" {
		if err := validateResourceVersion(route.ResourceVersion); err != nil {
			return MutationResult{Result: EventResultInvalid, Reason: ReasonInvalidRoute}
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.cleanupDeletionFencesLocked(now)

	current, hasCurrent := s.recordByObject[key]
	fenceResourceVersion := s.deletionByObject[key].resourceVersion

	if route.ResourceVersion == "" {
		if hasCurrent {
			s.deleteRecordLocked(key, current, current.ResourceVersion, now)
		}
		return MutationResult{Result: EventResultApplied}
	}

	currentResourceVersion := fenceResourceVersion
	if hasCurrent {
		currentResourceVersion = current.ResourceVersion
	}
	if currentResourceVersion != "" {
		comparison, err := resourceversion.CompareResourceVersion(
			route.ResourceVersion,
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
		s.deleteRecordLocked(key, current, route.ResourceVersion, now)
	} else if fence, ok := s.deletionByObject[key]; ok {
		// Keep the original retention deadline; only advance the watermark RV.
		fence.resourceVersion = route.ResourceVersion
		s.deletionByObject[key] = fence
	} else {
		s.deletionByObject[key] = deletionFence{
			resourceVersion: route.ResourceVersion,
			expiresAt:       now.Add(deletionFenceRetention),
		}
	}
	return MutationResult{Result: EventResultApplied}
}

// Get returns the unique active route for id.
func (s *Store) Get(id string) (Route, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key, exists := s.activeKeyByID[id]
	if !exists {
		return Route{}, false
	}
	record, exists := s.recordByObject[key]
	if !exists || record.ID != id {
		return Route{}, false
	}
	return record, true
}

// Len returns the number of active routes.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.activeKeyByID)
}

func (s *Store) installRouteLocked(key types.NamespacedName, route Route) (idTakenOver bool) {
	if current, exists := s.recordByObject[key]; exists && current.ID != route.ID {
		s.deactivateRouteLocked(key, current.ID)
	}
	if previousKey, exists := s.activeKeyByID[route.ID]; exists && previousKey != key {
		idTakenOver = true
	}
	delete(s.deletionByObject, key)
	s.recordByObject[key] = route
	s.activeKeyByID[route.ID] = key
	return idTakenOver
}

func (s *Store) deleteRecordLocked(
	key types.NamespacedName,
	current Route,
	fenceResourceVersion string,
	now time.Time,
) {
	s.deactivateRouteLocked(key, current.ID)
	delete(s.recordByObject, key)
	s.deletionByObject[key] = deletionFence{
		resourceVersion: fenceResourceVersion,
		expiresAt:       now.Add(deletionFenceRetention),
	}
}

func (s *Store) cleanupDeletionFencesLocked(now time.Time) {
	if now.Before(s.nextDeletionFenceCleanup) {
		return
	}
	// mutation-driven cleanup avoids a Store lifecycle;
	// add one only if idle-time eviction becomes an observable requirement.
	for key, fence := range s.deletionByObject {
		if now.After(fence.expiresAt) {
			delete(s.deletionByObject, key)
		}
	}
	s.nextDeletionFenceCleanup = now.Add(deletionFenceCleanupInterval)
}

func (s *Store) deactivateRouteLocked(key types.NamespacedName, id string) {
	if activeKey, exists := s.activeKeyByID[id]; exists && activeKey == key {
		delete(s.activeKeyByID, id)
	}
}
