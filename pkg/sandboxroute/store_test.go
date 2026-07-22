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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

type upsertTransitionCase struct {
	name           string
	arrange        func(*Store)
	incoming       Route
	expectResult   EventResult
	expectReason   Reason
	expectIDs      []string
	expectRequests int
}

func TestStoreUpsertTransitions(t *testing.T) {
	tests := []upsertTransitionCase{
		{
			name: "initial full route", incoming: fullRoute("legacy", "ns", "one", "uid-a", "1"),
			expectResult: EventResultApplied, expectIDs: []string{"legacy"},
		},
		{
			name: "same identity equal RV accepted",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", "ns", "one", "uid-a", "1"))
			},
			incoming:     fullRoute("id", "ns", "one", "uid-a", "1"),
			expectResult: EventResultApplied, expectIDs: []string{"id"},
		},
		{
			name: "legacy to short migration",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("ns--one", "ns", "one", "uid-a", "1"))
			},
			incoming:     fullRoute("short", "ns", "one", "uid-a", "2"),
			expectResult: EventResultApplied, expectIDs: []string{"short"},
		},
		{
			name: "newer full route can remap short route",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("short", "ns", "one", "uid-a", "2"))
			},
			incoming:     fullRoute("ns--one", "ns", "one", "uid-a", "3"),
			expectResult: EventResultApplied, expectIDs: []string{"ns--one"},
		},
		{
			name: "equal RV cannot remap ID",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("ns--one", "ns", "one", "uid-a", "1"))
			},
			incoming:     fullRoute("short", "ns", "one", "uid-a", "1"),
			expectResult: EventResultIgnored, expectReason: ReasonStaleResourceVersion,
			expectIDs: []string{"ns--one"},
		},
		{
			name: "older RV ignored",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", "ns", "one", "uid-a", "2"))
			},
			incoming:     fullRoute("id", "ns", "one", "uid-a", "1"),
			expectResult: EventResultIgnored, expectReason: ReasonStaleResourceVersion,
			expectIDs: []string{"id"},
		},
		{
			name: "new object incarnation replaces old",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("old", "ns", "one", "uid-a", "1"))
			},
			incoming:     fullRoute("new", "ns", "one", "uid-b", "2"),
			expectResult: EventResultApplied, expectIDs: []string{"new"},
		},
		{
			name: "equal RV new incarnation requires repair",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("old", "ns", "one", "uid-a", "1"))
			},
			incoming:     fullRoute("new", "ns", "one", "uid-b", "1"),
			expectResult: EventResultRepairRequired, expectReason: ReasonAmbiguousResourceVersion,
			expectRequests: 1,
		},
		{
			name: "ID collision",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("same", "ns", "one", "uid-a", "1"))
			},
			incoming:     fullRoute("same", "ns", "two", "uid-b", "1"),
			expectResult: EventResultCollision, expectReason: ReasonIDCollision,
			expectRequests: 2,
		},
		{
			name:         "ID-only route rejected",
			incoming:     idOnlyRoute("ns--one", "uid-a", "1"),
			expectResult: EventResultInvalid, expectReason: ReasonInvalidRoute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, nil, time.Second)
			if tt.arrange != nil {
				tt.arrange(store)
			}
			result := store.Upsert(tt.incoming)
			assert.Equal(t, tt.expectResult, result.Result)
			assert.Equal(t, tt.expectReason, result.Reason)
			assert.Len(t, result.RepairRequests, tt.expectRequests)
			assert.Equal(t, tt.expectIDs, routeIDs(store.List()))
		})
	}
}

