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

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/metrics"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

// TestRouteProjectionObservability keeps ID resolution metrics separate from route fallback metrics.
func TestRouteProjectionObservability(t *testing.T) {
	tests := []struct {
		name             string
		labels           map[string]string
		expectID         string
		expectResolution float64
	}{
		{
			name:             "legacy resolution records gateway without delete fallback",
			expectID:         "ns--sandbox",
			expectResolution: 1,
		},
		{
			name:     "short resolution does not increment legacy metric",
			labels:   map[string]string{sandboxid.LabelKey: "short-id"},
			expectID: "short-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := testSandbox("ns", "sandbox", "uid-a", "1", "")
			object.Labels = tt.labels
			fallbackLabels := map[string]string{}
			resolutionLabels := map[string]string{"surface": metrics.LegacyResolutionSurfaceGateway}
			fallbackBefore := gatewayCounterValue(t, "sandbox_route_legacy_fallback_total", fallbackLabels)
			resolutionBefore := gatewayCounterValue(t, "sandbox_id_legacy_resolution_total", resolutionLabels)

			route, err := sandboxroute.ProjectRoute(newGatewayProjectionSource(object))
			require.NoError(t, err)
			assert.Equal(t, tt.expectID, route.ID)
			assert.Equal(t, agentsv1alpha1.SandboxStateRunning, route.State)
			assert.Equal(t, fallbackBefore, gatewayCounterValue(t, "sandbox_route_legacy_fallback_total", fallbackLabels))
			assert.Equal(t, resolutionBefore+tt.expectResolution, gatewayCounterValue(t, "sandbox_id_legacy_resolution_total", resolutionLabels))
		})
	}
}

func TestGatewayProjectionAccessTokenCompatibility(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expectToken string
	}{
		{
			name: "runtime token",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-token",
			},
			expectToken: "runtime-token",
		},
		{
			name: "legacy envd fallback",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationEnvdAccessToken: "legacy-token",
			},
			expectToken: "legacy-token",
		},
		{
			name: "runtime token wins over legacy envd token",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-token",
				agentsv1alpha1.AnnotationEnvdAccessToken:    "legacy-token",
			},
			expectToken: "runtime-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := testSandbox("ns", "sandbox", "uid-a", "1", "")
			object.Annotations = tt.annotations

			route, err := sandboxroute.ProjectRoute(newGatewayProjectionSource(object))

			require.NoError(t, err)
			assert.Equal(t, tt.expectToken, route.AccessToken)
		})
	}
}

func gatewayCounterValue(t *testing.T, name string, expectedLabels map[string]string) float64 {
	t.Helper()
	registry := prometheus.NewRegistry()
	metrics.RegisterSandboxID(registry)
	metrics.RegisterSandboxRoute(registry)
	families, err := registry.Gather()
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
