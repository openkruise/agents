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

package models

import (
	"encoding/json"
	"testing"
)

func TestNewSandboxRequestAutoResumeUnmarshal(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		expectNil     bool
		expectEnabled bool
	}{
		{
			name:      "omitted auto resume",
			body:      `{"templateID":"t1"}`,
			expectNil: true,
		},
		{
			name:      "null auto resume",
			body:      `{"templateID":"t1","autoResume":null}`,
			expectNil: true,
		},
		{
			name:          "enabled auto resume",
			body:          `{"templateID":"t1","autoResume":{"enabled":true}}`,
			expectEnabled: true,
		},
		{
			name: "disabled auto resume",
			body: `{"templateID":"t1","autoResume":{"enabled":false}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var request NewSandboxRequest
			if err := json.Unmarshal([]byte(tt.body), &request); err != nil {
				t.Fatalf("json.Unmarshal() error = %v", err)
			}

			if tt.expectNil {
				if request.AutoResume != nil {
					t.Fatalf("AutoResume = %#v, want nil", request.AutoResume)
				}
				return
			}

			if request.AutoResume == nil {
				t.Fatal("AutoResume = nil, want non-nil")
			}
			if request.AutoResume.Enabled != tt.expectEnabled {
				t.Fatalf("AutoResume.Enabled = %v, want %v", request.AutoResume.Enabled, tt.expectEnabled)
			}
		})
	}
}
