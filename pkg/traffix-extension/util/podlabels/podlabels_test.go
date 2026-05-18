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

package podlabels

import (
	"encoding/base64"
	"testing"
)

func TestParseSandboxLabels(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
	}{
		{
			name:     "empty string",
			input:    "",
			expected: map[string]string{},
		},
		{
			name:     "invalid base64",
			input:    "!!!not-base64!!!",
			expected: map[string]string{},
		},
		{
			name:     "valid base64 but empty content",
			input:    base64.StdEncoding.EncodeToString([]byte("")),
			expected: map[string]string{},
		},
		{
			name:     "single label",
			input:    base64.StdEncoding.EncodeToString([]byte("app=sleep")),
			expected: map[string]string{"app": "sleep"},
		},
		{
			name:  "multiple labels",
			input: base64.StdEncoding.EncodeToString([]byte("app=sleep,networking.istio.io/tunnel=http,service.istio.io/canonical-name=sleep")),
			expected: map[string]string{
				"app":                             "sleep",
				"networking.istio.io/tunnel":      "http",
				"service.istio.io/canonical-name": "sleep",
			},
		},
		{
			name:  "full example from task",
			input: base64.StdEncoding.EncodeToString([]byte("app=sleep,networking.istio.io/tunnel=http,pod-template-hash=75d7b8ddbb,security.istio.io/tlsMode=disabled,service.istio.io/canonical-name=sleep,service.istio.io/canonical-revision=latest,topology.kubernetes.io/region=cn-hongkong,topology.kubernetes.io/zone=cn-hongkong-c")),
			expected: map[string]string{
				"app":                                 "sleep",
				"networking.istio.io/tunnel":          "http",
				"pod-template-hash":                   "75d7b8ddbb",
				"security.istio.io/tlsMode":           "disabled",
				"service.istio.io/canonical-name":     "sleep",
				"service.istio.io/canonical-revision": "latest",
				"topology.kubernetes.io/region":       "cn-hongkong",
				"topology.kubernetes.io/zone":         "cn-hongkong-c",
			},
		},
		{
			name:  "value with equals sign",
			input: base64.StdEncoding.EncodeToString([]byte("label=key=value")),
			expected: map[string]string{
				"label": "key=value",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := ParseSandboxLabels(tc.input)
			if len(result) != len(tc.expected) {
				t.Errorf("expected %d labels, got %d", len(tc.expected), len(result))
			}
			for k, v := range tc.expected {
				if result[k] != v {
					t.Errorf("label[%q]: expected %q, got %q", k, v, result[k])
				}
			}
		})
	}
}
