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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewSandboxRequest_AutoResumeJSON(t *testing.T) {
	tests := []struct {
		name   string
		body   string
		expect *AutoResumeConfig
	}{
		{
			name:   "autoResume absent",
			body:   `{"templateID":"t"}`,
			expect: nil,
		},
		{
			name:   "autoResume null",
			body:   `{"templateID":"t","autoResume":null}`,
			expect: nil,
		},
		{
			name:   "autoResume empty object defaults to disabled",
			body:   `{"templateID":"t","autoResume":{}}`,
			expect: &AutoResumeConfig{Enabled: false},
		},
		{
			name:   "autoResume enabled true",
			body:   `{"templateID":"t","autoResume":{"enabled":true}}`,
			expect: &AutoResumeConfig{Enabled: true},
		},
		{
			name:   "autoResume enabled false",
			body:   `{"templateID":"t","autoResume":{"enabled":false}}`,
			expect: &AutoResumeConfig{Enabled: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req NewSandboxRequest
			require.NoError(t, json.Unmarshal([]byte(tt.body), &req))
			assert.Equal(t, tt.expect, req.AutoResume)
		})
	}
}
