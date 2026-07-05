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

package proxyutils

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openkruise/agents/api/v1alpha1"
)

func TestRouteString(t *testing.T) {
	tests := []struct {
		name             string
		route            Route
		expectContains   []string
		expectNotContain []string
	}{
		{
			name: "route with access token masks token value",
			route: Route{
				IP:              "10.0.0.1",
				ID:              "default--my-sandbox",
				UID:             "uid-123",
				Owner:           "user1",
				State:           v1alpha1.SandboxStateRunning,
				ResourceVersion: "100",
				AccessToken:     "super-secret-token",
			},
			expectContains:   []string{"10.0.0.1", "default--my-sandbox", "uid-123", "user1", "running", "100", "AccessToken:***"},
			expectNotContain: []string{"super-secret-token"},
		},
		{
			name: "route without access token still prints masked placeholder",
			route: Route{
				IP:              "10.0.0.2",
				ID:              "default--no-token",
				UID:             "uid-456",
				Owner:           "",
				State:           v1alpha1.SandboxStateCreating,
				ResourceVersion: "50",
				AccessToken:     "",
			},
			expectContains:   []string{"10.0.0.2", "default--no-token", "uid-456", "creating", "50", "AccessToken:***"},
			expectNotContain: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.route.String()
			for _, s := range tt.expectContains {
				assert.Contains(t, result, s)
			}
			for _, s := range tt.expectNotContain {
				assert.NotContains(t, result, s)
			}
		})
	}
}
