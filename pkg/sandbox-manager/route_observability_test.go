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

package sandbox_manager

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/metrics"
	"github.com/openkruise/agents/pkg/sandbox-manager/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

func TestManagerRouteProjectorObservability(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expectID string
	}{
		{name: "legacy projection is not a delete fallback", expectID: "team-a--sandbox-a"},
		{name: "short projection is not a delete fallback", labels: map[string]string{sandboxid.LabelKey: "short-id"}, expectID: "short-id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a",
				Name:      "sandbox-a",
				UID:       "uid-a",
				Labels:    tt.labels,
			}}
			fallbackLabels := map[string]string{"surface": string(sandboxroute.SurfaceManager)}
			fallbackBefore := registeredCounterValue(t, "sandbox_route_legacy_fallback_total", fallbackLabels)

			route, err := newManagerRouteProjector().Project(sandboxroute.ProjectionInput{Object: object})
			require.NoError(t, err)
			assert.Equal(t, tt.expectID, route.ID)
			assert.Equal(t, fallbackBefore, registeredCounterValue(t, "sandbox_route_legacy_fallback_total", fallbackLabels))
		})
	}
}

func TestResolveSandboxIDRecordsOnlyLegacyResolution(t *testing.T) {
	tests := []struct {
		name        string
		labels      map[string]string
		expectID    string
		expectDelta float64
	}{
		{name: "legacy resolution", expectID: "team-a--sandbox-a", expectDelta: 1},
		{name: "short resolution", labels: map[string]string{sandboxid.LabelKey: "short-id"}, expectID: "short-id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := prometheus.NewRegistry()
			metrics.RegisterSandboxID(registry)
			before := sandboxIDMetricCounterValue(t, registry, "sandbox_id_legacy_resolution_total", "surface", metrics.LegacyResolutionSurfaceE2B)
			object := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sandbox-a", Labels: tt.labels}}

			id := (&SandboxManager{}).ResolveSandboxID(object)

			assert.Equal(t, tt.expectID, id)
			assert.Equal(t, before+tt.expectDelta, sandboxIDMetricCounterValue(t, registry, "sandbox_id_legacy_resolution_total", "surface", metrics.LegacyResolutionSurfaceE2B))
		})
	}
}

func registeredCounterValue(t *testing.T, name string, expectedLabels map[string]string) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics.RegisterSandboxRoute(registry)
	families, err := registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if metricLabelsMatch(metric, expectedLabels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func metricLabelsMatch(metric *dto.Metric, expected map[string]string) bool {
	if len(metric.Label) != len(expected) {
		return false
	}
	for _, label := range metric.Label {
		if expected[label.GetName()] != label.GetValue() {
			return false
		}
	}
	return true
}
