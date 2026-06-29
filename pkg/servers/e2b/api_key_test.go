/*
Copyright 2025.

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
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/sandbox-manager/quota"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

type retryCleanupQuotaManager struct {
	calls       atomic.Int64
	failures    int64
	callCh      chan int64
	sawDeadline atomic.Bool
}

func newRetryCleanupQuotaManager(failures int64) *retryCleanupQuotaManager {
	return &retryCleanupQuotaManager{
		failures: failures,
		callCh:   make(chan int64, 8),
	}
}

func (m *retryCleanupQuotaManager) Acquire(context.Context, quota.AcquireRequest) error {
	return nil
}

func (m *retryCleanupQuotaManager) Release(context.Context, quota.ReleaseRequest) error {
	return nil
}

func (m *retryCleanupQuotaManager) Cleanup(ctx context.Context, _ string) error {
	if deadline, ok := ctx.Deadline(); ok {
		m.sawDeadline.Store(true)
		if remaining := time.Until(deadline); remaining > apiKeyQuotaCleanupTimeout+50*time.Millisecond {
			return errors.New("cleanup deadline is too long")
		}
	}
	call := m.calls.Add(1)
	select {
	case m.callCh <- call:
	default:
	}
	if call <= m.failures {
		return errors.New("transient cleanup failure")
	}
	return nil
}

func TestListAPIKeys(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	tests := []struct {
		name        string
		user        *models.CreatedTeamAPIKey
		expectError *web.ApiError
		expectCount int // minimum expected count
	}{
		{
			name: "success - list keys for admin user",
			user: &models.CreatedTeamAPIKey{
				ID:   keys.AdminKeyID,
				Key:  InitKey,
				Name: "admin",
			},
			expectCount: 1, // at least the admin key itself
		},
		{
			name:        "fail without user",
			user:        nil,
			expectError: &web.ApiError{Message: "User not found"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiError := controller.ListAPIKeys(NewRequest(t, nil, nil, nil, tt.user))
			if tt.expectError != nil {
				require.NotNil(t, apiError)
				assert.Contains(t, apiError.Message, tt.expectError.Message)
			} else {
				require.Nil(t, apiError)
				assert.GreaterOrEqual(t, len(resp.Body), tt.expectCount)
			}
		})
	}
}

func TestListTeams(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	ctx := logs.NewContext()
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	teamAKey, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "team-a-key", TeamName: "team-a"})
	require.NoError(t, err)
	_, err = controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "team-b-key", TeamName: "team-b"})
	require.NoError(t, err)
	refreshKeyStorageForTest(t, controller)

	tests := []struct {
		name        string
		user        *models.CreatedTeamAPIKey
		expectNames []string
		expectError string
	}{
		{
			name:        "normal key returns own team only",
			user:        teamAKey,
			expectNames: []string{"team-a"},
		},
		{
			name:        "admin key returns all active teams",
			user:        adminUser,
			expectNames: []string{"admin", "team-a", "team-b"},
		},
		{
			name:        "fail without user",
			expectError: "User not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiError := controller.ListTeams(NewRequest(t, nil, nil, nil, tt.user))
			if tt.expectError != "" {
				require.NotNil(t, apiError)
				assert.Contains(t, apiError.Message, tt.expectError)
				return
			}
			require.Nil(t, apiError)
			require.Len(t, resp.Body, len(tt.expectNames))
			gotNames := make([]string, 0, len(resp.Body))
			for _, team := range resp.Body {
				gotNames = append(gotNames, team.Name)
				assert.Empty(t, team.APIKey)
				payload, err := json.Marshal(team)
				require.NoError(t, err)
				assert.Contains(t, string(payload), `"name":`)
				assert.NotEmpty(t, team.TeamID)
				assert.Contains(t, string(payload), `"teamID":`)
				assert.NotContains(t, string(payload), `"id":`)
			}
			assert.ElementsMatch(t, tt.expectNames, gotNames)
		})
	}
}

func TestCreateAPIKey(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	tests := []struct {
		name        string
		user        *models.CreatedTeamAPIKey
		request     models.NewTeamAPIKey
		expectError *web.ApiError
		expectCode  int
	}{
		{
			name: "success - create api key",
			user: &models.CreatedTeamAPIKey{
				ID:   keys.AdminKeyID,
				Key:  InitKey,
				Name: "admin",
			},
			request:    models.NewTeamAPIKey{Name: "test-key"},
			expectCode: http.StatusCreated,
		},
		{
			name:        "fail without user",
			user:        nil,
			request:     models.NewTeamAPIKey{Name: "test-key"},
			expectError: &web.ApiError{Message: "User not found"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := NewRequest(t, nil, tt.request, nil, tt.user)
			ctx := context.WithValue(req.Context(), newAPIKeyRequestContextKey, &tt.request)
			resp, apiError := controller.CreateAPIKey(req.WithContext(ctx))
			if tt.expectError != nil {
				require.NotNil(t, apiError)
				assert.Equal(t, tt.expectError.Code, apiError.Code)
				assert.Contains(t, apiError.Message, tt.expectError.Message)
			} else {
				require.Nil(t, apiError)
				assert.Equal(t, tt.expectCode, resp.Code)
				assert.NotEmpty(t, resp.Body.Key)
				assert.True(t, keys.IsE2BSDKCompatible(resp.Body.Key))
				rawKey, decoded := keys.DecodeFromE2BSDKCompatible(resp.Body.Key)
				require.True(t, decoded)
				assert.NotEqual(t, rawKey, resp.Body.Key)
				refreshKeyStorageForTest(t, controller)
				stored, found := controller.keys.LoadByID(t.Context(), resp.Body.ID.String())
				require.True(t, found)
				assert.Equal(t, rawKey, stored.Key)
				_, found = controller.keys.LoadByKey(t.Context(), resp.Body.Key)
				assert.False(t, found)
				_, found = controller.keys.LoadByKey(t.Context(), rawKey)
				assert.True(t, found)
				assert.Equal(t, tt.request.Name, resp.Body.Name)
				require.NotNil(t, resp.Body.CreatedBy)
				assert.Equal(t, tt.user.ID, resp.Body.CreatedBy.ID)
			}
		})
	}
}

func TestCreateAPIKey_QuotaJSONValidationAndResponses(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	ctx := logs.NewContext()
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	regularUser, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "regular-user", TeamName: "regular-team"})
	require.NoError(t, err)
	refreshKeyStorageForTest(t, controller)

	tests := []struct {
		name           string
		apiKey         string
		body           string
		expectCode     int
		expectContains []string
		expectMissing  []string
	}{
		{
			name:       "admin creates limited key with limits quota",
			apiKey:     InitKey,
			body:       `{"name":"limited-key","quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":2}]}}`,
			expectCode: http.StatusCreated,
			expectContains: []string{
				`"quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":2}]}`,
			},
		},
		{
			name:       "admin creates hard zero quota key",
			apiKey:     InitKey,
			body:       `{"name":"hard-zero-key","quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":0}]}}`,
			expectCode: http.StatusCreated,
			expectContains: []string{
				`"quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":0}]}`,
			},
		},
		{
			name:       "regular key cannot set quota",
			apiKey:     regularUser.Key,
			body:       `{"name":"regular-limited","quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":2}]}}`,
			expectCode: http.StatusForbidden,
			expectContains: []string{
				`only admin can set api-key quota`,
			},
		},
		{
			name:       "negative count is rejected",
			apiKey:     InitKey,
			body:       `{"name":"negative-key","quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":-1}]}}`,
			expectCode: http.StatusBadRequest,
			expectContains: []string{
				`quota limit must be non-negative`,
			},
		},
		{
			name:       "nested wire quota field is rejected",
			apiKey:     InitKey,
			body:       `{"name":"nested-key","quota":{"running":{"sandbox.count":2}}}`,
			expectCode: http.StatusBadRequest,
			expectContains: []string{
				`unknown field "running"`,
			},
		},
		{
			name:       "missing quota scope is rejected",
			apiKey:     InitKey,
			body:       `{"name":"internal-shape","quota":{"limits":[{"dimension":"sandbox.count","limit":2}]}}`,
			expectCode: http.StatusBadRequest,
			expectContains: []string{
				`unsupported quota scope`,
			},
		},
		{
			name:       "missing quota remains unlimited",
			apiKey:     InitKey,
			body:       `{"name":"unlimited-key"}`,
			expectCode: http.StatusCreated,
			expectMissing: []string{
				`"quota"`,
				`"limits"`,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api-keys", bytes.NewBufferString(tt.body))
			req.Header.Set(models.HeaderApiKey, tt.apiKey)
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			controller.mux.ServeHTTP(rec, req)

			require.Equal(t, tt.expectCode, rec.Code, rec.Body.String())
			body := rec.Body.String()
			message := body
			if rec.Code >= http.StatusBadRequest {
				var apiErr web.ApiError
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &apiErr))
				message = apiErr.Message
			}
			for _, expected := range tt.expectContains {
				assert.Contains(t, message, expected)
			}
			for _, missing := range tt.expectMissing {
				assert.NotContains(t, body, missing)
			}
		})
	}
}

func TestListAPIKeys_ReturnsLimitsQuotaJSON(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	req := httptest.NewRequest(http.MethodPost, "/api-keys", bytes.NewBufferString(`{"name":"limited-key","quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":2}]}}`))
	req.Header.Set(models.HeaderApiKey, InitKey)
	req.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	controller.mux.ServeHTTP(createRec, req)
	require.Equal(t, http.StatusCreated, createRec.Code, createRec.Body.String())
	refreshKeyStorageForTest(t, controller)

	listReq := httptest.NewRequest(http.MethodGet, "/api-keys", nil)
	listReq.Header.Set(models.HeaderApiKey, InitKey)
	listRec := httptest.NewRecorder()
	controller.mux.ServeHTTP(listRec, listReq)

	require.Equal(t, http.StatusOK, listRec.Code, listRec.Body.String())
	assert.Contains(t, listRec.Body.String(), `"quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":2}]}`)
}

func TestCreateAPIKey_QuotaAcceptedWhenRedisAbsent(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	req := httptest.NewRequest(http.MethodPost, "/api-keys", strings.NewReader(`{"name":"limited-without-redis","quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":2}]}}`))
	req.Header.Set(models.HeaderApiKey, InitKey)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	controller.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"quota":{"limits":[{"dimension":"sandbox.count","scope":"running","limit":2}]}`)
}

func TestCompatibleAPIKeyEndpoint(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	ctx := logs.NewContext()
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	regularUser, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "regular-user", TeamName: "regular-team"})
	require.NoError(t, err)
	refreshKeyStorageForTest(t, controller)
	compatibleKey := keys.EncodeForE2BSDK(regularUser.Key)

	tests := []struct {
		name   string
		header string
		want   string
	}{
		{name: "raw key returns compatible key", header: regularUser.Key, want: compatibleKey},
		{name: "compatible key returns same compatible key", header: compatibleKey, want: compatibleKey},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/api-keys/compatible", nil)
			req.Header.Set(models.HeaderApiKey, tt.header)
			rec := httptest.NewRecorder()

			controller.mux.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code)
			var body map[string]string
			require.NoError(t, json.NewDecoder(rec.Body).Decode(&body))
			assert.Equal(t, tt.want, body["key"])
			assert.NotEqual(t, regularUser.Key, body["key"])
			assert.True(t, keys.IsE2BSDKCompatible(body["key"]))
		})
	}

	t.Run("invalid key does not appear in unauthorized response", func(t *testing.T) {
		invalidCompatibleKey := keys.EncodeForE2BSDK("missing-key")
		req := httptest.NewRequest(http.MethodGet, "/api-keys/compatible", nil)
		req.Header.Set(models.HeaderApiKey, invalidCompatibleKey)
		rec := httptest.NewRecorder()

		controller.mux.ServeHTTP(rec, req)

		require.Equal(t, http.StatusUnauthorized, rec.Code)
		assert.NotContains(t, rec.Body.String(), invalidCompatibleKey)
		assert.NotContains(t, rec.Body.String(), "missing-key")
	})
}

func TestCreateAPIKeyPermissionMiddleware(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	ctx := logs.NewContext()
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	teamAKey, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "team-a-key", TeamName: "team-a"})
	require.NoError(t, err)
	refreshKeyStorageForTest(t, controller)
	require.NoError(t, fc.Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-a"},
	}))
	require.NoError(t, fc.Create(t.Context(), &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "team-c"},
	}))

	tests := []struct {
		name        string
		user        *models.CreatedTeamAPIKey
		request     models.NewTeamAPIKey
		expectCode  int
		expectTeam  string
		expectError string
	}{
		{
			name:       "no teamName creates key in caller team",
			user:       teamAKey,
			request:    models.NewTeamAPIKey{Name: "same-team-default"},
			expectCode: http.StatusCreated,
			expectTeam: "team-a",
		},
		{
			name:       "own teamName succeeds",
			user:       teamAKey,
			request:    models.NewTeamAPIKey{Name: "same-team-explicit", TeamName: "team-a"},
			expectCode: http.StatusCreated,
			expectTeam: "team-a",
		},
		{
			name:        "non-admin cannot target another team",
			user:        teamAKey,
			request:     models.NewTeamAPIKey{Name: "other-team", TeamName: "team-b"},
			expectCode:  http.StatusForbidden,
			expectError: "not allowed",
		},
		{
			name:       "admin can target new team when namespace exists",
			user:       adminUser,
			request:    models.NewTeamAPIKey{Name: "new-team", TeamName: "team-c"},
			expectCode: http.StatusCreated,
			expectTeam: "team-c",
		},
		{
			name:       "admin without teamName creates admin key without namespace",
			user:       adminUser,
			request:    models.NewTeamAPIKey{Name: "admin-team-default"},
			expectCode: http.StatusCreated,
			expectTeam: models.AdminTeamName,
		},
		{
			name:        "missing name fails",
			user:        adminUser,
			request:     models.NewTeamAPIKey{},
			expectCode:  http.StatusBadRequest,
			expectError: "name",
		},
		{
			name:        "admin targeting missing namespace fails",
			user:        adminUser,
			request:     models.NewTeamAPIKey{Name: "missing-team", TeamName: "missing-team"},
			expectCode:  http.StatusBadRequest,
			expectError: "namespace",
		},
		{
			name:        "admin targeting invalid namespace fails",
			user:        adminUser,
			request:     models.NewTeamAPIKey{Name: "invalid-team", TeamName: "INVALID_TEAM"},
			expectCode:  http.StatusBadRequest,
			expectError: "namespace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := NewRequest(t, nil, tt.request, nil, tt.user)
			ctx := context.WithValue(logs.NewContext(), "user", tt.user)
			ctx, apiError := controller.CheckCreateAPIKeyPermission(ctx, req)
			if tt.expectError != "" {
				require.NotNil(t, apiError)
				assert.Equal(t, tt.expectCode, apiError.Code)
				assert.Contains(t, apiError.Message, tt.expectError)
				return
			}
			require.Nil(t, apiError)

			resp, apiError := controller.CreateAPIKey(req.WithContext(ctx))
			require.Nil(t, apiError)
			assert.Equal(t, tt.expectCode, resp.Code)
			require.NotNil(t, resp.Body.Team)
			assert.Equal(t, tt.expectTeam, resp.Body.Team.Name)
		})
	}
}

func TestDeleteAPIKeyPermissionMiddleware(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	ctx := logs.NewContext()
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	teamAKey, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "team-a-key", TeamName: "team-a"})
	require.NoError(t, err)
	teamASecondKey, err := controller.keys.CreateKey(ctx, teamAKey, keys.CreateKeyOptions{Name: "team-a-second"})
	require.NoError(t, err)
	teamBKey, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "team-b-key", TeamName: "team-b"})
	require.NoError(t, err)
	refreshKeyStorageForTest(t, controller)

	tests := []struct {
		name                  string
		user                  *models.CreatedTeamAPIKey
		targetID              string
		expectCode            int
		expectError           string
		expectMiddlewareError bool
	}{
		{
			name:       "same-team deletion allowed",
			user:       teamAKey,
			targetID:   teamASecondKey.ID.String(),
			expectCode: http.StatusNoContent,
		},
		{
			name:                  "non-admin cross-team deletion denied",
			user:                  teamAKey,
			targetID:              teamBKey.ID.String(),
			expectCode:            http.StatusForbidden,
			expectError:           "not allowed",
			expectMiddlewareError: true,
		},
		{
			name:       "admin deleting non-admin team key allowed",
			user:       adminUser,
			targetID:   teamBKey.ID.String(),
			expectCode: http.StatusNoContent,
		},
		{
			name:                  "missing key returns not found",
			user:                  teamAKey,
			targetID:              uuid.NewString(),
			expectCode:            http.StatusNotFound,
			expectError:           "not found",
			expectMiddlewareError: true,
		},
		{
			name:                  "fail without user",
			user:                  nil,
			targetID:              keys.AdminKeyID.String(),
			expectCode:            http.StatusUnauthorized,
			expectError:           "User not found",
			expectMiddlewareError: true,
		},
		{
			name:        "well-known admin key deletion is forbidden",
			user:        adminUser,
			targetID:    keys.AdminKeyID.String(),
			expectCode:  http.StatusForbidden,
			expectError: "well-known admin api-key cannot be deleted",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := NewRequest(t, nil, nil, map[string]string{"apiKeyID": tt.targetID}, tt.user)
			ctx := context.WithValue(logs.NewContext(), "user", tt.user)
			ctx, apiError := controller.CheckDeleteAPIKeyPermission(ctx, req)
			if tt.expectMiddlewareError {
				require.NotNil(t, apiError)
				assert.Equal(t, tt.expectCode, apiError.Code)
				assert.Contains(t, apiError.Message, tt.expectError)
				return
			}
			require.Nil(t, apiError)

			resp, apiError := controller.DeleteAPIKey(req.WithContext(ctx))
			if tt.expectError != "" {
				require.NotNil(t, apiError)
				assert.Equal(t, tt.expectCode, apiError.Code)
				assert.Contains(t, apiError.Message, tt.expectError)
				return
			}
			require.Nil(t, apiError)
			assert.Equal(t, tt.expectCode, resp.Code)
		})
	}
}

func TestDeleteAPIKeyQuotaCleanupRetriesBestEffort(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	ctx := logs.NewContext()
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	target, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "limited-key", Quota: quotaSpecForAPIKeyTest(2)})
	require.NoError(t, err)
	refreshKeyStorageForTest(t, controller)

	fakeQuota := newRetryCleanupQuotaManager(1)
	controller.manager.SetQuotaEnforcer(fakeQuota)

	req := NewRequest(t, nil, nil, map[string]string{"apiKeyID": target.ID.String()}, adminUser)
	req = req.WithContext(context.WithValue(req.Context(), targetAPIKeyContextKey, target))
	resp, apiErr := controller.DeleteAPIKey(req)
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, resp.Code)

	for i := 0; i < 2; i++ {
		select {
		case <-fakeQuota.callCh:
		case <-time.After(time.Second):
			t.Fatalf("quota cleanup did not retry after transient failure, calls=%d", fakeQuota.calls.Load())
		}
	}
	assert.True(t, fakeQuota.sawDeadline.Load())
	assert.GreaterOrEqual(t, fakeQuota.calls.Load(), int64(2))
}

func TestDeleteAPIKeyUnlimitedSkipsQuotaCleanup(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	ctx := logs.NewContext()
	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	target, err := controller.keys.CreateKey(ctx, adminUser, keys.CreateKeyOptions{Name: "unlimited-key"})
	require.NoError(t, err)
	refreshKeyStorageForTest(t, controller)

	fakeQuota := newRetryCleanupQuotaManager(0)
	controller.manager.SetQuotaEnforcer(fakeQuota)

	req := NewRequest(t, nil, nil, map[string]string{"apiKeyID": target.ID.String()}, adminUser)
	req = req.WithContext(context.WithValue(req.Context(), targetAPIKeyContextKey, target))
	resp, apiErr := controller.DeleteAPIKey(req)
	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, resp.Code)

	select {
	case <-fakeQuota.callCh:
		t.Fatalf("unlimited key deletion must not call quota cleanup")
	case <-time.After(200 * time.Millisecond):
	}
	assert.Equal(t, int64(0), fakeQuota.calls.Load())
}

func quotaSpecForAPIKeyTest(count int64) *quotaspec.QuotaSpec {
	return &quotaspec.QuotaSpec{
		Limits: []quotaspec.QuotaLimit{{
			Dimension: quotaspec.DimSandboxCount,
			Scope:     quotaspec.ScopeRunning,
			Limit:     count,
		}},
	}
}
