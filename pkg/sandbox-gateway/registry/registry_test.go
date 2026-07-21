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

package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/sandboxroute"
)

func TestNewRegistry(t *testing.T) {
	store := sandboxroute.NewStore(sandboxroute.StoreOptions{})

	tests := []struct {
		name        string
		store       *sandboxroute.Store
		expectError string
	}{
		{name: "Store accepted", store: store},
		{name: "nil Store rejected", expectError: "must not be nil"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewRegistry(tt.store)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Same(t, tt.store, got.Store())
		})
	}
}

func TestRegistryMutationAdapters(t *testing.T) {
	tests := []struct {
		name            string
		mutate          func(*Registry) (sandboxroute.MutationResult, error)
		expectResult    sandboxroute.EventResult
		expectID        string
		expectPresent   bool
		expectRepairs   int
		expectCallbacks int
	}{
		{
			name: "full route is active",
			mutate: func(registry *Registry) (sandboxroute.MutationResult, error) {
				return registry.Upsert(fullRoute("short-a", "ns", "a", "uid-a", "1"))
			},
			expectResult:    sandboxroute.EventResultApplied,
			expectID:        "short-a",
			expectPresent:   true,
			expectCallbacks: 1,
		},
		{
			name: "ID-only route is active",
			mutate: func(registry *Registry) (sandboxroute.MutationResult, error) {
				return registry.Upsert(idOnlyRoute("ns--a", "uid-a", "1"))
			},
			expectResult:    sandboxroute.EventResultApplied,
			expectID:        "ns--a",
			expectPresent:   true,
			expectCallbacks: 1,
		},
		{
			name: "stale full route is ignored",
			mutate: func(registry *Registry) (sandboxroute.MutationResult, error) {
				_, _ = registry.Upsert(fullRoute("short-a", "ns", "a", "uid-a", "2"))
				return registry.Upsert(fullRoute("short-a", "ns", "a", "uid-a", "1"))
			},
			expectResult:    sandboxroute.EventResultIgnored,
			expectID:        "short-a",
			expectPresent:   true,
			expectCallbacks: 2,
		},
		{
			name: "authoritative ObjectKey delete removes full route",
			mutate: func(registry *Registry) (sandboxroute.MutationResult, error) {
				_, _ = registry.Upsert(fullRoute("short-a", "ns", "a", "uid-a", "1"))
				return registry.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "a"}, "ns--a")
			},
			expectResult:    sandboxroute.EventResultApplied,
			expectID:        "short-a",
			expectCallbacks: 2,
		},
		{
			name: "authoritative fallback removes only ID-only route",
			mutate: func(registry *Registry) (sandboxroute.MutationResult, error) {
				_, _ = registry.Upsert(idOnlyRoute("ns--a", "uid-a", "1"))
				return registry.DeleteAuthoritativeByObjectKey(types.NamespacedName{Namespace: "ns", Name: "a"}, "ns--a")
			},
			expectResult:    sandboxroute.EventResultApplied,
			expectID:        "ns--a",
			expectCallbacks: 2,
		},
		{
			name: "cross ObjectKey collision is quarantined and enqueued",
			mutate: func(registry *Registry) (sandboxroute.MutationResult, error) {
				_, _ = registry.Upsert(fullRoute("duplicate", "ns", "a", "uid-a", "1"))
				return registry.Upsert(fullRoute("duplicate", "ns", "b", "uid-b", "2"))
			},
			expectResult:    sandboxroute.EventResultCollision,
			expectID:        "duplicate",
			expectRepairs:   2,
			expectCallbacks: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := sandboxroute.NewStore(sandboxroute.StoreOptions{})
			registry, err := NewRegistry(store)
			require.NoError(t, err)
			var enqueued []sandboxroute.RepairRequest
			var callbackResults []sandboxroute.MutationResult
			registry.SetRepairEnqueuer(func(result sandboxroute.MutationResult) {
				callbackResults = append(callbackResults, result)
				enqueued = append(enqueued, result.RepairRequests...)
			})

			result, err := tt.mutate(registry)
			require.NoError(t, err)
			assert.Equal(t, tt.expectResult, result.Result)
			_, present := registry.Get(tt.expectID)
			assert.Equal(t, tt.expectPresent, present)
			assert.Len(t, enqueued, tt.expectRepairs)
			assert.Len(t, callbackResults, tt.expectCallbacks)
			assert.Equal(t, result, callbackResults[len(callbackResults)-1])
		})
	}
}

