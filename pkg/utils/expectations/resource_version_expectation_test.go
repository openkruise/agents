/*
Copyright 2025 The Kruise Authors.

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

package expectations

import (
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestResourceVersionExpectation(t *testing.T) {
	cases := []struct {
		expect      *v1.Pod
		observe     *v1.Pod
		isSatisfied *v1.Pod
		result      bool
	}{
		{
			expect:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", UID: "00"}},
			observe:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1", UID: "00"}},
			isSatisfied: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1", UID: "00"}},
			result:      false,
		},
		{
			expect:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", UID: "01"}},
			observe:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", UID: "01"}},
			isSatisfied: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", UID: "01"}},
			result:      true,
		},
		{
			expect:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", UID: "02"}},
			observe:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1", UID: "02"}},
			isSatisfied: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", UID: "02"}},
			result:      true,
		},
		{
			expect:      &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", UID: "03"}},
			observe:     &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "2", UID: "03"}},
			isSatisfied: &v1.Pod{ObjectMeta: metav1.ObjectMeta{ResourceVersion: "3", UID: "03"}},
			result:      true,
		},
	}

	for i, testCase := range cases {
		c := NewResourceVersionExpectation()
		c.Expect(testCase.expect)
		c.Observe(testCase.observe)
		got, _ := c.IsSatisfied(testCase.isSatisfied)
		if got != testCase.result {
			t.Fatalf("#%d expected %v, got %v", i, testCase.result, got)
		}
	}
}

func TestResourceVersionPlusOne(t *testing.T) {
	tests := []struct {
		name            string
		resourceVersion string
		expected        string
	}{
		{
			name:            "empty string returns 1",
			resourceVersion: "",
			expected:        "1",
		},
		{
			name:            "normal version increment",
			resourceVersion: "123",
			expected:        "124",
		},
		{
			name:            "zero version",
			resourceVersion: "0",
			expected:        "1",
		},
		{
			name:            "single digit",
			resourceVersion: "5",
			expected:        "6",
		},
		{
			name:            "large version number",
			resourceVersion: "999999",
			expected:        "1000000",
		},
		{
			name:            "max uint64 minus one",
			resourceVersion: "18446744073709551614",
			expected:        "18446744073709551615",
		},
		{
			name:            "invalid format returns original",
			resourceVersion: "abc",
			expected:        "abc",
		},
		{
			name:            "mixed alphanumeric returns original",
			resourceVersion: "123abc",
			expected:        "123abc",
		},
		{
			name:            "negative number returns original",
			resourceVersion: "-1",
			expected:        "-1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &v1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: tt.resourceVersion,
					UID:             "test-uid",
				},
			}
			result := GetNewerResourceVersion(pod)
			if result != tt.expected {
				t.Errorf("GetNewerResourceVersion(pod with version %q) = %q, want %q", tt.resourceVersion, result, tt.expected)
			}
		})
	}
}
