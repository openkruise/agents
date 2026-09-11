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

package logs

import "testing"

func TestSanitizeValue(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  string
	}{
		{
			name: "empty value",
		},
		{
			name:  "unchanged value",
			value: "normal value",
			want:  "normal value",
		},
		{
			name:  "newline",
			value: "before\nafter",
			want:  "beforeafter",
		},
		{
			name:  "carriage return",
			value: "before\rafter",
			want:  "beforeafter",
		},
		{
			name:  "carriage return newline",
			value: "before\r\nafter",
			want:  "beforeafter",
		},
		{
			name:  "multiple control characters",
			value: "a\nb\rc\r\nd",
			want:  "abcd",
		},
		{
			name:  "tab and unicode remain",
			value: "值\t保留",
			want:  "值\t保留",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SanitizeValue(tt.value); got != tt.want {
				t.Fatalf("SanitizeValue(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}
