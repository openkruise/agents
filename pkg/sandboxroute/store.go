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
	"fmt"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/metrics"
)

// CompatibilityDrainWindow is the default old-peer retry and drain window.
const CompatibilityDrainWindow = 2 * time.Second

// Surface identifies the component that owns a route Store.
type Surface string

const (
	SurfaceManager Surface = metrics.RouteSurfaceManager
	SurfaceGateway Surface = metrics.RouteSurfaceGateway
)

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
	ReasonDominatedByFull          Reason = "dominated_by_full"
	ReasonDeletionFence            Reason = "deletion_fence"
	ReasonRetiredUID               Reason = "retired_uid"
	ReasonIDCollision              Reason = "id_collision"
	ReasonUIDCollision             Reason = "uid_collision"
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
	Now         func() time.Time
	DrainWindow time.Duration
	// CollisionRecorder must be non-blocking and must not call back into Store.
	CollisionRecorder func()
}

// StoreStats contains bounded physical and active-view counts.
type StoreStats struct {
	Full      int
	IDOnly    int
	Retired   int
	Deletion  int
	Collision int
	Active    int
}

type routeRecord struct {
	route        Route
	generation   uint64
	lastObserved time.Time
	quarantined  bool
}

type retiredFence struct {
	createdAt time.Time
}

type deletionFence struct {
	uid                types.UID
	id                 string
	resourceVersion    string
	generation         uint64
	createdAt          time.Time
	confirmationQueued bool
	confirmed          bool
}

// Store owns full, compatibility, retired, deletion, collision, and active route state.
type Store struct {
	mu              sync.RWMutex
	surface         Surface
	now             func() time.Time
	drainWindow     time.Duration
	recordCollision func()

	generation       uint64
	fullByObject     map[types.NamespacedName]routeRecord
	fullByUID        map[types.UID]map[types.NamespacedName]struct{}
	compatByUID      map[types.UID]routeRecord
	retiredByUID     map[types.UID]retiredFence
	deletionByObject map[types.NamespacedName]deletionFence
	activeByID       map[string]Route
	collisionsByID   map[string]struct{}
}

// NewStore creates an empty Store for one supported component surface.
func NewStore(surface Surface) (*Store, error) {
	return NewStoreWithOptions(surface, StoreOptions{})
}

// NewStoreWithOptions creates an empty Store with optional runtime dependencies.
func NewStoreWithOptions(surface Surface, options StoreOptions) (*Store, error) {
	if surface != SurfaceManager && surface != SurfaceGateway {
		return nil, fmt.Errorf("unsupported route Store surface %q", surface)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	drainWindow := options.DrainWindow
	if drainWindow <= 0 {
		drainWindow = CompatibilityDrainWindow
	}
	store := &Store{
		surface:          surface,
		now:              now,
		drainWindow:      drainWindow,
		recordCollision:  options.CollisionRecorder,
		fullByObject:     make(map[types.NamespacedName]routeRecord),
		fullByUID:        make(map[types.UID]map[types.NamespacedName]struct{}),
		compatByUID:      make(map[types.UID]routeRecord),
		retiredByUID:     make(map[types.UID]retiredFence),
		deletionByObject: make(map[types.NamespacedName]deletionFence),
		activeByID:       make(map[string]Route),
		collisionsByID:   make(map[string]struct{}),
	}
	store.setRecordMetricsLocked()
	return store, nil
}

// Surface returns the component surface owning this Store.
func (s *Store) Surface() Surface {
	return s.surface
}

func hasExpectedShape(route Route, expected Shape) bool {
	if err := route.Validate(); err != nil {
		return false
	}
	shape, err := route.Shape()
	return err == nil && shape == expected
}

func equalOrNewer(current, incoming string) bool {
	comparison := CompareResourceVersions(current, incoming)
	return comparison == ResourceVersionEqual || comparison == ResourceVersionNewer
}

func (s *Store) recordWithoutMutation(result EventResult, reason Reason) MutationResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finishLocked(result, reason, nil)
}

// RecordInvalid records a decoded route that was rejected before Store dispatch.
func (s *Store) RecordInvalid() MutationResult {
	return s.recordWithoutMutation(EventResultInvalid, ReasonInvalidRoute)
}

func (s *Store) finishLocked(
	result EventResult,
	reason Reason,
	requests []RepairRequest,
) MutationResult {
	if result == EventResultInvalid {
		metrics.RecordSandboxRouteInvalid(string(s.surface))
	}
	if result == EventResultCollision && s.recordCollision != nil {
		s.recordCollision()
	}
	s.setRecordMetricsLocked()
	return MutationResult{
		Result:         result,
		Reason:         reason,
		RepairRequests: append([]RepairRequest(nil), requests...),
	}
}
