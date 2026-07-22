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
			return store.DeleteAuthoritativeByObjectKey(types.NamespacedName{Name: "one"})
		}, expectResult: EventResultInvalid, expectDelta: 1},
		{name: "invalid conditional delete", mutate: func(store *Store) MutationResult {
			return store.DeleteConditionally(Route{})
		}, expectResult: EventResultInvalid, expectDelta: 1},
		{name: "invalid authoritative repair", mutate: func(store *Store) MutationResult {
			return store.ApplyAuthoritativeRepair(RepairRequest{}, AuthoritativeObservation{})
		}, expectResult: EventResultInvalid, expectDelta: 1},
		{name: "applied is not counted", mutate: func(store *Store) MutationResult {
			return store.Upsert(fullRoute("id", "ns", "one", "uid-a", "1"))
		}, expectResult: EventResultApplied},
		{name: "ignored is not counted", mutate: func(store *Store) MutationResult {
			store.Upsert(fullRoute("id", "ns", "one", "uid-a", "2"))
			return store.Upsert(fullRoute("id", "ns", "one", "uid-a", "1"))
		}, expectResult: EventResultIgnored},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := NewStore(StoreOptions{})
			registry := newRouteMetricRegistry()
			before := routeMetricValue(t, registry, "sandbox_route_invalid_total")

			result := tt.mutate(store)

			assert.Equal(t, tt.expectResult, result.Result)
			assert.Equal(t, before+tt.expectDelta, routeMetricValue(t, registry, "sandbox_route_invalid_total"))
		})
	}
}

func TestRouteCollisionRecordMetric(t *testing.T) {
	store := NewStore(StoreOptions{})
	registry := newRouteMetricRegistry()
	assert.Zero(t, routeMetricValue(t, registry, "sandbox_route_collision_records"))

	store.Upsert(fullRoute("same", "ns", "one", "uid-a", "1"))
	store.Upsert(fullRoute("same", "ns", "two", "uid-b", "1"))
	assert.Equal(t, float64(1), routeMetricValue(t, registry, "sandbox_route_collision_records"))
}

func newRouteMetricRegistry() *prometheus.Registry {
	registry := prometheus.NewRegistry()
	metrics.RegisterSandboxRoute(registry)
	return registry
}

func routeMetricValue(t *testing.T, registry *prometheus.Registry, name string) float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name || len(family.Metric) != 1 {
			continue
		}
		metric := family.Metric[0]
		if metric.Counter != nil {
			return metric.GetCounter().GetValue()
		}
		return metric.GetGauge().GetValue()
	}
	return 0
}
