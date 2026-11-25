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

func TestStoreUpsertOrdersByResourceVersion(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "one"}
	tests := []struct {
		name         string
		arrange      func(*Store)
		incoming     Route
		expectResult EventResult
		expectReason Reason
		expectID     string
		expectGoneID string
	}{
		{
			name:         "initial route",
			incoming:     fullRoute("legacy", key.Namespace, key.Name, "uid-a", "1"),
			expectResult: EventResultApplied,
			expectID:     "legacy",
		},
		{
			name: "newer route replaces ID and UID",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("old", key.Namespace, key.Name, "uid-a", "1"))
			},
			incoming:     fullRoute("new", key.Namespace, key.Name, "uid-b", "2"),
			expectResult: EventResultApplied,
			expectID:     "new",
			expectGoneID: "old",
		},
		{
			name: "equal replay is stale",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("old", key.Namespace, key.Name, "uid-a", "2"))
			},
			incoming:     fullRoute("old", key.Namespace, key.Name, "uid-a", "2"),
			expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion,
			expectID:     "old",
		},
		{
			name: "older route is stale",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("new", key.Namespace, key.Name, "uid-a", "3"))
			},
			incoming:     fullRoute("old", key.Namespace, key.Name, "uid-a", "2"),
			expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion,
			expectID:     "new",
		},
		{
			name: "equal fence is stale",
			arrange: func(store *Store) {
				store.Delete(Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "4"})
			},
			incoming:     fullRoute("id", key.Namespace, key.Name, "uid-a", "4"),
			expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion,
		},
		{
			name: "newer route crosses fence",
			arrange: func(store *Store) {
				store.Delete(Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "4"})
			},
			incoming:     fullRoute("id", key.Namespace, key.Name, "uid-b", "5"),
			expectResult: EventResultApplied,
			expectID:     "id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore()
			if tt.arrange != nil {
				tt.arrange(store)
			}
			result := store.Upsert(tt.incoming)
			assert.Equal(t, tt.expectResult, result.Result)
			assert.Equal(t, tt.expectReason, result.Reason)
			if tt.expectID == "" {
				assert.Zero(t, store.Len())
			} else {
				require.Equal(t, 1, store.Len())
				assert.Equal(t, tt.expectID, mustGetRoute(t, store, tt.expectID).ID)
			}
			if tt.expectGoneID != "" {
				_, exists := store.Get(tt.expectGoneID)
				assert.False(t, exists)
			}
			assertStoreObjectInvariant(t, store, key)
		})
	}
}

func TestStoreDelete(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "one"}
	tests := []struct {
		name         string
		arrange      func(*Store)
		deletion     Route
		expectResult EventResult
		expectReason Reason
		expectRoute  bool
		expectFence  string
	}{
		{
			name: "absent authoritative delete creates fence",
			deletion: Route{
				Namespace:       key.Namespace,
				Name:            key.Name,
				ResourceVersion: "2",
			},
			expectResult: EventResultApplied,
			expectFence:  "2",
		},
		{
			name: "equal record delete",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-a", "2"))
			},
			deletion:     Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "2"},
			expectResult: EventResultApplied,
			expectFence:  "2",
		},
		{
			name: "older record delete is stale",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-a", "3"))
			},
			deletion:     Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "2"},
			expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion,
			expectRoute:  true,
		},
		{
			name: "newer record delete",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-a", "3"))
			},
			deletion:     Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "4"},
			expectResult: EventResultApplied,
			expectFence:  "4",
		},
		{
			name: "older fence delete is stale",
			arrange: func(store *Store) {
				store.Delete(Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "3"})
			},
			deletion:     Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "2"},
			expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion,
			expectFence:  "3",
		},
		{
			name: "newer delete advances fence",
			arrange: func(store *Store) {
				store.Delete(Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "2"})
			},
			deletion:     Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "3"},
			expectResult: EventResultApplied,
			expectFence:  "3",
		},
		{
			name: "empty resource version deletes current record using its resource version",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-a", "10"))
			},
			deletion:     Route{Namespace: key.Namespace, Name: key.Name},
			expectResult: EventResultApplied,
			expectFence:  "10",
		},
		{
			name: "empty resource version preserves existing fence",
			arrange: func(store *Store) {
				store.Delete(Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "12"})
			},
			deletion:     Route{Namespace: key.Namespace, Name: key.Name},
			expectResult: EventResultApplied,
			expectFence:  "12",
		},
		{
			name:         "empty resource version for unseen object is no-op",
			deletion:     Route{Namespace: key.Namespace, Name: key.Name},
			expectResult: EventResultApplied,
		},
		{
			name: "equal fence delete is idempotent",
			arrange: func(store *Store) {
				store.Delete(Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "3"})
			},
			deletion:     Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "3"},
			expectResult: EventResultApplied,
			expectFence:  "3",
		},
		{
			name:         "invalid object key",
			deletion:     Route{ResourceVersion: "3"},
			expectResult: EventResultInvalid,
			expectReason: ReasonInvalidRoute,
		},
		{
			name:         "invalid resource version",
			deletion:     Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "rv"},
			expectResult: EventResultInvalid,
			expectReason: ReasonInvalidRoute,
		},
		{
			name:         "ID-only delete is invalid",
			deletion:     Route{ID: "ns--one", ResourceVersion: "2"},
			expectResult: EventResultInvalid,
			expectReason: ReasonInvalidRoute,
		},
		{
			name:         "partial ObjectKey is invalid",
			deletion:     Route{ID: "ns--one", Namespace: key.Namespace, ResourceVersion: "2"},
			expectResult: EventResultInvalid,
			expectReason: ReasonInvalidRoute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore()
			if tt.arrange != nil {
				tt.arrange(store)
			}
			result := store.Delete(tt.deletion)
			assert.Equal(t, tt.expectResult, result.Result)
			assert.Equal(t, tt.expectReason, result.Reason)
			_, hasRoute := store.Get("id")
			assert.Equal(t, tt.expectRoute, hasRoute)
			assert.Equal(t, tt.expectFence, store.deletionByObject[key].resourceVersion)
			assertStoreObjectInvariant(t, store, key)
		})
	}
}

