/*
Copyright 2025.

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

package core

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// resetPodPropagatableAnnotations restores the package-level allowlist to its
// empty default so tests do not leak registrations into one another.
func resetPodPropagatableAnnotations(t *testing.T) {
	t.Helper()
	propagationMu.Lock()
	defer propagationMu.Unlock()
	podPropagatableAnnotationKeys = map[string]struct{}{}
	podPropagatableAnnotationPrefixes = nil
}

func TestShouldPropagateAnnotationToPod(t *testing.T) {
	tests := []struct {
		name             string
		registerKeys     []string
		registerPrefixes []string
		key              string
		want             bool
	}{
		{
			name: "empty allowlist - nothing propagates",
			key:  "agents.kruise.io/foo",
			want: false,
		},
		{
			name:         "exact key match",
			registerKeys: []string{"internal.company.io/scheduler-hint"},
			key:          "internal.company.io/scheduler-hint",
			want:         true,
		},
		{
			name:         "exact key miss",
			registerKeys: []string{"internal.company.io/scheduler-hint"},
			key:          "internal.company.io/other",
			want:         false,
		},
		{
			name:             "prefix match",
			registerPrefixes: []string{"internal.company.io/pod-"},
			key:              "internal.company.io/pod-network",
			want:             true,
		},
		{
			name:             "prefix miss",
			registerPrefixes: []string{"internal.company.io/pod-"},
			key:              "internal.company.io/node-network",
			want:             false,
		},
		{
			name:             "exact key takes effect alongside prefixes",
			registerKeys:     []string{"exact.key/name"},
			registerPrefixes: []string{"prefix.key/"},
			key:              "exact.key/name",
			want:             true,
		},
		{
			name:             "prefix takes effect alongside keys",
			registerKeys:     []string{"exact.key/name"},
			registerPrefixes: []string{"prefix.key/"},
			key:              "prefix.key/anything",
			want:             true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetPodPropagatableAnnotations(t)
			t.Cleanup(func() { resetPodPropagatableAnnotations(t) })

			RegisterPodPropagatableAnnotationKeys(tt.registerKeys...)
			RegisterPodPropagatableAnnotationPrefixes(tt.registerPrefixes...)

			assert.Equal(t, tt.want, shouldPropagateAnnotationToPod(tt.key))
		})
	}
}

func TestRegisterPodPropagatableAnnotations_IgnoresEmpty(t *testing.T) {
	resetPodPropagatableAnnotations(t)
	t.Cleanup(func() { resetPodPropagatableAnnotations(t) })

	RegisterPodPropagatableAnnotationKeys("", "valid.key/name")
	RegisterPodPropagatableAnnotationPrefixes("", "valid.prefix/")

	assert.False(t, shouldPropagateAnnotationToPod(""))
	assert.True(t, shouldPropagateAnnotationToPod("valid.key/name"))
	assert.True(t, shouldPropagateAnnotationToPod("valid.prefix/anything"))
}

func TestFilterPodPropagatableAnnotations(t *testing.T) {
	tests := []struct {
		name             string
		registerKeys     []string
		registerPrefixes []string
		input            map[string]string
		want             map[string]string
	}{
		{
			name:  "nil input returns nil",
			input: nil,
			want:  nil,
		},
		{
			name:  "empty input returns nil",
			input: map[string]string{},
			want:  nil,
		},
		{
			name:         "no key eligible returns nil",
			registerKeys: []string{"allowed/key"},
			input:        map[string]string{"other/key": "v"},
			want:         nil,
		},
		{
			name:             "only eligible keys are kept",
			registerKeys:     []string{"allowed/key"},
			registerPrefixes: []string{"pod-prefix/"},
			input: map[string]string{
				"allowed/key":                   "v1",
				"pod-prefix/net":                "v2",
				"e2b.agents.kruise.io/envd-url": "internal",
				"random/key":                    "v3",
			},
			want: map[string]string{
				"allowed/key":    "v1",
				"pod-prefix/net": "v2",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resetPodPropagatableAnnotations(t)
			t.Cleanup(func() { resetPodPropagatableAnnotations(t) })

			RegisterPodPropagatableAnnotationKeys(tt.registerKeys...)
			RegisterPodPropagatableAnnotationPrefixes(tt.registerPrefixes...)

			assert.Equal(t, tt.want, filterPodPropagatableAnnotations(tt.input))
		})
	}
}
