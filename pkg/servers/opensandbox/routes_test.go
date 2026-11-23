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

// TestRegisterRoutes verifies that the route table registers on a shared mux
// without a pattern conflict. The OpenSandbox spec puts list and
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

// TestRegisterRoutesRouting checks authentication and rejects unregistered
// descendants before Manager access.
func TestRegisterRoutesRouting(t *testing.T) {
	mux := http.NewServeMux()
	require.NoError(t, RegisterRoutes(Deps{
		Mux:     mux,
		Manager: &sandboxmanager.SandboxManager{},
		Keys:    &fakeKeyStorage{},
	}))

	network := "/sandboxes/sbx-1/networkpolicy"
	for _, tt := range []struct {
		method string
		path   string
		status int
	}{
		{http.MethodPost, "/sandboxes", http.StatusUnauthorized},
		{http.MethodGet, "/sandboxes", http.StatusUnauthorized},
		{http.MethodGet, "/sandboxes/sbx-1", http.StatusUnauthorized},
		{http.MethodDelete, "/sandboxes/sbx-1", http.StatusUnauthorized},
		{http.MethodPost, "/sandboxes/sbx-1/pause", http.StatusUnauthorized},
		{http.MethodPost, "/sandboxes/sbx-1/resume", http.StatusUnauthorized},
		{http.MethodPost, "/sandboxes/sbx-1/renew-expiration", http.StatusUnauthorized},
		{http.MethodPost, "/sandboxes/sbx-1/snapshots", http.StatusUnauthorized},
		{http.MethodGet, "/snapshots", http.StatusUnauthorized},
		{http.MethodGet, "/snapshots/", http.StatusUnauthorized},
		{http.MethodDelete, "/snapshots/snapshot-1", http.StatusUnauthorized},
		{http.MethodGet, network, http.StatusUnauthorized},
		{http.MethodPut, network, http.StatusUnauthorized},
		{http.MethodPatch, network, http.StatusUnauthorized},
		{http.MethodDelete, network, http.StatusUnauthorized},
		{http.MethodGet, "/snapshots/snapshot-1", http.StatusNotFound},
		{http.MethodPost, "/sandboxes/sbx-1/snapshots/extra", http.StatusNotFound},
		{http.MethodDelete, "/snapshots/snapshot-1/extra", http.StatusNotFound},
		{http.MethodGet, network + "/extra", http.StatusNotFound},
		{http.MethodDelete, network + "/extra", http.StatusNotFound},
	} {
		t.Run(tt.method+" "+tt.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, httptest.NewRequest(tt.method, RoutePrefix+tt.path, nil))
			require.Equal(t, tt.status, w.Code, w.Body.String())
			if tt.status == http.StatusUnauthorized {
				assert.Contains(t, w.Body.String(), `"code":"UNAUTHORIZED"`)
			} else {
				assert.Contains(t, w.Body.String(), `"code":"NOT_FOUND"`)
			}
		})
	}
}
