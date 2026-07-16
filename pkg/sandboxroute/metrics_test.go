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

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestLegacyDeleteFallbackMetric(t *testing.T) {
	tests := []struct {
		name        string
		surface     Surface
		arrange     func(*Store)
		fallbackID  string
		expectDelta float64
	}{
		{
			name:    "manager successful compatibility delete",
			surface: SurfaceManager,
			arrange: func(store *Store) {
				store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1"))
			},
			fallbackID:  "legacy",
			expectDelta: 1,
		},
		{
			name:    "gateway successful compatibility delete",
			surface: SurfaceGateway,
			arrange: func(store *Store) {
				store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1"))
			},
			fallbackID:  "legacy",
			expectDelta: 1,
		},
		{
			name:    "full authoritative delete is not fallback",
			surface: SurfaceManager,
			arrange: func(store *Store) {
				store.UpsertFull(fullRoute("legacy", "ns", "one", "uid-a", "1"))
			},
			fallbackID: "legacy",
		},
		{
			name:       "absent compatibility record is not fallback",
			surface:    SurfaceManager,
			fallbackID: "legacy",
		},
		{
			name:    "compatibility collision is not successful fallback",
			surface: SurfaceManager,
			arrange: func(store *Store) {
				store.UpsertIDOnly(idOnlyRoute("legacy", "uid-a", "1"))
				store.UpsertIDOnly(idOnlyRoute("legacy", "uid-b", "2"))
			},
			fallbackID: "legacy",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(tt.surface)
			require.NoError(t, err)
			if tt.arrange != nil {
				tt.arrange(store)
			}
			metric := routeLegacyFallbackTotal.WithLabelValues(string(tt.surface))
			before := testutil.ToFloat64(metric)

			store.DeleteAuthoritativeByObjectKey(
				types.NamespacedName{Namespace: "ns", Name: "one"},
				tt.fallbackID,
			)

			assert.Equal(t, before+tt.expectDelta, testutil.ToFloat64(metric))
		})
	}
}

func TestRecordInvalidMetric(t *testing.T) {
	tests := []struct {
		name      string
		surface   Surface
		operation Operation
		route     Route
		shape     Shape
	}{
		{name: "manager ID-only upsert", surface: SurfaceManager, operation: OperationUpsert, route: Route{}, shape: ShapeIDOnly},
		{name: "manager full upsert", surface: SurfaceManager, operation: OperationUpsert, route: Route{Namespace: "ns"}, shape: ShapeFull},
		{name: "manager ID-only delete", surface: SurfaceManager, operation: OperationDelete, route: Route{}, shape: ShapeIDOnly},
		{name: "manager full delete", surface: SurfaceManager, operation: OperationDelete, route: Route{Name: "sandbox"}, shape: ShapeFull},
		{name: "gateway ID-only upsert", surface: SurfaceGateway, operation: OperationUpsert, route: Route{}, shape: ShapeIDOnly},
		{name: "gateway full upsert", surface: SurfaceGateway, operation: OperationUpsert, route: Route{Namespace: "ns"}, shape: ShapeFull},
		{name: "gateway ID-only delete", surface: SurfaceGateway, operation: OperationDelete, route: Route{}, shape: ShapeIDOnly},
		{name: "gateway full delete", surface: SurfaceGateway, operation: OperationDelete, route: Route{Name: "sandbox"}, shape: ShapeFull},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store, err := NewStore(tt.surface)
			require.NoError(t, err)
			metric := routeEventTotal.WithLabelValues(
				string(tt.surface),
				string(tt.shape),
				string(tt.operation),
				string(EventResultInvalid),
			)
			before := testutil.ToFloat64(metric)

			result := store.RecordInvalid(tt.operation, tt.route)

			assert.Equal(t, EventResultInvalid, result.Result)
			assert.Equal(t, ReasonInvalidRoute, result.Reason)
			assert.Empty(t, result.RepairRequests)
			assert.Equal(t, before+1, testutil.ToFloat64(metric))
			assert.Empty(t, store.List())
			assert.Equal(t, StoreStats{}, store.Stats())
		})
	}
}
