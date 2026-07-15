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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestNewStore(t *testing.T) {
	tests := []struct {
		name        string
		surface     Surface
		expectError string
	}{
		{name: "manager", surface: SurfaceManager},
		{name: "gateway", surface: SurfaceGateway},
		{name: "unsupported", surface: "other", expectError: "unsupported route Store surface"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(tt.surface)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.surface, store.Surface())
			assert.Equal(t, StoreStats{}, store.Stats())
		})
	}
}

func TestStoreFullTransitions(t *testing.T) {
	tests := []struct {
		name           string
		arrange        func(*Store)
		incoming       Route
		expectResult   EventResult
		expectReason   Reason
		expectIDs      []string
		expectRequests int
		expectStats    StoreStats
	}{
		{
			name: "initial full route", incoming: fullRoute("legacy", "ns", "one", "uid-a", "1"),
			expectResult: EventResultApplied, expectIDs: []string{"legacy"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name:     "same UID switches ID only at newer RV",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("legacy", "ns", "one", "uid-a", "1")) },
			incoming: fullRoute("short", "ns", "one", "uid-a", "2"), expectResult: EventResultApplied,
			expectIDs: []string{"short"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name:     "same UID equal RV cannot switch ID",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("legacy", "ns", "one", "uid-a", "1")) },
			incoming: fullRoute("short", "ns", "one", "uid-a", "1"), expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion, expectIDs: []string{"legacy"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name:     "same identity older event ignored",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("id", "ns", "one", "uid-a", "2")) },
			incoming: fullRoute("id", "ns", "one", "uid-a", "1"), expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion, expectIDs: []string{"id"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name:     "new UID newer RV replaces incarnation",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "1")) },
			incoming: fullRoute("new", "ns", "one", "uid-b", "2"), expectResult: EventResultApplied,
			expectIDs: []string{"new"}, expectStats: StoreStats{Full: 1, Retired: 1, Active: 1},
		},
		{
			name:     "new UID older RV ignored",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "2")) },
			incoming: fullRoute("new", "ns", "one", "uid-b", "1"), expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion, expectIDs: []string{"old"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name:     "new UID equal RV requires repair and quarantines current",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "1")) },
			incoming: fullRoute("new", "ns", "one", "uid-b", "1"), expectResult: EventResultRepairRequired,
			expectReason: ReasonAmbiguousResourceVersion, expectRequests: 1,
			expectStats: StoreStats{Full: 1, Collision: 1},
		},
		{
			name:     "new UID unorderable RV requires repair",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "old")) },
			incoming: fullRoute("new", "ns", "one", "uid-b", "new"), expectResult: EventResultRepairRequired,
			expectReason: ReasonAmbiguousResourceVersion, expectRequests: 1,
			expectStats: StoreStats{Full: 1, Collision: 1},
		},
		{
			name:     "ID-only legacy adopts same UID full short",
			arrange:  func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1")) },
			incoming: fullRoute("short", "ns", "one", "uid-a", "1"), expectResult: EventResultApplied,
			expectIDs: []string{"short"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name:     "strictly newer full supersedes different compatibility UID",
			arrange:  func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1")) },
			incoming: fullRoute("legacy", "ns", "one", "uid-b", "2"), expectResult: EventResultApplied,
			expectIDs: []string{"legacy"}, expectStats: StoreStats{Full: 1, Retired: 1, Active: 1},
		},
		{
			name:     "equal full cannot supersede compatibility UID",
			arrange:  func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1")) },
			incoming: fullRoute("legacy", "ns", "one", "uid-b", "1"), expectResult: EventResultCollision,
			expectReason: ReasonIDCollision, expectRequests: 1,
			expectStats: StoreStats{Full: 1, IDOnly: 1, Collision: 1},
		},
		{
			name:     "cross ObjectKey duplicate ID quarantines both",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("same", "ns", "one", "uid-a", "1")) },
			incoming: fullRoute("same", "ns", "two", "uid-b", "1"), expectResult: EventResultCollision,
			expectReason: ReasonIDCollision, expectRequests: 2,
			expectStats: StoreStats{Full: 2, Collision: 1},
		},
		{
			name:     "same UID and ID across ObjectKeys preserves and quarantines both claimants",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("one", "ns", "one", "uid-a", "1")) },
			incoming: fullRoute("one", "ns", "two", "uid-a", "2"), expectResult: EventResultCollision,
			expectReason: ReasonUIDCollision, expectRequests: 2,
			expectStats: StoreStats{Full: 2, Collision: 1},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, nil, time.Second)
			if tt.arrange != nil {
				tt.arrange(store)
			}
			result := store.UpsertFull(tt.incoming)
			assert.Equal(t, tt.expectResult, result.Result)
			assert.Equal(t, tt.expectReason, result.Reason)
			assert.Len(t, result.RepairRequests, tt.expectRequests)
			assert.Equal(t, tt.expectIDs, routeIDs(store.List()))
			assert.Equal(t, tt.expectStats, store.Stats())
		})
	}
}

