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
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func newSandboxScopedRequest(t *testing.T, method, target string, sbx *sandboxcr.Sandbox) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, RoutePrefix+"/sandboxes/sbx-1"+target, nil)
	// A user is always stashed so handlers that read the user before the sandbox
	// (delete) reach their sandbox-nil guard rather than short-circuiting on the
	// user check.
	ctx := context.WithValue(r.Context(), userContextKey, &models.CreatedTeamAPIKey{
		ID: uuid.New(), Name: "tester", Team: models.AdminTeam(),
	})
	if sbx != nil {
		ctx = context.WithValue(ctx, sandboxContextKey, sbx)
	}
	return r.WithContext(ctx)
}

func TestDescribeSandbox(t *testing.T) {
	t.Run("viewable sandbox is projected", func(t *testing.T) {
		sbx := runningClaimedSandbox(t, "team-a", "sbx-1", "sbx-id-1", nil)
		s := &Server{manager: nil}

		resp, apiErr := s.DescribeSandbox(newSandboxScopedRequest(t, http.MethodGet, "", sbx))
		require.Nil(t, apiErr)
		assert.Equal(t, "sbx-id-1", resp.Body.ID)
		assert.Equal(t, SandboxStateRunning, resp.Body.Status.State)
		assert.Equal(t, []string{}, resp.Body.Entrypoint)
	})

	t.Run("non-viewable sandbox is not found", func(t *testing.T) {
		sbx := runningClaimedSandbox(t, "team-a", "sbx-1", "sbx-id-1", func(s *agentsv1alpha1.Sandbox) {
			s.Status.Phase = agentsv1alpha1.SandboxTerminating
		})
		s := &Server{manager: nil}

		resp, apiErr := s.DescribeSandbox(newSandboxScopedRequest(t, http.MethodGet, "", sbx))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusNotFound, apiErr.Code)
		assert.Equal(t, SandboxResponse{}, resp.Body)
	})

	t.Run("missing sandbox in context is internal error", func(t *testing.T) {
		s := &Server{manager: nil}

		resp, apiErr := s.DescribeSandbox(newSandboxScopedRequest(t, http.MethodGet, "", nil))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
		assert.Equal(t, SandboxResponse{}, resp.Body)
	})
}

// TestLifecycleHandlers_MissingSandbox covers the shared guard every sandbox-
// scoped handler runs first: when loadOwnedSandbox did not stash a sandbox (a
// wiring bug, since the middleware otherwise short-circuits with 404), the
// handler returns 500 rather than dereferencing nil. The manager is nil so any
// handler that reached its manager call would panic instead of returning.
func TestLifecycleHandlers_MissingSandbox(t *testing.T) {
	s := &Server{manager: nil}
	r := func(method, target string) *http.Request {
		return newSandboxScopedRequest(t, method, target, nil)
	}

	t.Run("delete", func(t *testing.T) {
		_, apiErr := s.DeleteSandbox(r(http.MethodDelete, ""))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
	})
	t.Run("pause", func(t *testing.T) {
		_, apiErr := s.PauseSandbox(r(http.MethodPost, "/pause"))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
	})
	t.Run("resume", func(t *testing.T) {
		_, apiErr := s.ResumeSandbox(r(http.MethodPost, "/resume"))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
	})
}
