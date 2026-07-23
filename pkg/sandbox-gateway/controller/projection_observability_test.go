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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

// TestRouteProjectionObservability verifies gateway ID resolution in route projection.
func TestRouteProjectionObservability(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expectID string
	}{
		{
			name:     "legacy resolution",
			expectID: "ns--sandbox",
		},
		{
			name:     "short resolution",
			labels:   map[string]string{sandboxid.LabelKey: "short-id"},
			expectID: "short-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := testSandbox("ns", "sandbox", "uid-a", "1", "")
			object.Labels = tt.labels

			route, err := sandboxroute.ProjectRoute(newGatewayProjectionSource(object))
			require.NoError(t, err)
			assert.Equal(t, tt.expectID, route.ID)
			assert.Equal(t, agentsv1alpha1.SandboxStateRunning, route.State)
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