func TestStoreIDOnlyTransitions(t *testing.T) {
	tests := []struct {
		name         string
		arrange      func(*Store)
		incoming     Route
		expectResult EventResult
		expectReason Reason
		expectIDs    []string
		expectStats  StoreStats
	}{
		{name: "initial compatibility", incoming: idOnlyRoute("legacy", "uid-a", "1"), expectResult: EventResultApplied, expectIDs: []string{"legacy"}, expectStats: StoreStats{IDOnly: 1, Active: 1}},
		{
			name:     "same compatibility newer update",
			arrange:  func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1")) },
			incoming: idOnlyRoute("legacy", "uid-a", "2"), expectResult: EventResultApplied,
			expectIDs: []string{"legacy"}, expectStats: StoreStats{IDOnly: 1, Active: 1},
		},
		{
			name:     "same compatibility older ignored",
			arrange:  func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "2")) },
			incoming: idOnlyRoute("legacy", "uid-a", "1"), expectResult: EventResultIgnored,
			expectReason: ReasonStaleResourceVersion, expectIDs: []string{"legacy"}, expectStats: StoreStats{IDOnly: 1, Active: 1},
		},
		{
			name:     "same UID different ID collides without alias",
			arrange:  func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1")) },
			incoming: idOnlyRoute("other", "uid-a", "2"), expectResult: EventResultCollision,
			expectReason: ReasonUIDCollision, expectIDs: []string{"legacy"}, expectStats: StoreStats{IDOnly: 1, Active: 1},
		},
		{
			name:     "different UID same ID fails closed",
			arrange:  func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1")) },
			incoming: idOnlyRoute("legacy", "uid-b", "2"), expectResult: EventResultCollision,
			expectReason: ReasonIDCollision, expectStats: StoreStats{IDOnly: 2, Collision: 1},
		},
		{
			name:     "same UID full ownership dominates",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("short", "ns", "one", "uid-a", "1")) },
			incoming: idOnlyRoute("legacy", "uid-a", "9"), expectResult: EventResultIgnored,
			expectReason: ReasonDominatedByFull, expectIDs: []string{"short"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name:     "target full ownership dominates different UID",
			arrange:  func(store *Store) { store.UpsertFull(fullRoute("short", "ns", "one", "uid-a", "1")) },
			incoming: idOnlyRoute("short", "uid-b", "9"), expectResult: EventResultIgnored,
			expectReason: ReasonDominatedByFull, expectIDs: []string{"short"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name: "retired UID cannot revive",
			arrange: func(store *Store) {
				store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "1"))
				store.UpsertFull(fullRoute("new", "ns", "one", "uid-b", "2"))
			},
			incoming: idOnlyRoute("old", "uid-a", "9"), expectResult: EventResultIgnored,
			expectReason: ReasonRetiredUID, expectIDs: []string{"new"}, expectStats: StoreStats{Full: 1, Retired: 1, Active: 1},
		},
		{
			name: "deletion fence ID rejects a different compatibility UID",
			arrange: func(store *Store) {
				store.UpsertFull(fullRoute("deleted", "ns", "one", "uid-a", "1"))
				store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "one"}, "")
			},
			incoming: idOnlyRoute("deleted", "uid-b", "99"), expectResult: EventResultIgnored,
			expectReason: ReasonDeletionFence, expectStats: StoreStats{Retired: 1, Deletion: 1},
		},
		{
			name:     "wrong shape invalid",
			incoming: fullRoute("id", "ns", "one", "uid-a", "1"), expectResult: EventResultInvalid,
			expectReason: ReasonInvalidRoute, expectStats: StoreStats{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, nil, time.Second)
			if tt.arrange != nil {
				tt.arrange(store)
			}
			result := store.UpsertIDOnly(tt.incoming)
			assert.Equal(t, tt.expectResult, result.Result)
			assert.Equal(t, tt.expectReason, result.Reason)
			assert.Equal(t, tt.expectIDs, routeIDs(store.List()))
			assert.Equal(t, tt.expectStats, store.Stats())
		})
	}
}