func TestStoreDeleteModes(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "one"}
	tests := []struct {
		name         string
		arrange      func(*Store)
		delete       func(*Store) MutationResult
		expectResult EventResult
		expectReason Reason
		expectIDs    []string
	}{
		{
			name: "authoritative ObjectKey delete",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", "ns", "one", "uid-a", "1"))
			},
			delete:       func(store *Store) MutationResult { return store.DeleteAuthoritativeByObjectKey(key) },
			expectResult: EventResultApplied,
		},
		{
			name:         "authoritative absent",
			delete:       func(store *Store) MutationResult { return store.DeleteAuthoritativeByObjectKey(key) },
			expectResult: EventResultIgnored, expectReason: ReasonAbsent,
		},
		{
			name: "invalid authoritative ObjectKey",
			delete: func(store *Store) MutationResult {
				return store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Name: "one"})
			},
			expectResult: EventResultInvalid, expectReason: ReasonInvalidObjectKey,
		},
		{
			name: "conditional equal delete",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", "ns", "one", "uid-a", "2"))
			},
			delete: func(store *Store) MutationResult {
				return store.DeleteConditionally(fullRoute("id", "ns", "one", "uid-a", "2"))
			},
			expectResult: EventResultApplied,
		},
		{
			name: "conditional older ignored",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", "ns", "one", "uid-a", "2"))
			},
			delete: func(store *Store) MutationResult {
				return store.DeleteConditionally(fullRoute("id", "ns", "one", "uid-a", "1"))
			},
			expectResult: EventResultIgnored, expectReason: ReasonStaleResourceVersion,
			expectIDs: []string{"id"},
		},
		{
			name: "old UID cannot delete replacement",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("old", "ns", "one", "uid-a", "1"))
				store.Upsert(fullRoute("new", "ns", "one", "uid-b", "2"))
			},
			delete: func(store *Store) MutationResult {
				return store.DeleteConditionally(fullRoute("old", "ns", "one", "uid-a", "9"))
			},
			expectResult: EventResultIgnored, expectReason: ReasonIdentityMismatch,
			expectIDs: []string{"new"},
		},
		{
			name: "ID-only delete rejected",
			delete: func(store *Store) MutationResult {
				return store.DeleteConditionally(idOnlyRoute("ns--one", "uid-a", "1"))
			},
			expectResult: EventResultInvalid, expectReason: ReasonInvalidRoute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, nil, time.Second)
			if tt.arrange != nil {
				tt.arrange(store)
			}
			result := tt.delete(store)
			assert.Equal(t, tt.expectResult, result.Result)
			assert.Equal(t, tt.expectReason, result.Reason)
			assert.Equal(t, tt.expectIDs, routeIDs(store.List()))
		})
	}
}

func TestStoreRepairGenerationAndDeletionFence(t *testing.T) {
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	store := newTestStore(t, func() time.Time { return now }, time.Second)
	key := types.NamespacedName{Namespace: "ns", Name: "one"}

	store.Upsert(fullRoute("old", "ns", "one", "uid-a", "1"))
	ambiguous := store.Upsert(fullRoute("new", "ns", "one", "uid-b", "1"))
	require.Len(t, ambiguous.RepairRequests, 1)

	store.Upsert(fullRoute("other", "ns", "other", "uid-c", "10"))
	result := store.ApplyAuthoritativeRepair(ambiguous.RepairRequests[0], AuthoritativeObservation{
		Present: true,
		Route:   fullRoute("new", "ns", "one", "uid-b", "1"),
	})
	assert.Equal(t, EventResultApplied, result.Result)
	assert.Equal(t, []string{"new", "other"}, routeIDs(store.List()))
	store.Upsert(fullRoute("new", "ns", "one", "uid-b", "2"))

	deleted := store.DeleteAuthoritativeByObjectKey(key)
	assert.Equal(t, EventResultApplied, deleted.Result)
	assert.Empty(t, store.Maintenance())

	stale := store.Upsert(fullRoute("new", "ns", "one", "uid-b", "1"))
	assert.Equal(t, EventResultIgnored, stale.Result)

	now = now.Add(time.Second)
	requests := store.Maintenance()
	require.Len(t, requests, 1)
	assert.Empty(t, store.Maintenance())

	repaired := store.ApplyAuthoritativeRepair(requests[0], AuthoritativeObservation{})
	assert.Equal(t, EventResultApplied, repaired.Result)
	assert.Empty(t, store.deletionByObject)
}

