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

package resourceaccess

import "testing"

func TestPolicy(t *testing.T) {
	for _, tt := range []struct {
		name        string
		annotations map[string]string
		want        Mode
		wantError   bool
	}{
		{name: "unmarked legacy", want: TrustedDomain},
		{name: "unrelated annotation", annotations: map[string]string{"app": "one"}, want: TrustedDomain},
		{name: "accepted compatible", annotations: map[string]string{AnnotationPolicy: "trusted-domain-v1"}, want: TrustedDomain},
		{name: "accepted isolated", annotations: map[string]string{AnnotationPolicy: "isolated-v1"}, want: Isolated},
		{name: "empty is not legacy", annotations: map[string]string{AnnotationPolicy: ""}, wantError: true},
		{name: "unknown version", annotations: map[string]string{AnnotationPolicy: "isolated-v2"}, wantError: true},
		{name: "unversioned is invalid", annotations: map[string]string{AnnotationPolicy: "isolated"}, wantError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := FromAnnotations(tt.annotations)
			if (err != nil) != tt.wantError || got != tt.want {
				t.Fatalf("got mode=%q error=%v, want mode=%q error=%t", got, err, tt.want, tt.wantError)
			}
		})
	}
}

func TestStamp(t *testing.T) {
	for _, tt := range []struct {
		mode Mode
		want Mode
	}{
		{mode: "", want: TrustedDomain},
		{mode: TrustedDomain, want: TrustedDomain},
		{mode: Isolated, want: Isolated},
		{mode: "unknown"},
	} {
		t.Run(string(tt.mode), func(t *testing.T) {
			original := map[string]string{"app": "one", AnnotationPolicy: "isolated-v1"}
			stamped, err := Stamp(original, tt.mode)
			if tt.mode == "unknown" {
				if err == nil {
					t.Fatal("invalid mode accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := FromAnnotations(stamped)
			if err != nil || got != tt.want || stamped["app"] != "one" {
				t.Fatalf("unexpected policy: %v, %v", stamped, err)
			}
			stamped["app"] = "two"
			if original["app"] != "one" || original[AnnotationPolicy] != "isolated-v1" {
				t.Fatal("input metadata mutated")
			}
		})
	}
	t.Run("nil metadata", func(t *testing.T) {
		got, err := Stamp(nil, Isolated)
		if err != nil || got[AnnotationPolicy] != "isolated-v1" {
			t.Fatalf("got %v, %v", got, err)
		}
	})
}