func TestStoreDeleteModes(t *testing.T) {
	tests := []struct {
		name         string
		arrange      func(*Store)
		delete       func(*Store) MutationResult
		expectResult EventResult
		expectReason Reason
		expectIDs    []string
		expectStats  StoreStats
	}{
		{
			name:    "authoritative ObjectKey delete",
			arrange: func(store *Store) { store.UpsertFull(fullRoute("id", "ns", "one", "uid-a", "1")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "one"}, "")
			},
			expectResult: EventResultApplied, expectStats: StoreStats{Retired: 1, Deletion: 1},
		},
		{
			name:    "authoritative fallback removes ID-only only",
			arrange: func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "one"}, "legacy")
			},
			expectResult: EventResultApplied, expectStats: StoreStats{Retired: 1, Deletion: 1},
		},
		{
			name: "authoritative full delete fence suppresses an existing ID-only claimant",
			arrange: func(store *Store) {
				store.UpsertIDOnly(idOnlyRoute("same", "uid-b", "1"))
				store.UpsertFull(fullRoute("same", "ns", "one", "uid-a", "1"))
			},
			delete: func(store *Store) MutationResult {
				return store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "one"}, "")
			},
			expectResult: EventResultApplied,
			expectStats:  StoreStats{IDOnly: 1, Retired: 1, Deletion: 1},
		},
		{
			name:    "authoritative fallback never deletes another full object",
			arrange: func(store *Store) { store.UpsertFull(fullRoute("legacy", "ns", "other", "uid-a", "1")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "one"}, "legacy")
			},
			expectResult: EventResultIgnored, expectReason: ReasonAbsent, expectIDs: []string{"legacy"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name: "invalid authoritative ObjectKey",
			delete: func(store *Store) MutationResult {
				return store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Name: "one"}, "")
			},
			expectResult: EventResultInvalid, expectReason: ReasonInvalidObjectKey, expectStats: StoreStats{},
		},
		{
			name:    "full conditional equal delete",
			arrange: func(store *Store) { store.UpsertFull(fullRoute("id", "ns", "one", "uid-a", "2")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteFullConditionally(fullRoute("id", "ns", "one", "uid-a", "2"))
			},
			expectResult: EventResultApplied, expectStats: StoreStats{Retired: 1, Deletion: 1},
		},
		{
			name:    "full conditional older ignored",
			arrange: func(store *Store) { store.UpsertFull(fullRoute("id", "ns", "one", "uid-a", "2")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteFullConditionally(fullRoute("id", "ns", "one", "uid-a", "1"))
			},
			expectResult: EventResultIgnored, expectReason: ReasonStaleResourceVersion, expectIDs: []string{"id"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name: "full conditional old UID cannot delete replacement",
			arrange: func(store *Store) {
				store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "1"))
				store.UpsertFull(fullRoute("new", "ns", "one", "uid-b", "2"))
			},
			delete: func(store *Store) MutationResult {
				return store.DeleteFullConditionally(fullRoute("old", "ns", "one", "uid-a", "9"))
			},
			expectResult: EventResultIgnored, expectReason: ReasonIdentityMismatch, expectIDs: []string{"new"}, expectStats: StoreStats{Full: 1, Retired: 1, Active: 1},
		},
		{
			name:    "full conditional may remove exact compatibility",
			arrange: func(store *Store) { store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteFullConditionally(fullRoute("legacy", "ns", "one", "uid-a", "2"))
			},
			expectResult: EventResultApplied, expectStats: StoreStats{Retired: 1, Deletion: 1},
		},
		{
			name:    "ID-only delete cannot delete full",
			arrange: func(store *Store) { store.UpsertFull(fullRoute("id", "ns", "one", "uid-a", "1")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteIDOnlyConditionally(idOnlyRoute("id", "uid-a", "9"))
			},
			expectResult: EventResultIgnored, expectReason: ReasonDominatedByFull, expectIDs: []string{"id"}, expectStats: StoreStats{Full: 1, Active: 1},
		},
		{
			name:    "ID-only exact delete",
			arrange: func(store *Store) { store.UpsertIDOnly(idOnlyRoute("id", "uid-a", "1")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteIDOnlyConditionally(idOnlyRoute("id", "uid-a", "2"))
			},
			expectResult: EventResultApplied, expectStats: StoreStats{Retired: 1},
		},
		{
			name:    "ID-only mismatched delete",
			arrange: func(store *Store) { store.UpsertIDOnly(idOnlyRoute("id", "uid-a", "1")) },
			delete: func(store *Store) MutationResult {
				return store.DeleteIDOnlyConditionally(idOnlyRoute("other", "uid-a", "2"))
			},
			expectResult: EventResultIgnored, expectReason: ReasonIdentityMismatch, expectIDs: []string{"id"}, expectStats: StoreStats{IDOnly: 1, Active: 1},
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
			assert.Equal(t, tt.expectStats, store.Stats())
		})
	}
}