func TestStoreCollisionRepair(t *testing.T) {
	store := newTestStore(t, nil, time.Second)
	first := fullRoute("same", "ns", "one", "uid-a", "1")
	second := fullRoute("same", "ns", "two", "uid-b", "1")
	store.Upsert(first)
	collision := store.Upsert(second)
	require.Equal(t, EventResultCollision, collision.Result)
	require.Len(t, collision.RepairRequests, 2)
	assert.Empty(t, store.List())

	for _, request := range collision.RepairRequests {
		if request.ObjectKey.Name != "two" {
			continue
		}
		result := store.ApplyAuthoritativeRepair(request, AuthoritativeObservation{})
		assert.Equal(t, EventResultApplied, result.Result)
	}
	assert.Equal(t, []string{"same"}, routeIDs(store.List()))
}

func TestApplyAuthoritativeRepairValidation(t *testing.T) {
	store := newTestStore(t, nil, time.Second)
	tests := []struct {
		name         string
		request      RepairRequest
		observation  AuthoritativeObservation
		expectReason Reason
	}{
		{name: "empty request", expectReason: ReasonInvalidObjectKey},
		{
			name: "ID-only observation",
			request: RepairRequest{
				ObjectKey: types.NamespacedName{Namespace: "ns", Name: "one"}, Generation: 1,
			},
			observation:  AuthoritativeObservation{Present: true, Route: idOnlyRoute("ns--one", "uid", "1")},
			expectReason: ReasonInvalidRoute,
		},
		{
			name: "mismatched key",
			request: RepairRequest{
				ObjectKey: types.NamespacedName{Namespace: "ns", Name: "one"}, Generation: 1,
			},
			observation:  AuthoritativeObservation{Present: true, Route: fullRoute("id", "ns", "two", "uid", "1")},
			expectReason: ReasonIdentityMismatch,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := store.ApplyAuthoritativeRepair(tt.request, tt.observation)
			assert.Equal(t, EventResultInvalid, result.Result)
			assert.Equal(t, tt.expectReason, result.Reason)
		})
	}
}

func TestStoreConcurrentReadWrite(t *testing.T) {
	store := newTestStore(t, nil, time.Second)
	const workers = 16
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rv := 1; rv <= 100; rv++ {
				id := fmt.Sprintf("id-%d", worker)
				store.Upsert(fullRoute(id, "ns", id, types.UID(id), fmt.Sprint(rv)))
				store.Get(id)
				store.List()
			}
		}()
	}
	wg.Wait()
	assert.Len(t, store.List(), workers)
}

func TestStoreCollisionRecorder(t *testing.T) {
	calls := 0
	store := NewStore(StoreOptions{CollisionRecorder: func() { calls++ }})
	store.Upsert(fullRoute("same", "ns", "one", "uid-a", "1"))
	result := store.Upsert(fullRoute("same", "ns", "two", "uid-b", "1"))
	assert.Equal(t, EventResultCollision, result.Result)
	assert.Equal(t, 1, calls)
}

func newTestStore(t *testing.T, now func() time.Time, deletionFenceDelay time.Duration) *Store {
	t.Helper()
	return NewStore(StoreOptions{Now: now, DeletionFenceDelay: deletionFenceDelay})
}

func routeIDs(routes []Route) []string {
	if len(routes) == 0 {
		return nil
	}
	ids := make([]string, 0, len(routes))
	for _, route := range routes {
		ids = append(ids, route.ID)
	}
	return ids
}

func storeIsEmpty(store *Store) bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return len(store.recordByObject) == 0 && len(store.deletionByObject) == 0 &&
		len(store.activeByID) == 0 && len(store.collisionsByID) == 0
}