func TestStoreDeletionFenceRetentionAndCleanup(t *testing.T) {
	store := NewStore()
	key := types.NamespacedName{Namespace: "ns", Name: "one"}
	deletion := Route{Namespace: key.Namespace, Name: key.Name, ResourceVersion: "3"}

	earliestExpiry := time.Now().Add(deletionFenceRetention)
	require.Equal(t, EventResultApplied, store.Delete(deletion).Result)
	latestExpiry := time.Now().Add(deletionFenceRetention)
	fence := store.deletionByObject[key]
	assert.False(t, fence.expiresAt.Before(earliestExpiry))
	assert.False(t, fence.expiresAt.After(latestExpiry))

	oldExpiry := time.Now().Add(time.Hour)
	store.deletionByObject[key] = deletionFence{resourceVersion: "3", expiresAt: oldExpiry}
	assert.Equal(t, EventResultIgnored, store.Delete(Route{
		Namespace: key.Namespace, Name: key.Name, ResourceVersion: "2",
	}).Result)
	assert.Equal(t, EventResultApplied, store.Delete(Route{
		Namespace: key.Namespace, Name: key.Name,
	}).Result)
	assert.Equal(t, oldExpiry, store.deletionByObject[key].expiresAt)

	// Equal or newer fence-only deletes may advance RV but never refresh expiry.
	require.Equal(t, EventResultApplied, store.Delete(deletion).Result)
	assert.Equal(t, oldExpiry, store.deletionByObject[key].expiresAt)
	assert.Equal(t, "3", store.deletionByObject[key].resourceVersion)
	require.Equal(t, EventResultApplied, store.Delete(Route{
		Namespace: key.Namespace, Name: key.Name, ResourceVersion: "4",
	}).Result)
	assert.Equal(t, oldExpiry, store.deletionByObject[key].expiresAt)
	assert.Equal(t, "4", store.deletionByObject[key].resourceVersion)

	now := time.Unix(100, 0)
	equalKey := types.NamespacedName{Namespace: "ns", Name: "equal"}
	expiredKey := types.NamespacedName{Namespace: "ns", Name: "expired"}
	futureKey := types.NamespacedName{Namespace: "ns", Name: "future"}
	store.deletionByObject[equalKey] = deletionFence{resourceVersion: "1", expiresAt: now}
	store.deletionByObject[expiredKey] = deletionFence{resourceVersion: "2", expiresAt: now.Add(-time.Nanosecond)}
	store.deletionByObject[futureKey] = deletionFence{resourceVersion: "3", expiresAt: now.Add(time.Second)}
	store.nextDeletionFenceCleanup = now.Add(time.Second)
	store.mu.Lock()
	store.cleanupDeletionFencesLocked(now)
	store.mu.Unlock()
	assert.Contains(t, store.deletionByObject, expiredKey)

	store.nextDeletionFenceCleanup = time.Time{}
	store.mu.Lock()
	store.cleanupDeletionFencesLocked(now)
	store.mu.Unlock()
	assert.Contains(t, store.deletionByObject, equalKey)
	assert.NotContains(t, store.deletionByObject, expiredKey)
	assert.Contains(t, store.deletionByObject, futureKey)
	assert.Equal(t, now.Add(deletionFenceCleanupInterval), store.nextDeletionFenceCleanup)

	// An expired, cleaned fence no longer rejects an older observation.
	assert.Equal(t, EventResultApplied, store.Upsert(Route{
		Namespace: expiredKey.Namespace, Name: expiredKey.Name,
		ID: "id-expired", UID: "uid-expired", ResourceVersion: "1",
	}).Result)
	_, hasRoute := store.Get("id-expired")
	assert.True(t, hasRoute)
}