func TestStoreRepairAndMaintenance(t *testing.T) {
	baseTime := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		run  func(*testing.T, *Store, *time.Time)
	}{
		{
			name: "authoritative present resolves ambiguous replacement",
			run: func(t *testing.T, store *Store, _ *time.Time) {
				store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "1"))
				ambiguous := store.UpsertFull(fullRoute("new", "ns", "one", "uid-b", "1"))
				require.Len(t, ambiguous.RepairRequests, 1)
				result := store.ApplyAuthoritativeRepair(ambiguous.RepairRequests[0], AuthoritativeObservation{
					Present: true, Route: fullRoute("new", "ns", "one", "uid-b", "1"),
				})
				assert.Equal(t, EventResultApplied, result.Result)
				assert.Equal(t, []string{"new"}, routeIDs(store.List()))
				assert.Equal(t, StoreStats{Full: 1, Retired: 1, Active: 1}, store.Stats())
			},
		},
		{
			name: "unrelated Store activity does not stale repair",
			run: func(t *testing.T, store *Store, _ *time.Time) {
				store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "1"))
				ambiguous := store.UpsertFull(fullRoute("new", "ns", "one", "uid-b", "1"))
				store.UpsertFull(fullRoute("other", "ns", "other", "uid-c", "10"))
				result := store.ApplyAuthoritativeRepair(ambiguous.RepairRequests[0], AuthoritativeObservation{
					Present: true, Route: fullRoute("new", "ns", "one", "uid-b", "1"),
				})
				assert.Equal(t, EventResultApplied, result.Result)
				assert.Equal(t, []string{"new", "other"}, routeIDs(store.List()))
			},
		},
		{
			name: "same ObjectKey activity stales repair",
			run: func(t *testing.T, store *Store, _ *time.Time) {
				store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "1"))
				ambiguous := store.UpsertFull(fullRoute("new", "ns", "one", "uid-b", "1"))
				store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "2"))
				result := store.ApplyAuthoritativeRepair(ambiguous.RepairRequests[0], AuthoritativeObservation{
					Present: true, Route: fullRoute("new", "ns", "one", "uid-b", "1"),
				})
				assert.Equal(t, EventResultIgnored, result.Result)
				assert.Equal(t, ReasonStaleRepairGeneration, result.Reason)
			},
		},
		{
			name: "cross ObjectKey collision resolves after one claimant absent",
			run: func(t *testing.T, store *Store, _ *time.Time) {
				store.UpsertFull(fullRoute("same", "ns", "one", "uid-a", "1"))
				collision := store.UpsertFull(fullRoute("same", "ns", "two", "uid-b", "1"))
				require.Len(t, collision.RepairRequests, 2)
				result := store.ApplyAuthoritativeRepair(collision.RepairRequests[0], AuthoritativeObservation{})
				assert.Equal(t, EventResultApplied, result.Result)
				assert.Equal(t, []string{"same"}, routeIDs(store.List()))
			},
		},
		{
			name: "normal delete needs one post-drain confirmation",
			run: func(t *testing.T, store *Store, now *time.Time) {
				store.UpsertFull(fullRoute("id", "ns", "one", "uid-a", "1"))
				store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "one"}, "")
				assert.Empty(t, store.Maintenance())
				*now = now.Add(time.Second)
				requests := store.Maintenance()
				require.Len(t, requests, 1)
				assert.Empty(t, store.Maintenance())
				result := store.ApplyAuthoritativeRepair(requests[0], AuthoritativeObservation{})
				assert.Equal(t, EventResultApplied, result.Result)
				assert.Equal(t, StoreStats{}, store.Stats())
			},
		},
		{
			name: "live repair crosses deletion fence without RV ordering",
			run: func(t *testing.T, store *Store, now *time.Time) {
				store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "rv-a"))
				store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "one"}, "")
				*now = now.Add(time.Second)
				requests := store.Maintenance()
				require.Len(t, requests, 1)
				result := store.ApplyAuthoritativeRepair(requests[0], AuthoritativeObservation{
					Present: true, Route: fullRoute("new", "ns", "one", "uid-b", "rv-b"),
				})
				assert.Equal(t, EventResultApplied, result.Result)
				assert.Equal(t, []string{"new"}, routeIDs(store.List()))
			},
		},
		{
			name: "compatibility expiry keeps a remaining full claimant quarantined until repair",
			run: func(t *testing.T, store *Store, now *time.Time) {
				full := fullRoute("same", "ns", "one", "uid-b", "1")
				store.UpsertIDOnly(idOnlyRoute("same", "uid-a", "1"))
				collision := store.UpsertFull(full)
				require.Equal(t, EventResultCollision, collision.Result)
				*now = now.Add(time.Second)
				requests := store.Maintenance()
				require.Len(t, requests, 1)
				assert.Equal(t, StoreStats{Full: 1, Collision: 1}, store.Stats())
				_, routable := store.Get("same")
				assert.False(t, routable)
				result := store.ApplyAuthoritativeRepair(requests[0], AuthoritativeObservation{Present: true, Route: full})
				assert.Equal(t, EventResultApplied, result.Result)
				assert.Equal(t, []string{"same"}, routeIDs(store.List()))
			},
		},
		{
			name: "compatibility records expire without collision activation",
			run: func(t *testing.T, store *Store, now *time.Time) {
				store.UpsertIDOnly(idOnlyRoute("same", "uid-a", "1"))
				store.UpsertIDOnly(idOnlyRoute("same", "uid-b", "1"))
				*now = now.Add(time.Second)
				assert.Empty(t, store.Maintenance())
				assert.Equal(t, StoreStats{}, store.Stats())
			},
		},
		{
			name: "retired UID expires only without deletion fence",
			run: func(t *testing.T, store *Store, now *time.Time) {
				store.UpsertIDOnly(idOnlyRoute("id", "uid-a", "1"))
				store.DeleteIDOnlyConditionally(idOnlyRoute("id", "uid-a", "1"))
				*now = now.Add(time.Second)
				store.Maintenance()
				assert.Equal(t, StoreStats{}, store.Stats())
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := baseTime
			store := newTestStore(t, func() time.Time { return now }, time.Second)
			tt.run(t, store, &now)
		})
	}
}

