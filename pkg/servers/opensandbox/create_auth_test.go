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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

const allocationStopped = "identity test stopped before allocation"

// createIdentityInfra observes the real HTTP -> handler -> Manager boundary.
// No reusable test infra exposes claim arguments, so this probe stops before
// allocation rather than starting Kubernetes or a daemon for an identity test.
// Unimplemented operations deliberately fail if this path starts using them.
type createIdentityInfra struct {
	infra.Infrastructure
	lookup    *infra.HasTemplateOptions
	claim     *infra.ClaimSandboxOptions
	clone     *infra.CloneSandboxOptions
	principal *models.CreatedTeamAPIKey
}

func (p *createIdentityInfra) Build() infra.Infrastructure                     { return p }
func (p *createIdentityInfra) GetSandboxRouteSource() infra.SandboxRouteSource { return p }
func (p *createIdentityInfra) Subscribe(context.Context, infra.SandboxRouteEventHandler) error {
	return nil
}

func (p *createIdentityInfra) HasTemplate(ctx context.Context, opts infra.HasTemplateOptions) bool {
	p.lookup = &opts
	p.principal = GetUserFromContext(ctx)
	return true
}

func (p *createIdentityInfra) ClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, infra.ClaimMetrics, error) {
	p.claim = &opts
	p.principal = GetUserFromContext(ctx)
	return nil, infra.ClaimMetrics{}, managererrors.NewError(managererrors.ErrorBadRequest, allocationStopped)
}

func (p *createIdentityInfra) CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions) (infra.Sandbox, infra.CloneMetrics, error) {
	p.clone = &opts
	p.principal = GetUserFromContext(ctx)
	return nil, infra.CloneMetrics{}, managererrors.NewError(managererrors.ErrorBadRequest, allocationStopped)
}