func TestStoreRejectsRoutesWithoutFullObjectKeyWithoutAllocatingState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Store) MutationResult
	}{
		{
			name: "ID-only upsert",
			mutate: func(store *Store) MutationResult {
				return store.Upsert(idOnlyRoute("ns--one", "uid-a", "1"))
			},
		},
		{
			name: "partial-key upsert",
			mutate: func(store *Store) MutationResult {
				return store.Upsert(Route{
					ID: "id", Namespace: "ns", UID: "uid-a", ResourceVersion: "1",
				})
			},
		},
		{
			name: "ID-only delete",
			mutate: func(store *Store) MutationResult {
				return store.Delete(Route{ID: "ns--one", ResourceVersion: "1"})
			},
		},
		{
			name: "partial-key delete",
			mutate: func(store *Store) MutationResult {
				return store.Delete(Route{ID: "id", Namespace: "ns", ResourceVersion: "1"})
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore()
			result := tt.mutate(store)
			assert.Equal(t, MutationResult{
				Result: EventResultInvalid,
				Reason: ReasonInvalidRoute,
			}, result)
			assert.Empty(t, store.recordByObject)
			assert.Empty(t, store.activeKeyByID)
			assert.Empty(t, store.deletionByObject)
		})
	}
}

func TestStoreShortIDCollisionAcrossObjects(t *testing.T) {
	keyA := types.NamespacedName{Namespace: "ns", Name: "a"}
	keyB := types.NamespacedName{Namespace: "ns", Name: "b"}
	routeA := fullRoute("shared", keyA.Namespace, keyA.Name, "uid-a", "1")
	routeB := fullRoute("shared", keyB.Namespace, keyB.Name, "uid-b", "1")

	newCollisionStore := func(t *testing.T) *Store {
		t.Helper()
		store := NewStore()
		firstResult := store.Upsert(routeA)
		require.Equal(t, EventResultApplied, firstResult.Result)
		assert.Empty(t, firstResult.Reason)
		takeoverResult := store.Upsert(routeB)
		require.Equal(t, EventResultApplied, takeoverResult.Result)
		assert.Equal(t, ReasonIDTakeover, takeoverResult.Reason)
		assert.Equal(t, routeB, mustGetRoute(t, store, "shared"))
		return store
	}

	t.Run("deleting displaced object keeps takeover active", func(t *testing.T) {
		store := newCollisionStore(t)
		deletion := Route{Namespace: keyA.Namespace, Name: keyA.Name, ResourceVersion: "2"}
		require.Equal(t, EventResultApplied, store.Delete(deletion).Result)
		assert.Equal(t, routeB, mustGetRoute(t, store, "shared"))
		assert.Equal(t, 1, store.Len())
		assertStoreObjectInvariant(t, store, keyA)
		assertStoreObjectInvariant(t, store, keyB)
	})

	t.Run("deleting takeover keeps ID inactive until displaced object advances", func(t *testing.T) {
		store := newCollisionStore(t)
		deletion := Route{Namespace: keyB.Namespace, Name: keyB.Name, ResourceVersion: "2"}
		require.Equal(t, EventResultApplied, store.Delete(deletion).Result)
		_, present := store.Get("shared")
		assert.False(t, present)
		assert.Equal(t, 0, store.Len())

		staleResult := store.Upsert(routeA)
		assert.Equal(t, MutationResult{
			Result: EventResultIgnored,
			Reason: ReasonStaleResourceVersion,
		}, staleResult)
		_, present = store.Get("shared")
		assert.False(t, present)

		updatedRouteA := routeA
		updatedRouteA.ResourceVersion = "2"
		require.Equal(t, EventResultApplied, store.Upsert(updatedRouteA).Result)
		assert.Equal(t, updatedRouteA, mustGetRoute(t, store, "shared"))
		assertStoreObjectInvariant(t, store, keyA)
		assertStoreObjectInvariant(t, store, keyB)
	})
}

func TestStoreConcurrentReadWrite(t *testing.T) {
	store := NewStore()
	const workers = 16
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for rv := 1; rv <= 100; rv++ {
				id := fmt.Sprintf("id-%d", worker)
				route := fullRoute(id, "ns", id, types.UID(id), fmt.Sprint(rv))
				assert.Equal(t, EventResultApplied, store.Upsert(route).Result)
				_, present := store.Get(id)
				assert.True(t, present)
				store.Len()
				store.Delete(route)
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 0, store.Len())
}

func mustGetRoute(t *testing.T, store *Store, id string) Route {
	t.Helper()
	route, exists := store.Get(id)
	require.True(t, exists)
	return route
}

func assertStoreObjectInvariant(t *testing.T, store *Store, key types.NamespacedName) {
	t.Helper()
	store.mu.RLock()
	defer store.mu.RUnlock()
	_, hasRecord := store.recordByObject[key]
	_, hasFence := store.deletionByObject[key]
	assert.False(t, hasRecord && hasFence, "record and fence must not coexist")
}
