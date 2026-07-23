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
				store.Delete(Delete{ObjectKey: key, ResourceVersion: "4"})
			},
			incoming:     fullRoute("id", key.Namespace, key.Name, "uid-a", "4"),
			expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion,
		},
		{
			name: "newer route crosses fence",
			arrange: func(store *Store) {
				store.Delete(Delete{ObjectKey: key, ResourceVersion: "4"})
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
				assert.Empty(t, store.List())
			} else {
				require.Len(t, store.List(), 1)
				assert.Equal(t, tt.expectID, store.List()[0].ID)
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
		deletion     Delete
		expectResult EventResult
		expectReason Reason
		expectRoute  bool
		expectFence  string
	}{
		{
			name: "absent authoritative delete creates fence",
			deletion: Delete{
				ObjectKey:       key,
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
			deletion:     Delete{ObjectKey: key, ResourceVersion: "2"},
			expectResult: EventResultApplied,
			expectFence:  "2",
		},
		{
			name: "older record delete is stale",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-a", "3"))
			},
			deletion:     Delete{ObjectKey: key, ResourceVersion: "2"},
			expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion,
			expectRoute:  true,
		},
		{
			name: "newer delete advances fence",
			arrange: func(store *Store) {
				store.Delete(Delete{ObjectKey: key, ResourceVersion: "2"})
			},
			deletion:     Delete{ObjectKey: key, ResourceVersion: "3"},
			expectResult: EventResultApplied,
			expectFence:  "3",
		},
		{
			name: "equal fence delete is idempotent",
			arrange: func(store *Store) {
				store.Delete(Delete{ObjectKey: key, ResourceVersion: "3"})
			},
			deletion:     Delete{ObjectKey: key, ResourceVersion: "3"},
			expectResult: EventResultApplied,
			expectFence:  "3",
		},
		{
			name:         "invalid object key",
			deletion:     Delete{ResourceVersion: "3"},
			expectResult: EventResultInvalid,
			expectReason: ReasonInvalidObjectKey,
		},
		{
			name:         "invalid resource version",
			deletion:     Delete{ObjectKey: key, ResourceVersion: "rv"},
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
			assert.Equal(t, tt.expectFence, store.deletionByObject[key])
			assertStoreObjectInvariant(t, store, key)
		})
	}
}

func TestStoreDeleteIfTracked(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "one"}

	t.Run("unseen object does not create fence", func(t *testing.T) {
		store := NewStore()
		result := store.DeleteIfTracked(Delete{ObjectKey: key, ResourceVersion: "2"})
		assert.Equal(t, EventResultApplied, result.Result)
		assert.Empty(t, store.deletionByObject)
	})

	t.Run("record becomes fence", func(t *testing.T) {
		store := NewStore()
		store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-a", "1"))
		result := store.DeleteIfTracked(Delete{ObjectKey: key, ResourceVersion: "2"})
		assert.Equal(t, EventResultApplied, result.Result)
		assert.Equal(t, "2", store.deletionByObject[key])
		assertStoreObjectInvariant(t, store, key)
	})

	t.Run("existing fence advances without adding key", func(t *testing.T) {
		store := NewStore()
		store.Delete(Delete{ObjectKey: key, ResourceVersion: "1"})
		result := store.DeleteIfTracked(Delete{ObjectKey: key, ResourceVersion: "2"})
		assert.Equal(t, EventResultApplied, result.Result)
		assert.Equal(t, "2", store.deletionByObject[key])
		assert.Len(t, store.deletionByObject, 1)
	})

	t.Run("empty resource version is rejected", func(t *testing.T) {
		store := NewStore()
		result := store.DeleteIfTracked(Delete{ObjectKey: key})
		assert.Equal(t, EventResultInvalid, result.Result)
		assert.Equal(t, ReasonInvalidRoute, result.Reason)
	})
}

func TestStoreEmptyResourceVersionDelete(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "one"}

	t.Run("record resource version becomes fence", func(t *testing.T) {
		store := NewStore()
		store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-a", "10"))
		result := store.Delete(Delete{ObjectKey: key})
		assert.Equal(t, EventResultApplied, result.Result)
		assert.Equal(t, "10", store.deletionByObject[key])
		assert.Equal(t, EventResultIgnored, store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-a", "10")).Result)
		assert.Equal(t, EventResultApplied, store.Upsert(fullRoute("id", key.Namespace, key.Name, "uid-b", "11")).Result)
		assertStoreObjectInvariant(t, store, key)
	})

	t.Run("existing fence is preserved", func(t *testing.T) {
		store := NewStore()
		store.Delete(Delete{ObjectKey: key, ResourceVersion: "12"})
		result := store.Delete(Delete{ObjectKey: key})
		assert.Equal(t, EventResultApplied, result.Result)
		assert.Equal(t, "12", store.deletionByObject[key])
	})

	t.Run("unseen object remains untracked", func(t *testing.T) {
		store := NewStore()
		result := store.Delete(Delete{ObjectKey: key})
		assert.Equal(t, EventResultApplied, result.Result)
		assert.Empty(t, store.deletionByObject)
	})
}

func TestStoreActiveKeyIndexReferencesAuthoritativeRecord(t *testing.T) {
	store := NewStore()
	key := types.NamespacedName{Namespace: "ns", Name: "one"}
	legacy := fullRoute("ns--one", key.Namespace, key.Name, "uid-a", "1")

	require.Equal(t, EventResultApplied, store.Upsert(legacy).Result)
	assert.Equal(t, key, store.activeKeyByID[legacy.ID])
	assert.Equal(t, legacy, store.recordByObject[key])

	short := fullRoute("short", key.Namespace, key.Name, "uid-a", "2")
	require.Equal(t, EventResultApplied, store.Upsert(short).Result)
	_, legacyPresent := store.Get(legacy.ID)
	assert.False(t, legacyPresent)
	assert.Equal(t, short, mustGetRoute(t, store, short.ID))
	assertStoreObjectInvariant(t, store, key)
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
				store.Upsert(fullRoute(id, "ns", id, types.UID(id), fmt.Sprint(rv)))
				store.Get(id)
				store.List()
			}
		}()
	}
	wg.Wait()
	assert.Len(t, store.List(), workers)
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