func TestStoreSameUIDClaimantRepair(t *testing.T) {
	tests := []struct {
		name string
		run  func(*testing.T, *Store)
	}{
		{
			name: "present then absent observations recheck the remaining claimant before activation",
			run: func(t *testing.T, store *Store) {
				first := fullRoute("same", "ns", "one", "uid-a", "1")
				second := fullRoute("same", "ns", "two", "uid-a", "2")
				store.UpsertFull(first)
				collision := store.UpsertFull(second)
				require.Len(t, collision.RepairRequests, 2)
				assert.Empty(t, store.List())

				confirmed := store.ApplyAuthoritativeRepair(
					collision.RepairRequests[0],
					AuthoritativeObservation{Present: true, Route: first},
				)
				assert.Equal(t, EventResultCollision, confirmed.Result)
				assert.Empty(t, confirmed.RepairRequests)

				absent := store.ApplyAuthoritativeRepair(collision.RepairRequests[1], AuthoritativeObservation{})
				assert.Equal(t, EventResultApplied, absent.Result)
				require.Len(t, absent.RepairRequests, 1)
				assert.Empty(t, store.List())

				rechecked := store.ApplyAuthoritativeRepair(
					absent.RepairRequests[0],
					AuthoritativeObservation{Present: true, Route: first},
				)
				assert.Equal(t, EventResultApplied, rechecked.Result)
				assert.Equal(t, []string{"same"}, routeIDs(store.List()))
			},
		},
		{
			name: "authoritative UID change preserves a newly discovered duplicate claimant",
			run: func(t *testing.T, store *Store) {
				store.UpsertFull(fullRoute("same", "ns", "one", "uid-a", "1"))
				store.UpsertFull(fullRoute("other", "ns", "two", "uid-b", "1"))
				ambiguous := store.UpsertFull(fullRoute("other", "ns", "two", "uid-c", "1"))
				require.Len(t, ambiguous.RepairRequests, 1)

				result := store.ApplyAuthoritativeRepair(
					ambiguous.RepairRequests[0],
					AuthoritativeObservation{Present: true, Route: fullRoute("same", "ns", "two", "uid-a", "2")},
				)
				assert.Equal(t, EventResultCollision, result.Result)
				assert.Equal(t, ReasonUIDCollision, result.Reason)
				assert.Len(t, result.RepairRequests, 2)
				assert.Empty(t, store.List())
				assert.Equal(t, StoreStats{Full: 2, Retired: 1, Collision: 1}, store.Stats())
			},
		},
		{
			name: "normal UID and ID change requeues the old remaining claimant",
			run: func(t *testing.T, store *Store) {
				first := fullRoute("same", "ns", "one", "uid-a", "1")
				second := fullRoute("same", "ns", "two", "uid-a", "2")
				store.UpsertFull(first)
				collision := store.UpsertFull(second)
				require.Len(t, collision.RepairRequests, 2)
				confirmed := store.ApplyAuthoritativeRepair(
					collision.RepairRequests[0],
					AuthoritativeObservation{Present: true, Route: first},
				)
				assert.Equal(t, EventResultCollision, confirmed.Result)

				changed := store.UpsertFull(fullRoute("new", "ns", "two", "uid-b", "3"))
				assert.Equal(t, EventResultApplied, changed.Result)
				require.Len(t, changed.RepairRequests, 1)
				assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "one"}, changed.RepairRequests[0].ObjectKey)
				assert.Equal(t, []string{"new"}, routeIDs(store.List()))

				rechecked := store.ApplyAuthoritativeRepair(
					changed.RepairRequests[0],
					AuthoritativeObservation{Present: true, Route: first},
				)
				assert.Equal(t, EventResultApplied, rechecked.Result)
				assert.Equal(t, []string{"new", "same"}, routeIDs(store.List()))
				assert.Equal(t, StoreStats{Full: 2, Active: 2}, store.Stats())
			},
		},
		{
			name: "authoritative UID and ID change requeues the old remaining claimant",
			run: func(t *testing.T, store *Store) {
				first := fullRoute("same", "ns", "one", "uid-a", "1")
				second := fullRoute("same", "ns", "two", "uid-a", "2")
				store.UpsertFull(first)
				collision := store.UpsertFull(second)
				require.Len(t, collision.RepairRequests, 2)
				confirmed := store.ApplyAuthoritativeRepair(
					collision.RepairRequests[0],
					AuthoritativeObservation{Present: true, Route: first},
				)
				assert.Equal(t, EventResultCollision, confirmed.Result)

				changed := store.ApplyAuthoritativeRepair(
					collision.RepairRequests[1],
					AuthoritativeObservation{Present: true, Route: fullRoute("new", "ns", "two", "uid-b", "3")},
				)
				assert.Equal(t, EventResultApplied, changed.Result)
				require.Len(t, changed.RepairRequests, 1)
				assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "one"}, changed.RepairRequests[0].ObjectKey)
				assert.Equal(t, []string{"new"}, routeIDs(store.List()))

				rechecked := store.ApplyAuthoritativeRepair(
					changed.RepairRequests[0],
					AuthoritativeObservation{Present: true, Route: first},
				)
				assert.Equal(t, EventResultApplied, rechecked.Result)
				assert.Equal(t, []string{"new", "same"}, routeIDs(store.List()))
				assert.Equal(t, StoreStats{Full: 2, Active: 2}, store.Stats())
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.run(t, newTestStore(t, nil, time.Second))
		})
	}
}

