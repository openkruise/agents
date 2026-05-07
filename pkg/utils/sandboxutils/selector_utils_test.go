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

package sandboxutils

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSelectorsOverlap(t *testing.T) {
	tests := []struct {
		name     string
		s1       *metav1.LabelSelector
		s2       *metav1.LabelSelector
		overlap bool
	}{
		{
			name: "identical exact matches",
			s1: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			s2: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			overlap: true,
		},
		{
			name: "conflicting exact matches",
			s1: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			s2: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "other"},
			},
			overlap: false,
		},
		{
			name: "one empty, one specific",
			s1: &metav1.LabelSelector{},
			s2: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			overlap: true, // Empty matches everything
		},
		{
			name: "different keys, potentially overlapping",
			s1: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": "test"},
			},
			s2: &metav1.LabelSelector{
				MatchLabels: map[string]string{"env": "prod"},
			},
			overlap: true, // A sandbox could have both labels
		},
		{
			name: "nil selectors",
			s1:      nil,
			s2:      nil,
			overlap: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SelectorsOverlap(tt.s1, tt.s2)
			assert.Equal(t, tt.overlap, got)
		})
	}
}
