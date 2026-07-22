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

	"github.com/openkruise/agents/pkg/metrics"
)

// DeletionFenceConfirmationDelay is the delay before an authoritative deletion is confirmed.
const DeletionFenceConfirmationDelay = 2 * time.Second

// EventResult identifies the result of a route mutation event.
type EventResult string

const (
	EventResultApplied        EventResult = "applied"
	EventResultIgnored        EventResult = "ignored"
	EventResultInvalid        EventResult = "invalid"
	EventResultCollision      EventResult = "collision"
	EventResultRepairRequired EventResult = "repair_required"
)

// RepairResult identifies a targeted repair outcome for structured logs.
type RepairResult string

const (
	RepairResultSuccess         RepairResult = "success"
	RepairResultGetError        RepairResult = "get_error"
	RepairResultProjectionError RepairResult = "projection_error"
	RepairResultStale           RepairResult = "stale"
)

// Reason identifies a fixed explanation for a mutation result.
type Reason string

const (
	ReasonNone                     Reason = ""
	ReasonInvalidRoute             Reason = "invalid_route"
	ReasonStaleResourceVersion     Reason = "stale_resource_version"
	ReasonIDCollision              Reason = "id_collision"
	ReasonAbsent                   Reason = "absent"
	ReasonIdentityMismatch         Reason = "identity_mismatch"
	ReasonInvalidObjectKey         Reason = "invalid_object_key"
	ReasonAmbiguousResourceVersion Reason = "ambiguous_resource_version"
	ReasonStaleRepairGeneration    Reason = "stale_repair_generation"
	ReasonAuthoritativePresent     Reason = "authoritative_present"
	ReasonAuthoritativeAbsent      Reason = "authoritative_absent"
)

// RepairRequest identifies one affected ObjectKey generation to read directly.
type RepairRequest struct {
	ObjectKey  types.NamespacedName
	Generation uint64
}

// MutationResult describes the outcome of one Store mutation request.
type MutationResult struct {
	Result         EventResult
	Reason         Reason
	RepairRequests []RepairRequest
}

// AuthoritativeObservation is one scoped direct-reader result for an ObjectKey.
// Present=false represents NotFound, deletion, or component-policy exclusion.
type AuthoritativeObservation struct {
	Present bool
	Route   Route
}

// StoreOptions provides optional runtime dependencies for a Store.
type StoreOptions struct {
	Now                func() time.Time
	DeletionFenceDelay time.Duration
	// CollisionRecorder must be non-blocking and must not call back into Store.
	CollisionRecorder func()
}

type routeRecord struct {
	route       Route
	generation  uint64
	quarantined bool
}

type deletionFence struct {
	resourceVersion    string
	generation         uint64
	createdAt          time.Time
	confirmationQueued bool
	confirmed          bool
}

// Store owns source records, transition fences, and a derived active/collision view.
type Store struct {
	mu                 sync.RWMutex
	now                func() time.Time
	deletionFenceDelay time.Duration
	recordCollision    func()

	generation       uint64
	recordByObject   map[types.NamespacedName]routeRecord
	deletionByObject map[types.NamespacedName]deletionFence

	activeByID     map[string]Route
	collisionsByID map[string]struct{}
}

// NewStore creates an empty Store with optional runtime dependencies.
func NewStore(options StoreOptions) *Store {
	now := options.Now
	if now == nil {
		now = time.Now
	}
	deletionFenceDelay := options.DeletionFenceDelay
	if deletionFenceDelay <= 0 {
		deletionFenceDelay = DeletionFenceConfirmationDelay
	}
	store := &Store{
		now:                now,
		deletionFenceDelay: deletionFenceDelay,
		recordCollision:    options.CollisionRecorder,
		recordByObject:     make(map[types.NamespacedName]routeRecord),
		deletionByObject:   make(map[types.NamespacedName]deletionFence),
		activeByID:         make(map[string]Route),
		collisionsByID:     make(map[string]struct{}),
	}
	store.setRecordMetricsLocked()
	return store
}

// RecordInvalid records a decoded route that was rejected before Store dispatch.
func (s *Store) RecordInvalid() MutationResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishLocked(MutationResult{
		Result: EventResultInvalid,
		Reason: ReasonInvalidRoute,
	})
}

// finishLocked records a route mutation event and returns the result. Must be called with s.mu.Lock held.
func (s *Store) finishLocked(result MutationResult) MutationResult {
	if result.Result == EventResultInvalid {
		metrics.RecordSandboxRouteInvalid()
	}
	if result.Result == EventResultCollision && s.recordCollision != nil {
		s.recordCollision()
	}
	s.setRecordMetricsLocked()
	result.RepairRequests = append([]RepairRequest(nil), result.RepairRequests...)
	return result
}
