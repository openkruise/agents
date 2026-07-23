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
	name         string
	arrange      func(*Store)
	incoming     Route
	expectResult EventResult
	expectReason Reason
	expectIDs    []string
	expectIP     string
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
			incoming: Route{
				ID: "id", Namespace: "ns", Name: "one", UID: "uid-a", ResourceVersion: "1", IP: "updated",
			},
			expectResult: EventResultApplied, expectIDs: []string{"id"}, expectIP: "updated",
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
			name: "equal RV different UID ignored",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("old", "ns", "one", "uid-a", "1"))
			},
			incoming:     fullRoute("new", "ns", "one", "uid-b", "1"),
			expectResult: EventResultIgnored, expectReason: ReasonIdentityMismatch,
			expectIDs: []string{"old"},
		},
		{
			name:         "legacy ID-only route normalized",
			incoming:     idOnlyRoute("ns--one", "uid-a", "1"),
			expectResult: EventResultApplied, expectIDs: []string{"ns--one"},
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
			assert.Empty(t, result.RepairRequests)
			assert.Equal(t, tt.expectIDs, routeIDs(store.List()))
			if tt.expectIP != "" {
				assert.Equal(t, tt.expectIP, mustGetRoute(t, store, tt.incoming.ID).IP)
			}
		})
	}
}

func TestStoreActiveKeyIndexReferencesAuthoritativeRecord(t *testing.T) {
	store := newTestStore(t, nil, time.Second)
	key := types.NamespacedName{Namespace: "ns", Name: "one"}
	legacy := fullRoute("ns--one", key.Namespace, key.Name, "uid-a", "1")

	require.Equal(t, EventResultApplied, store.Upsert(legacy).Result)
	store.mu.RLock()
	assert.Equal(t, key, store.activeKeyByID[legacy.ID])
	assert.Equal(t, legacy, store.recordByObject[key].route)
	store.mu.RUnlock()
	assert.Equal(t, legacy, mustGetRoute(t, store, legacy.ID))

	short := fullRoute("short", key.Namespace, key.Name, "uid-a", "2")
	require.Equal(t, EventResultApplied, store.Upsert(short).Result)
	_, legacyPresent := store.Get(legacy.ID)
	assert.False(t, legacyPresent)
	assert.Equal(t, short, mustGetRoute(t, store, short.ID))
}

func TestStoreDelete(t *testing.T) {
	tests := []struct {
		name         string
		arrange      func(*Store)
		delete       func(*Store) MutationResult
		expectResult EventResult
		expectReason Reason
		expectIDs    []string
		expectFence  string
	}{
		{
			name: "equal resource version",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", "ns", "one", "uid-a", "2"))
			},
			delete: func(store *Store) MutationResult {
				return store.Delete(fullRoute("id", "ns", "one", "uid-a", "2"))
			},
			expectResult: EventResultApplied, expectFence: "2",
		},
		{
			name: "absent route",
			delete: func(store *Store) MutationResult {
				return store.Delete(fullRoute("id", "ns", "one", "uid-a", "1"))
			},
			expectResult: EventResultIgnored, expectReason: ReasonAbsent,
		},
		{
			name: "opaque ID-only route is invalid",
			delete: func(store *Store) MutationResult {
				return store.Delete(idOnlyRoute("opaque", "uid-a", "1"))
			},
			expectResult: EventResultInvalid, expectReason: ReasonInvalidRoute,
		},
		{
			name: "newer legacy ID deletes current short ID",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("short", "ns", "one", "uid-a", "2"))
			},
			delete: func(store *Store) MutationResult {
				return store.Delete(idOnlyRoute("ns--one", "uid-a", "3"))
			},
			expectResult: EventResultApplied, expectFence: "3",
		},
		{
			name: "older resource version ignored",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", "ns", "one", "uid-a", "2"))
			},
			delete: func(store *Store) MutationResult {
				return store.Delete(fullRoute("id", "ns", "one", "uid-a", "1"))
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
				return store.Delete(fullRoute("old", "ns", "one", "uid-a", "9"))
			},
			expectResult: EventResultIgnored, expectReason: ReasonIdentityMismatch,
			expectIDs: []string{"new"},
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
			if tt.expectFence != "" {
				fence, exists := store.deletionByObject[types.NamespacedName{Namespace: "ns", Name: "one"}]
				require.True(t, exists)
				assert.Equal(t, tt.expectFence, fence.resourceVersion)
			}
		})
	}
}

func TestStoreDeleteRejectsStaleObjectKeySnapshots(t *testing.T) {
	store := newTestStore(t, nil, time.Second)
	key := types.NamespacedName{Namespace: "ns", Name: "one"}

	require.Equal(t, EventResultApplied, store.Upsert(fullRoute("old", "ns", "one", "uid-a", "1")).Result)
	staleSnapshot, exists := store.GetByObjectKey(key)
	require.True(t, exists)
	require.Equal(t, EventResultApplied, store.Upsert(fullRoute("new", "ns", "one", "uid-a", "2")).Result)
	stale := store.Delete(staleSnapshot)
	assert.Equal(t, EventResultIgnored, stale.Result)
	assert.Equal(t, ReasonStaleResourceVersion, stale.Reason)

	oldIncarnation, exists := store.GetByObjectKey(key)
	require.True(t, exists)
	require.Equal(t, EventResultApplied, store.Upsert(fullRoute("replacement", "ns", "one", "uid-b", "3")).Result)
	mismatched := store.Delete(oldIncarnation)
	assert.Equal(t, EventResultIgnored, mismatched.Result)
	assert.Equal(t, ReasonIdentityMismatch, mismatched.Reason)
	assert.Equal(t, "replacement", mustGetRoute(t, store, "replacement").ID)
}

func TestStoreRepairGenerationAndDeletionFence(t *testing.T) {
	now := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	store := newTestStore(t, func() time.Time { return now }, time.Second)

	store.Upsert(fullRoute("old", "ns", "one", "uid-a", "1"))
	deleted := store.Delete(fullRoute("old", "ns", "one", "uid-a", "1"))
	require.Equal(t, EventResultApplied, deleted.Result)
	ambiguous := store.Upsert(fullRoute("old", "ns", "one", "uid-a", "1"))
	require.Len(t, ambiguous.RepairRequests, 1)
	_, oldActive := store.Get("old")
	assert.False(t, oldActive)

	store.Upsert(fullRoute("other", "ns", "other", "uid-c", "10"))
	result := store.ApplyAuthoritativeRepair(ambiguous.RepairRequests[0], AuthoritativeObservation{
		Present: true,
		Route:   fullRoute("new", "ns", "one", "uid-b", "2"),
	})
	assert.Equal(t, EventResultApplied, result.Result)
	assert.Equal(t, []string{"new", "other"}, routeIDs(store.List()))

	deleted = store.Delete(fullRoute("new", "ns", "one", "uid-b", "2"))
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
			name: "opaque ID-only observation",
			request: RepairRequest{
				ObjectKey: types.NamespacedName{Namespace: "ns", Name: "one"}, Generation: 1,
			},
			observation:  AuthoritativeObservation{Present: true, Route: idOnlyRoute("opaque", "uid", "1")},
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

func mustGetRoute(t *testing.T, store *Store, id string) Route {
	t.Helper()
	route, exists := store.Get(id)
	require.True(t, exists)
	return route
}

func storeIsEmpty(store *Store) bool {
	store.mu.RLock()
	defer store.mu.RUnlock()
	return len(store.recordByObject) == 0 && len(store.deletionByObject) == 0 &&
		len(store.activeKeyByID) == 0
}
