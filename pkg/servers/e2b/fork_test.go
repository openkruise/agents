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

package e2b

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseForkSandboxRequest(t *testing.T) {
	controller := &Controller{maxTimeout: 60}
	tests := []struct {
		name        string
		body        string
		expect      forkSandboxRequest
		expectError string
	}{
		{
			name:   "empty body uses E2B defaults",
			body:   "",
			expect: forkSandboxRequest{timeout: defaultForkTimeoutSeconds, count: 1},
		},
		{
			name:   "empty object uses E2B defaults",
			body:   "{}",
			expect: forkSandboxRequest{timeout: defaultForkTimeoutSeconds, count: 1},
		},
		{
			name:   "explicit values",
			body:   `{"timeout": 42, "count": 3}`,
			expect: forkSandboxRequest{timeout: 42, count: 3},
		},
		{
			name:        "zero timeout is rejected",
			body:        `{"timeout": 0}`,
			expectError: "timeout should be between 1 and 60",
		},
		{
			name:        "timeout above server limit is rejected",
			body:        `{"timeout": 61}`,
			expectError: "timeout should be between 1 and 60",
		},
		{
			name:        "zero count is rejected",
			body:        `{"count": 0}`,
			expectError: "count should be between 1 and 100",
		},
		{
			name:        "count above E2B limit is rejected",
			body:        `{"count": 101}`,
			expectError: "count should be between 1 and 100",
		},
		{
			name:        "unknown field is rejected",
			body:        `{"unknown": true}`,
			expectError: "failed to decode fork request",
		},
		{
			name:        "trailing JSON is rejected",
			body:        `{} {}`,
			expectError: "fork request must contain a single JSON object",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/sandboxes/source/fork", bytes.NewBufferString(tt.body))
			got, apiErr := controller.parseForkSandboxRequest(request)
			if tt.expectError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectError)
				return
			}
			require.Nil(t, apiErr)
			assert.Equal(t, tt.expect, got)
		})
	}
}
