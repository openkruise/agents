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
				"sandbox_route_legacy_fallback_total": {metricType: dto.MetricType_COUNTER, labelSets: []map[string]string{{}}},
				"sandbox_route_invalid_total":         {metricType: dto.MetricType_COUNTER, labelSets: []map[string]string{{}}},
				"sandbox_route_records":               {metricType: dto.MetricType_GAUGE, labelSets: routeRecordLabelSets()},
				"sandbox_route_repair_queue_depth":    {metricType: dto.MetricType_GAUGE, labelSets: []map[string]string{{}}},
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

type metricExpectation struct {
	metricType dto.MetricType
	labelSets  []map[string]string
}

func recordAllSandboxIDMetrics() {
	RecordSandboxIDLegacyResolutionE2B()
	RecordSandboxIDLegacyResolutionGateway()
	RecordSandboxIDAssignment(true)
	RecordSandboxIDAssignment(false)
	RecordSandboxIDCollisionCache()
	RecordSandboxIDCollisionManagerRoute()
	RecordSandboxIDCollisionGatewayRoute()
}

func recordAllSandboxRouteMetrics() {
	RecordSandboxRouteLegacyFallback()
	RecordSandboxRouteInvalid()
	SetSandboxRouteRecords(1, 1)
	SetSandboxRouteRepairQueueDepth(1)
}

func routeRecordLabelSets() []map[string]string {
	return []map[string]string{
		{"shape": RouteRecordShapeIDOnly},
		{"shape": RouteRecordShapeCollision},
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
