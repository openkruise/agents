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
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

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
				assert.NotEmpty(t, team.TeamID)
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
		{
			name: "fail with empty name",
			user: &models.CreatedTeamAPIKey{
				ID:   keys.AdminKeyID,
				Key:  InitKey,
				Name: "admin",
			},
			request: models.NewTeamAPIKey{Name: ""},
			expectError: &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: "Failed to create API key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiError := controller.CreateAPIKey(NewRequest(t, nil, tt.request, nil, tt.user))
			if tt.expectError != nil {
				require.NotNil(t, apiError)
				assert.Equal(t, tt.expectError.Code, apiError.Code)
				assert.Contains(t, apiError.Message, tt.expectError.Message)
			} else {
				require.Nil(t, apiError)
				assert.Equal(t, tt.expectCode, resp.Code)
				assert.NotEmpty(t, resp.Body.Key)
				assert.Equal(t, tt.request.Name, resp.Body.Name)
				require.NotNil(t, resp.Body.CreatedBy)
				assert.Equal(t, tt.user.ID, resp.Body.CreatedBy.ID)
			}
		})
	}
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
			name:        "last admin key deletion is forbidden",
			user:        adminUser,
			targetID:    keys.AdminKeyID.String(),
			expectCode:  http.StatusForbidden,
			expectError: "last active admin",
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