func TestApplyAuthoritativeRepairValidation(t *testing.T) {
	tests := []struct {
		name         string
		request      RepairRequest
		observation  AuthoritativeObservation
		expectReason Reason
	}{
		{name: "empty request", expectReason: ReasonInvalidObjectKey},
		{name: "ID-only observation", request: RepairRequest{ObjectKey: types.NamespacedName{Namespace: "ns", Name: "one"}, Generation: 1}, observation: AuthoritativeObservation{Present: true, Route: idOnlyRoute("id", "uid", "1")}, expectReason: ReasonInvalidRoute},
		{name: "mismatched key", request: RepairRequest{ObjectKey: types.NamespacedName{Namespace: "ns", Name: "one"}, Generation: 1}, observation: AuthoritativeObservation{Present: true, Route: fullRoute("id", "ns", "two", "uid", "1")}, expectReason: ReasonIdentityMismatch},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newTestStore(t, nil, time.Second)
			result := store.ApplyAuthoritativeRepair(tt.request, tt.observation)
			assert.Equal(t, EventResultInvalid, result.Result)
			assert.Equal(t, tt.expectReason, result.Reason)
		})
	}
}

func TestStoreCollisionRecorder(t *testing.T) {
	tests := []struct {
		name        string
		run         func(*testing.T, *Store)
		expectCalls int
	}{
		{
			name: "event collision",
			run: func(t *testing.T, store *Store) {
				store.UpsertFull(fullRoute("same", "ns", "one", "uid-a", "1"))
				result := store.UpsertFull(fullRoute("same", "ns", "two", "uid-b", "1"))
				assert.Equal(t, EventResultCollision, result.Result)
			},
			expectCalls: 1,
		},
		{
			name: "repair collision preserves counting boundary",
			run: func(t *testing.T, store *Store) {
				first := fullRoute("same", "ns", "one", "uid-a", "1")
				second := fullRoute("same", "ns", "two", "uid-a", "2")
				store.UpsertFull(first)
				collision := store.UpsertFull(second)
				require.Len(t, collision.RepairRequests, 2)
				result := store.ApplyAuthoritativeRepair(
					collision.RepairRequests[0],
					AuthoritativeObservation{Present: true, Route: first},
				)
				assert.Equal(t, EventResultCollision, result.Result)
			},
			expectCalls: 2,
		},
		{
			name: "repair required is not collision result",
			run: func(t *testing.T, store *Store) {
				store.UpsertFull(fullRoute("old", "ns", "one", "uid-a", "1"))
				result := store.UpsertFull(fullRoute("new", "ns", "one", "uid-b", "1"))
				assert.Equal(t, EventResultRepairRequired, result.Result)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			store, err := NewStoreWithOptions(SurfaceManager, StoreOptions{
				CollisionRecorder: func() { calls++ },
			})
			require.NoError(t, err)

			tt.run(t, store)

			assert.Equal(t, tt.expectCalls, calls)
		})
	}
}

func newTestStore(t *testing.T, now func() time.Time, drainWindow time.Duration) *Store {
	t.Helper()
	store, err := NewStoreWithOptions(SurfaceManager, StoreOptions{Now: now, DrainWindow: drainWindow})
	require.NoError(t, err)
	return store
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
