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

package sandbox_manager

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/openkruise/agents/pkg/sandboxroute"
)

func TestSandboxManager_DebugMaskAccessToken(t *testing.T) {
	manager, _ := setupTestManager(t)

	tests := []struct {
		name        string
		routes      []sandboxroute.Route
		expectCount int
	}{
		{
			name:        "no routes returns empty list",
			routes:      nil,
			expectCount: 0,
		},
		{
			name: "routes with access token are masked",
			routes: []sandboxroute.Route{
				{ID: "default--sbx1", IP: "10.0.0.1", Namespace: "default", Name: "sbx1", UID: "uid-sbx1", State: "running", ResourceVersion: "1", AccessToken: "secret-token-1"},
				{ID: "default--sbx2", IP: "10.0.0.2", Namespace: "default", Name: "sbx2", UID: "uid-sbx2", State: "running", ResourceVersion: "2", AccessToken: ""},
			},
			expectCount: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Setup routes
			for _, route := range tt.routes {
				manager.proxy.SetRoute(t.Context(), route)
			}

			info := manager.GetDebugInfo()
			assert.Len(t, info.Routes, tt.expectCount)

			// Verify all AccessTokens are masked to "***"
			for _, route := range info.Routes {
				assert.Equal(t, "***", route.AccessToken, "AccessToken should be masked for route %s", route.ID)
			}
		})
	}
}