func TestRegistryListAndClear(t *testing.T) {
	tests := []struct {
		name        string
		clear       bool
		expectCount int
	}{
		{name: "list active routes", expectCount: 2},
		{name: "clear replaces test Store", clear: true, expectCount: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := sandboxroute.NewStore(sandboxroute.StoreOptions{})
			registry, err := NewRegistry(store)
			require.NoError(t, err)
			registry.SetRepairEnqueuer(func(sandboxroute.MutationResult) {})
			_, err = registry.Upsert(idOnlyRoute("a", "uid-a", "1"))
			require.NoError(t, err)
			_, err = registry.Upsert(idOnlyRoute("b", "uid-b", "1"))
			require.NoError(t, err)
			if tt.clear {
				registry.Clear()
			}
			assert.Len(t, registry.List(), tt.expectCount)
		})
	}
}

func TestRegistryLifecycleReadiness(t *testing.T) {
	tests := []struct {
		name          string
		activate      bool
		teardown      bool
		expectReady   bool
		expectError   string
		expectPresent bool
	}{
		{
			name:        "startup before repair handoff rejects mutation",
			expectError: ErrNotReady.Error(),
		},
		{
			name:          "active repair handoff accepts mutation and read",
			activate:      true,
			expectReady:   true,
			expectPresent: true,
		},
		{
			name:        "teardown rejects later mutation and read",
			activate:    true,
			teardown:    true,
			expectError: ErrNotReady.Error(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := sandboxroute.NewStore(sandboxroute.StoreOptions{})
			registry, err := NewRegistry(store)
			require.NoError(t, err)
			if tt.activate {
				registry.SetRepairEnqueuer(func(sandboxroute.MutationResult) {})
			}
			if tt.teardown {
				registry.SetRepairEnqueuer(nil)
			}

			_, err = registry.Upsert(idOnlyRoute("opaque-id", "uid-a", "1"))
			if tt.expectError != "" {
				require.Error(t, err)
				assert.ErrorIs(t, err, ErrNotReady)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}
			_, present, ready := registry.GetIfReady("opaque-id")
			assert.Equal(t, tt.expectReady, ready)
			assert.Equal(t, tt.expectPresent, present)
			assert.Equal(t, tt.expectReady, registry.Ready())
			_, stored := registry.Get("opaque-id")
			assert.Equal(t, tt.expectPresent, stored)
		})
	}
}

func TestRegistryHandoffPreservesAppliedRepairRequests(t *testing.T) {
	tests := []struct {
		name string
	}{
		{name: "applied mutation forwards displaced claimant repair"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := sandboxroute.NewStore(sandboxroute.StoreOptions{})
			registry, err := NewRegistry(store)
			require.NoError(t, err)
			var handedOff []sandboxroute.MutationResult
			registry.SetRepairEnqueuer(func(result sandboxroute.MutationResult) {
				handedOff = append(handedOff, result)
			})

			first := fullRoute("same", "ns", "one", "uid-a", "1")
			second := fullRoute("same", "ns", "two", "uid-a", "2")
			_, err = registry.Upsert(first)
			require.NoError(t, err)
			collision, err := registry.Upsert(second)
			require.NoError(t, err)
			require.Len(t, collision.RepairRequests, 2)
			confirmed := store.ApplyAuthoritativeRepair(
				collision.RepairRequests[0],
				sandboxroute.AuthoritativeObservation{Present: true, Route: first},
			)
			require.Equal(t, sandboxroute.EventResultCollision, confirmed.Result)
			handedOff = nil

			applied, err := registry.Upsert(fullRoute("new", "ns", "two", "uid-b", "3"))

			require.NoError(t, err)
			require.Equal(t, sandboxroute.EventResultApplied, applied.Result)
			require.Len(t, applied.RepairRequests, 1)
			require.Len(t, handedOff, 1)
			assert.Equal(t, applied, handedOff[0])
			assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "one"}, handedOff[0].RepairRequests[0].ObjectKey)
		})
	}
}

func fullRoute(id, namespace, name, uid, resourceVersion string) sandboxroute.Route {
	return sandboxroute.Route{
		ID:              id,
		Namespace:       namespace,
		Name:            name,
		UID:             types.UID(uid),
		ResourceVersion: resourceVersion,
	}
}

func idOnlyRoute(id, uid, resourceVersion string) sandboxroute.Route {
	return sandboxroute.Route{ID: id, UID: types.UID(uid), ResourceVersion: resourceVersion}
}
