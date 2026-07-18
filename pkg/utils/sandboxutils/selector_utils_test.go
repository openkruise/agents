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

func TestIsSelectorOverlapping(t *testing.T) {
	tests := []struct {
		name     string
		s1       *metav1.LabelSelector
		s2       *metav1.LabelSelector
		expected bool
	}{
		{
			name:     "Both nil",
			s1:       nil,
			s2:       nil,
			expected: true,
		},
		{
			name:     "S1 nil",
			s1:       nil,
			s2:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			expected: true,
		},
		{
			name:     "Both empty",
			s1:       &metav1.LabelSelector{},
			s2:       &metav1.LabelSelector{},
			expected: true,
		},
		{
			name:     "Conflicting MatchLabels",
			s1:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			s2:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "bar"}},
			expected: false,
		},
		{
			name:     "Overlapping MatchLabels",
			s1:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo", "env": "prod"}},
			s2:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo", "tier": "web"}},
			expected: true,
		},
		{
			name:     "No common keys",
			s1:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			s2:       &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			expected: true,
		},
		{
			name: "MatchExpressions In overlap",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"foo", "bar"}},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"bar", "baz"}},
				},
			},
			expected: true,
		},
		{
			name: "MatchExpressions In disjoint",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"foo"}},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"bar"}},
				},
			},
			expected: false,
		},
		{
			name: "MatchLabel vs NotIn excluding it",
			s1:   &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "env", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"prod"}},
				},
			},
			expected: false,
		},
		{
			name: "Exists vs DoesNotExist",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "team", Operator: metav1.LabelSelectorOpExists},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "team", Operator: metav1.LabelSelectorOpDoesNotExist},
				},
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsSelectorOverlapping(tt.s1, tt.s2)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsSelectorLooseOverlap(t *testing.T) {
	tests := []struct {
		name     string
		s1       *metav1.LabelSelector
		s2       *metav1.LabelSelector
		expected bool
	}{
		{
			name:     "Same key In/In with overlapping values",
			s1:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			s2:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			expected: true,
		},
		{
			name: "Same key In/In with disjoint values",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"foo"}},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"bar"}},
				},
			},
			expected: false,
		},
		{
			name:     "Different keys do not overlap",
			s1:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			s2:       &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
			expected: false,
		},
		{
			name:     "s2 has extra key not in s1",
			s1:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			s2:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo", "env": "prod"}},
			expected: false,
		},
		{
			name: "Same key Exists/Exists",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpExists},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpExists},
				},
			},
			expected: true,
		},
		{
			name: "Same key Exists/In",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpExists},
				},
			},
			s2:       &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			expected: true,
		},
		{
			name: "Same key In/NotIn excluding all In values",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpIn, Values: []string{"foo"}},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"foo", "bar"}},
				},
			},
			expected: false,
		},
		{
			name: "Same key NotIn/NotIn always overlap",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"foo"}},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpNotIn, Values: []string{"bar"}},
				},
			},
			expected: true,
		},
		{
			name: "Same key Exists/DoesNotExist",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpExists},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpDoesNotExist},
				},
			},
			expected: false,
		},
		{
			name: "Same key DoesNotExist/DoesNotExist",
			s1: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpDoesNotExist},
				},
			},
			s2: &metav1.LabelSelector{
				MatchExpressions: []metav1.LabelSelectorRequirement{
					{Key: "app", Operator: metav1.LabelSelectorOpDoesNotExist},
				},
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := IsSelectorLooseOverlap(tt.s1, tt.s2)
			assert.Equal(t, tt.expected, result)
		})
	}
}
