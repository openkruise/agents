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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/pkg/metrics"
)

func TestLegacyDeleteFallbackMetric(t *testing.T) {
	tests := []struct {
		name        string
		arrange     func(*Store)
		fallbackID  string
		expectDelta float64
	}{
		{
			name: "successful compatibility delete",
			arrange: func(store *Store) {
				store.Upsert(idOnlyRoute("legacy", "uid-a", "1"))
			},
			fallbackID:  "legacy",
			expectDelta: 1,
		},
		{
			name: "full authoritative delete is not fallback",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("legacy", "ns", "one", "uid-a", "1"))
			},
			fallbackID: "legacy",
		},
		{name: "absent compatibility record is not fallback", fallbackID: "legacy"},
		{
			name: "compatibility collision is not successful fallback",
			arrange: func(store *Store) {
				store.Upsert(idOnlyRoute("legacy", "uid-a", "1"))
				store.Upsert(idOnlyRoute("legacy", "uid-b", "2"))
			},
			fallbackID: "legacy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore(StoreOptions{})
			if tt.arrange != nil {
				tt.arrange(store)
			}
			registry := newRouteMetricRegistry()
			before := routeCounterValue(t, registry, "sandbox_route_legacy_fallback_total")

			store.DeleteAuthoritativeByObjectKey(
				types.NamespacedName{Namespace: "ns", Name: "one"},
				tt.fallbackID,
			)

			assert.Equal(t, before+tt.expectDelta, routeCounterValue(t, registry, "sandbox_route_legacy_fallback_total"))
		})
	}
}

func TestRouteInvalidMetricOnlyCountsInvalidMutations(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*Store) MutationResult
		expectResult EventResult
		expectDelta  float64
	}{
		{name: "recorded invalid", mutate: func(store *Store) MutationResult { return store.RecordInvalid() }, expectResult: EventResultInvalid, expectDelta: 1},
		{name: "invalid upsert", mutate: func(store *Store) MutationResult { return store.Upsert(Route{}) }, expectResult: EventResultInvalid, expectDelta: 1},
		{name: "invalid authoritative delete", mutate: func(store *Store) MutationResult {
			return store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Name: "one"}, "")
		}, expectResult: EventResultInvalid, expectDelta: 1},
		{name: "invalid conditional delete", mutate: func(store *Store) MutationResult {
			return store.DeleteConditionally(Route{})
		}, expectResult: EventResultInvalid, expectDelta: 1},
		{name: "invalid authoritative repair", mutate: func(store *Store) MutationResult {
			return store.ApplyAuthoritativeRepair(RepairRequest{}, AuthoritativeObservation{})
		}, expectResult: EventResultInvalid, expectDelta: 1},
		{name: "applied is not counted", mutate: func(store *Store) MutationResult { return store.Upsert(idOnlyRoute("legacy", "uid-a", "1")) }, expectResult: EventResultApplied},
		{name: "ignored is not counted", mutate: func(store *Store) MutationResult {
			store.Upsert(idOnlyRoute("legacy", "uid-a", "2"))
			return store.Upsert(idOnlyRoute("legacy", "uid-a", "1"))
		}, expectResult: EventResultIgnored},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore(StoreOptions{})
			registry := newRouteMetricRegistry()
			before := routeCounterValue(t, registry, "sandbox_route_invalid_total")

			result := tt.mutate(store)

			assert.Equal(t, tt.expectResult, result.Result)
			assert.Equal(t, before+tt.expectDelta, routeCounterValue(t, registry, "sandbox_route_invalid_total"))
		})
	}
}

func TestRouteRecordMetricsTrackIDOnlyAndCollisionState(t *testing.T) {
	tests := []struct {
		name            string
		arrange         func(*Store)
		expectIDOnly    float64
		expectCollision float64
	}{
		{name: "empty Store"},
		{
			name: "full record is not exposed",
			arrange: func(store *Store) {
				store.Upsert(fullRoute("short", "ns", "one", "uid-a", "1"))
			},
		},
		{
			name: "ID-only record",
			arrange: func(store *Store) {
				store.Upsert(idOnlyRoute("legacy", "uid-a", "1"))
			},
			expectIDOnly: 1,
		},
		{
			name: "ID collision",
			arrange: func(store *Store) {
				store.Upsert(idOnlyRoute("legacy", "uid-a", "1"))
				store.Upsert(idOnlyRoute("legacy", "uid-b", "2"))
			},
			expectIDOnly:    2,
			expectCollision: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore(StoreOptions{})
			if tt.arrange != nil {
				tt.arrange(store)
			}
			registry := newRouteMetricRegistry()

			assert.Equal(t, tt.expectIDOnly, routeRecordGaugeValue(t, registry, metrics.RouteRecordShapeIDOnly))
			assert.Equal(t, tt.expectCollision, routeRecordGaugeValue(t, registry, metrics.RouteRecordShapeCollision))
		})
	}
}

func newRouteMetricRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	metrics.RegisterSandboxRoute(registry)
	return registry
}

func routeCounterValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if len(metric.Label) == 0 {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func routeRecordGaugeValue(t *testing.T, registry *prometheus.Registry, shape string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != "sandbox_route_records" {
			continue
		}
		for _, metric := range family.Metric {
			labels := make(map[string]string, len(metric.Label))
			for _, label := range metric.Label {
				labels[label.GetName()] = label.GetValue()
			}
			if len(labels) == 1 && labels["shape"] == shape {
				return metric.GetGauge().GetValue()
			}
		}
	}
	return 0
}
