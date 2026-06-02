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

package proxyutils

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestGetRouteFromSandbox_WakeOnTraffic(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expectWake  string
	}{
		{
			name:       "no annotation",
			expectWake: "",
		},
		{
			name:        "timeout seconds",
			annotations: map[string]string{agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:300"},
			expectWake:  "timeout:300",
		},
		{
			name:        "timeout never",
			annotations: map[string]string{agentsv1alpha1.AnnotationWakeOnTraffic: "timeout:never"},
			expectWake:  "timeout:never",
		},
		{
			name:        "malformed copied verbatim",
			annotations: map[string]string{agentsv1alpha1.AnnotationWakeOnTraffic: "garbage"},
			expectWake:  "garbage",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "sandbox",
					Namespace:   "default",
					Annotations: tt.annotations,
				},
				Spec: agentsv1alpha1.SandboxSpec{
					EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
						Template: &corev1.PodTemplateSpec{},
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
					Phase:   agentsv1alpha1.SandboxRunning,
				},
			}

			route := GetRouteFromSandbox(sbx)

			assert.Equal(t, tt.expectWake, route.WakeOnTraffic)
		})
	}
}

func TestRoute_JSONCompatibility(t *testing.T) {
	tests := []struct {
		name       string
		payload    string
		route      Route
		expectWake string
	}{
		{
			name:       "old peer payload omits wake field",
			payload:    `{"ip":"10.0.0.1","id":"default--sandbox","uid":"abc","owner":"u","state":"running","resourceVersion":"42"}`,
			expectWake: "",
		},
		{
			name: "new peer payload carries wake field",
			route: Route{
				IP:              "10.0.0.1",
				ID:              "default--sandbox",
				Owner:           "u",
				State:           "paused",
				ResourceVersion: "43",
				WakeOnTraffic:   "timeout:300",
			},
			expectWake: "timeout:300",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := []byte(tt.payload)
			if len(payload) == 0 {
				var err error
				payload, err = json.Marshal(tt.route)
				require.NoError(t, err)
			}

			var got Route
			require.NoError(t, json.Unmarshal(payload, &got))
			assert.Equal(t, tt.expectWake, got.WakeOnTraffic)
		})
	}
}
