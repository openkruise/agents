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

package identity

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestValidateAgentName exercises the standalone validator consumed by the
// E2B parse layer: an empty value (not opted in) and any valid Kubernetes
// label value must pass, while values violating label-value constraints must
// be rejected with an error naming the annotation key.
func TestValidateAgentName(t *testing.T) {
	tests := []struct {
		name        string
		agentName   string
		expectError string
	}{
		{
			name:      "empty value is valid",
			agentName: "",
		},
		{
			name:      "valid label value passes",
			agentName: "my-agent_v1.0",
		},
		{
			name:        "value exceeding 63 characters is rejected",
			agentName:   strings.Repeat("a", 64),
			expectError: "is not a valid label value",
		},
		{
			name:        "value with invalid characters is rejected",
			agentName:   "agent/with/slashes",
			expectError: "is not a valid label value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateAgentName(tt.agentName)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Contains(t, err.Error(), AnnotationAgentName,
					"error must name the offending annotation key for diagnosability")
				return
			}
			require.NoError(t, err)
		})
	}
}
