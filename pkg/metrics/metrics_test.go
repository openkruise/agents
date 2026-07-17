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

package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMetricGroupsExposeOnlyBoundedSeries(t *testing.T) {
	recordAllSandboxIDMetrics()
	recordAllSandboxRouteMetrics()

	tests := []struct {
		name             string
		register         func(prometheus.Registerer)
		expectedFamilies map[string]metricExpectation
	}{
		{
			name:     "Sandbox ID group",
			register: RegisterSandboxID,
			expectedFamilies: map[string]metricExpectation{
				"sandbox_id_legacy_resolution_total": {metricType: dto.MetricType_COUNTER, labelSets: []map[string]string{{"surface": LegacyResolutionSurfaceE2B}, {"surface": LegacyResolutionSurfaceGateway}}},
				"sandbox_id_assignment_total":        {metricType: dto.MetricType_COUNTER, labelSets: []map[string]string{{"result": SandboxIDAssignmentResultSuccess}, {"result": SandboxIDAssignmentResultFailure}}},
				"sandbox_id_collision_total":         {metricType: dto.MetricType_COUNTER, labelSets: []map[string]string{{"surface": CollisionSurfaceCache}, {"surface": CollisionSurfaceManagerRoute}, {"surface": CollisionSurfaceGatewayRoute}}},
			},
		},
		{
			name:     "Sandbox route group",
			register: RegisterSandboxRoute,
			expectedFamilies: map[string]metricExpectation{
				"sandbox_route_legacy_fallback_total": {metricType: dto.MetricType_COUNTER, labelSets: routeSurfaceLabelSets()},
				"sandbox_route_invalid_total":         {metricType: dto.MetricType_COUNTER, labelSets: routeSurfaceLabelSets()},
				"sandbox_route_records":               {metricType: dto.MetricType_GAUGE, labelSets: routeRecordLabelSets()},
				"sandbox_route_repair_queue_depth":    {metricType: dto.MetricType_GAUGE, labelSets: routeSurfaceLabelSets()},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			tt.register(registry)
			families, err := registry.Gather()
			require.NoError(t, err)
			require.Len(t, families, len(tt.expectedFamilies))
			for _, family := range families {
				expected, exists := tt.expectedFamilies[family.GetName()]
				require.True(t, exists, "unexpected metric family %s", family.GetName())
				assert.Equal(t, expected.metricType, family.GetType())
				assert.ElementsMatch(t, expected.labelSets, metricLabelSets(family.Metric))
			}
		})
	}
}

func TestRecordersRejectUnsupportedLabels(t *testing.T) {
	tests := []struct {
		name       string
		register   func(prometheus.Registerer)
		record     func()
		metricName string
	}{
		{name: "legacy resolution surface", register: RegisterSandboxID, record: func() { RecordSandboxIDLegacyResolution("other") }, metricName: "sandbox_id_legacy_resolution_total"},
		{name: "assignment result", register: RegisterSandboxID, record: func() { RecordSandboxIDAssignment("other") }, metricName: "sandbox_id_assignment_total"},
		{name: "collision surface", register: RegisterSandboxID, record: func() { RecordSandboxIDCollision("other") }, metricName: "sandbox_id_collision_total"},
		{name: "legacy fallback surface", register: RegisterSandboxRoute, record: func() { RecordSandboxRouteLegacyFallback("other") }, metricName: "sandbox_route_legacy_fallback_total"},
		{name: "invalid route surface", register: RegisterSandboxRoute, record: func() { RecordSandboxRouteInvalid("other") }, metricName: "sandbox_route_invalid_total"},
		{name: "route record shape", register: RegisterSandboxRoute, record: func() { SetSandboxRouteRecords(RouteSurfaceManager, "full", 1) }, metricName: "sandbox_route_records"},
		{name: "repair queue surface", register: RegisterSandboxRoute, record: func() { SetSandboxRouteRepairQueueDepth("other", 1) }, metricName: "sandbox_route_repair_queue_depth"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			tt.register(registry)
			before := metricSeriesCount(t, registry, tt.metricName)
			tt.record()
			assert.Equal(t, before, metricSeriesCount(t, registry, tt.metricName))
		})
	}
}

type metricExpectation struct {
	metricType dto.MetricType
	labelSets  []map[string]string
}

func recordAllSandboxIDMetrics() {
	RecordSandboxIDLegacyResolution(LegacyResolutionSurfaceE2B)
	RecordSandboxIDLegacyResolution(LegacyResolutionSurfaceGateway)
	RecordSandboxIDAssignment(SandboxIDAssignmentResultSuccess)
	RecordSandboxIDAssignment(SandboxIDAssignmentResultFailure)
	RecordSandboxIDCollision(CollisionSurfaceCache)
	RecordSandboxIDCollision(CollisionSurfaceManagerRoute)
	RecordSandboxIDCollision(CollisionSurfaceGatewayRoute)
}

func recordAllSandboxRouteMetrics() {
	for _, surface := range []string{RouteSurfaceManager, RouteSurfaceGateway} {
		RecordSandboxRouteLegacyFallback(surface)
		RecordSandboxRouteInvalid(surface)
		SetSandboxRouteRecords(surface, RouteRecordShapeIDOnly, 1)
		SetSandboxRouteRecords(surface, RouteRecordShapeCollision, 1)
		SetSandboxRouteRepairQueueDepth(surface, 1)
	}
}

func routeSurfaceLabelSets() []map[string]string {
	return []map[string]string{{"surface": RouteSurfaceManager}, {"surface": RouteSurfaceGateway}}
}

func routeRecordLabelSets() []map[string]string {
	return []map[string]string{
		{"surface": RouteSurfaceManager, "shape": RouteRecordShapeIDOnly},
		{"surface": RouteSurfaceManager, "shape": RouteRecordShapeCollision},
		{"surface": RouteSurfaceGateway, "shape": RouteRecordShapeIDOnly},
		{"surface": RouteSurfaceGateway, "shape": RouteRecordShapeCollision},
	}
}

func metricLabelSets(metrics []*dto.Metric) []map[string]string {
	result := make([]map[string]string, 0, len(metrics))
	for _, metric := range metrics {
		labels := make(map[string]string, len(metric.Label))
		for _, label := range metric.Label {
			labels[label.GetName()] = label.GetValue()
		}
		result = append(result, labels)
	}
	return result
}

func metricSeriesCount(t *testing.T, registry *prometheus.Registry, name string) int {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == name {
			return len(family.Metric)
		}
	}
	return 0
}