func TestCreateHTTPIdentity(t *testing.T) {
	team := &models.Team{ID: uuid.New(), Name: "team-a"}
	userA := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "key-a", Name: "display-a", Team: team}
	userB := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "key-b", Name: "display-b", Team: team}
	admin := &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Key: "admin-key", Name: "admin", Team: models.AdminTeam()}
	otherAdmin := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "other-admin-key", Name: "other-admin", Team: models.AdminTeam()}
	legacy := &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "legacy-key", Name: "legacy"}
	store := &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{
		userA.Key: userA, userB.Key: userB, admin.Key: admin, otherAdmin.Key: otherAdmin, legacy.Key: legacy,
	}}

	for _, source := range []string{"image", "snapshot", "pool"} {
		t.Run(source, func(t *testing.T) {
			for _, tt := range []struct {
				name        string
				key         string
				e2bKey      string
				disableAuth bool
				extra       string
				wantUser    *models.CreatedTeamAPIKey
				wantNS      string
				wantStatus  int
			}{
				{name: "ordinary team", key: userA.Key, wantUser: userA, wantNS: team.Name},
				{name: "same team retains distinct key owner", key: userB.Key, wantUser: userB, wantNS: team.Name},
				{name: "SDK encoded key uses native key normalization", key: keys.EncodeForE2BSDK(userA.Key), wantUser: userA, wantNS: team.Name},
				{name: "admin lookup scope retains key owner", key: admin.Key, wantUser: admin},
				{name: "other admin key retains distinct owner", key: otherAdmin.Key, wantUser: otherAdmin},
				{name: "legacy key retains its own owner", key: legacy.Key, wantUser: legacy},
				{name: "disabled authentication uses canonical admin owner", disableAuth: true, wantUser: anonymousUser},
				{name: "disabled authentication ignores presented key", disableAuth: true, key: userA.Key, wantUser: anonymousUser},
				{name: "enabled canonical admin matches anonymous owner", key: admin.Key, wantUser: anonymousUser},
				{name: "body cannot replace authenticated identity", key: userA.Key, extra: `,"owner":"forged","user":"forged","namespace":"other","tenant":"other","team":{"name":"other"}`, wantUser: userA, wantNS: team.Name},
				{name: "each protocol selects its own header", key: userA.Key, e2bKey: userB.Key, wantUser: userA, wantNS: team.Name},
				{name: "missing API key", wantStatus: http.StatusUnauthorized},
				{name: "invalid API key", key: "invalid", wantStatus: http.StatusUnauthorized},
				{name: "E2B header alone cannot authenticate OpenSandbox", e2bKey: userA.Key, wantStatus: http.StatusUnauthorized},
				{name: "valid E2B key cannot rescue invalid OpenSandbox key", key: "invalid", e2bKey: userA.Key, wantStatus: http.StatusUnauthorized},
				{name: "secureAccess false does not disable API auth", extra: `,"secureAccess":false`, wantStatus: http.StatusUnauthorized},
				{name: "metadata cannot replace persisted owner", key: userA.Key, extra: `,"metadata":{"` + agentsv1alpha1.AnnotationOwner + `":"forged"}`, wantStatus: http.StatusBadRequest},
			} {
				t.Run(tt.name, func(t *testing.T) {
					probe := &createIdentityInfra{}
					manager, err := sandboxmanager.NewSandboxManagerBuilder(config.SandboxManagerOptions{}).
						WithCustomInfra(func() (infra.Builder, error) { return probe, nil }).Build()
					require.NoError(t, err)
					mux := http.NewServeMux()
					var keyStore keys.KeyStorage = store
					if tt.disableAuth {
						keyStore = nil
					}
					require.NoError(t, RegisterRoutes(Deps{
						Mux: mux, Manager: manager, Keys: keyStore, MaxTimeout: 3600,
					}))
					bodyJSON := imageRequest(tt.extra)
					if source == "snapshot" {
						bodyJSON = `{"snapshotId":"snapshot-1"` + tt.extra + `}`
					} else if source == "pool" {
						bodyJSON = `{"extensions":{"poolRef":"pool-a"}` + tt.extra + `}`
					}
					r := newCreateRequest(t, bodyJSON)
					if tt.key != "" {
						r.Header.Set(HeaderOpenSandboxAPIKey, tt.key)
					}
					if tt.e2bKey != "" {
						r.Header.Set(models.HeaderApiKey, tt.e2bKey)
					}
					w := httptest.NewRecorder()
					mux.ServeHTTP(w, r)
					var body map[string]any
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
					assert.Len(t, body, 2)
					assert.NotEmpty(t, w.Header().Get("X-Request-ID"))
					if tt.wantStatus != 0 {
						require.Equal(t, tt.wantStatus, w.Code)
						assert.Nil(t, probe.lookup, "rejection must precede source lookup")
						assert.Nil(t, probe.claim, "rejection must precede allocation")
						assert.Nil(t, probe.clone)
						if tt.wantStatus == http.StatusUnauthorized {
							assert.Equal(t, map[string]any{"code": "UNAUTHORIZED", "message": "Invalid API Key"}, body)
						} else {
							assert.Equal(t, "INVALID_REQUEST", body["code"])
						}
						return
					}
					// A successful identity check reaches the probe, which deliberately
					// returns a known error. This is not a successful sandbox creation.
					require.Equal(t, http.StatusBadRequest, w.Code)
					assert.Equal(t, "INVALID_REQUEST", body["code"])
					assert.Contains(t, body["message"], allocationStopped)
					if source != "pool" {
						assert.Nil(t, probe.lookup, "cold creation must not require a preexisting template")
					}
					require.NotNil(t, probe.principal)
					assert.Equal(t, tt.wantUser.ID, probe.principal.ID)
					if source == "snapshot" {
						assert.Nil(t, probe.claim)
						require.NotNil(t, probe.clone)
						assert.Equal(t, tt.wantNS, probe.clone.Namespace)
						assert.Equal(t, tt.wantUser.ID.String(), probe.clone.User)
						assert.Equal(t, "snapshot-1", probe.clone.CheckPointID)
						require.NotNil(t, probe.clone.Startup)
						assert.Equal(t, []string{"sleep", "infinity"}, probe.clone.Startup.Command)
						assert.NotEmpty(t, probe.clone.Startup.InitRuntime.AccessToken)
						assert.False(t, probe.clone.Startup.InitRuntime.ReInit)
					} else {
						assert.Nil(t, probe.clone)
						require.NotNil(t, probe.claim)
						assert.Equal(t, tt.wantNS, probe.claim.Namespace)
						if source == "pool" {
							require.NotNil(t, probe.lookup)
							assert.Equal(t, tt.wantNS, probe.lookup.Namespace)
							assert.Equal(t, "pool-a", probe.claim.Template)
							assert.Nil(t, probe.claim.ColdStart)
							require.NotNil(t, probe.claim.StartProcess)
							assert.Equal(t, []string{"sleep", "infinity"}, probe.claim.StartProcess.Command)
						} else {
							assert.Empty(t, probe.claim.Template)
							require.NotNil(t, probe.claim.ColdStart)
							assert.Equal(t, "python:3.11", probe.claim.ColdStart.Image)
							assert.Equal(t, []string{"sleep", "600"}, probe.claim.ColdStart.Command)
							assert.Nil(t, probe.claim.StartProcess)
						}
						assert.Equal(t, tt.wantUser.ID.String(), probe.claim.User)
					}
				})
			}
		})
	}
}

func TestAuthenticationContextsAreProtocolScoped(t *testing.T) {
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "opensandbox-user"}
	store := &fakeKeyStorage{byKey: map[string]*models.CreatedTeamAPIKey{"key": user}}
	request := newCreateRequest(t, imageRequest(""))
	request.Header.Set(HeaderOpenSandboxAPIKey, "key")
	ctx, apiErr := CheckApiKey(store)(t.Context(), request)
	require.Nil(t, apiErr)
	assert.Same(t, user, GetUserFromContext(ctx))
	assert.Nil(t, e2b.GetUserFromContext(ctx))

	controller := e2b.NewController(e2b.ControllerOptions{})
	e2bCtx, apiErr := controller.CheckApiKey(t.Context(), request)
	require.Nil(t, apiErr)
	assert.Nil(t, GetUserFromContext(e2bCtx))
	assert.Equal(t, keys.AdminKeyID, e2b.GetUserFromContext(e2bCtx).ID)

	// Populating either context must not overwrite the other protocol's caller.
	for _, openSandboxFirst := range []bool{true, false} {
		var combined context.Context
		if openSandboxFirst {
			combined, apiErr = controller.CheckApiKey(ctx, request)
		} else {
			combined, apiErr = CheckApiKey(store)(e2bCtx, request)
		}
		require.Nil(t, apiErr)
		assert.Same(t, user, GetUserFromContext(combined))
		assert.Equal(t, keys.AdminKeyID, e2b.GetUserFromContext(combined).ID)
	}
}
