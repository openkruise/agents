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

package id

import "testing"

func TestValid(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{name: "openkruise", value: string(OpenKruise), want: true},
		{name: "single character", value: "a", want: true},
		{name: "digits and separators", value: "Infra_2-alpha", want: true},
		{name: "empty", value: "", want: false},
		{name: "leading separator", value: "-infra", want: false},
		{name: "leading underscore", value: "_infra", want: false},
		{name: "space", value: "open kruise", want: false},
		{name: "slash", value: "open/kruise", want: false},
		{name: "dot", value: "open.kruise", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Valid(tt.value); got != tt.want {
				t.Fatalf("Valid(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}

func TestOpenKruiseIsValid(t *testing.T) {
	if !Valid(string(OpenKruise)) {
		t.Fatalf("OpenKruise identifier %q must be valid", OpenKruise)
	}
}
