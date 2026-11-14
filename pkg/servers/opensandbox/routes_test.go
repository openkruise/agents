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

package opensandbox

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
)

// TestRegisterRoutes verifies the full Phase 1 + Phase 2 route table registers
// on a shared mux without a pattern conflict. The OpenSandbox spec puts list and
// describe on the same base path (GET /v1/sandboxes and GET
// /v1/sandboxes/{sandboxId}); Go's ServeMux panics at HandleFunc time if two
// patterns overlap ambiguously, so a clean registration proves the wildcard and
// the trailing-slash variants web.RegisterRoute adds coexist.
func TestRegisterRoutes(t *testing.T) {
	t.Run("registers without conflict", func(t *testing.T) {
		mux := http.NewServeMux()
		require.NotPanics(t, func() {
			err := RegisterRoutes(Deps{
				Mux:     mux,
				Manager: &sandboxmanager.SandboxManager{},
			})
			require.NoError(t, err)
		})
	})

	t.Run("missing mux is rejected", func(t *testing.T) {
		err := RegisterRoutes(Deps{Manager: &sandboxmanager.SandboxManager{}})
		require.Error(t, err)
	})

	t.Run("missing manager is rejected", func(t *testing.T) {
		err := RegisterRoutes(Deps{Mux: http.NewServeMux()})
		require.Error(t, err)
	})
}

// TestRegisterRoutesRouting checks that every registered route runs API-key
// authentication and uses the same OpenSandbox error body before Manager access.
func TestRegisterRoutesRouting(t *testing.T) {
	mux := http.NewServeMux()
	require.NoError(t, RegisterRoutes(Deps{
		Mux:     mux,
		Manager: &sandboxmanager.SandboxManager{},
		Keys:    &fakeKeyStorage{},
	}))

	tests := []struct {
		name   string
		method string
		target string
	}{
		{name: "create", method: http.MethodPost, target: RoutePrefix + "/sandboxes"},
		{name: "list", method: http.MethodGet, target: RoutePrefix + "/sandboxes"},
		{name: "describe", method: http.MethodGet, target: RoutePrefix + "/sandboxes/sbx-1"},
		{name: "delete", method: http.MethodDelete, target: RoutePrefix + "/sandboxes/sbx-1"},
		{name: "pause", method: http.MethodPost, target: RoutePrefix + "/sandboxes/sbx-1/pause"},
		{name: "resume", method: http.MethodPost, target: RoutePrefix + "/sandboxes/sbx-1/resume"},
		{name: "renew", method: http.MethodPost, target: RoutePrefix + "/sandboxes/sbx-1/renew-expiration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r, err := http.NewRequest(tt.method, tt.target, http.NoBody)
			require.NoError(t, err)
			mux.ServeHTTP(w, r)
			assert.Equal(t, http.StatusUnauthorized, w.Code,
				"route %s %s must run authentication", tt.method, tt.target)
			assert.Contains(t, w.Body.String(), `"code":"UNAUTHORIZED"`)
		})
	}
}
