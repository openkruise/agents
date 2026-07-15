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

package controller

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/openkruise/agents/pkg/sandboxroute"
)

func TestRouteProjectorObservability(t *testing.T) {
	tests := []struct {
		name             string
		resolver         FormattedResolver
		format           string
		expectID         string
		expectResolution float64
		expectError      string
	}{
		{
			name:             "legacy resolution records gateway without delete fallback",
			resolver:         func(metav1.Object) (string, string) { return "ns--sandbox", "legacy" },
			format:           "legacy",
			expectID:         "ns--sandbox",
			expectResolution: 1,
		},
		{
			name:             "short resolution records gateway only",
			resolver:         func(metav1.Object) (string, string) { return "short-id", "short" },
			format:           "short",
			expectID:         "short-id",
			expectResolution: 1,
		},
		{name: "nil resolver remains a projection error", expectError: "resolver is nil"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{
				Namespace:       "ns",
				Name:            "sandbox",
				UID:             "uid-a",
				ResourceVersion: "1",
			}}
			fallbackLabels := map[string]string{"surface": string(sandboxroute.SurfaceGateway)}
			resolutionLabels := map[string]string{"format": tt.format, "surface": "gateway"}
			fallbackBefore := gatewayCounterValue(t, "sandbox_route_legacy_fallback_total", fallbackLabels)
			resolutionBefore := gatewayCounterValue(t, "sandbox_id_resolved_total", resolutionLabels)

			route, err := NewRouteProjector(tt.resolver).Project(sandboxroute.ProjectionInput{Object: object})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expectID, route.ID)
			}
			assert.Equal(t, fallbackBefore, gatewayCounterValue(t, "sandbox_route_legacy_fallback_total", fallbackLabels))
			assert.Equal(t, resolutionBefore+tt.expectResolution, gatewayCounterValue(t, "sandbox_id_resolved_total", resolutionLabels))
		})
	}
}

func gatewayCounterValue(t *testing.T, name string, expectedLabels map[string]string) float64 {
	t.Helper()
	families, err := metrics.Registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if gatewayMetricLabelsMatch(metric, expectedLabels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func gatewayMetricLabelsMatch(metric *dto.Metric, expected map[string]string) bool {
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
