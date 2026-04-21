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
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
			}
		})
	}
}

func TestDeleteAPIKey(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	adminUser := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	// Create a key to delete later
	createResp, apiError := controller.CreateAPIKey(NewRequest(t, nil, models.NewTeamAPIKey{Name: "key-to-delete"}, nil, adminUser))
	require.Nil(t, apiError)
	createdKeyID := createResp.Body.ID

	tests := []struct {
		name        string
		user        *models.CreatedTeamAPIKey
		pathValues  map[string]string
		expectError *web.ApiError
		expectCode  int
	}{
		{
			name:       "success - delete api key by creator",
			user:       adminUser,
			pathValues: map[string]string{"apiKeyID": createdKeyID.String()},
			expectCode: http.StatusNoContent,
		},
		{
			name:       "fail without user",
			user:       nil,
			pathValues: map[string]string{"apiKeyID": keys.AdminKeyID.String()},
			expectError: &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: "User not found",
			},
		},
		{
			name:       "fail with non-existent key",
			user:       adminUser,
			pathValues: map[string]string{"apiKeyID": uuid.NewString()},
			expectError: &web.ApiError{
				Code:    http.StatusNotFound,
				Message: "API key not found",
			},
		},
		{
			name:       "fail with unauthorized user",
			user:       &models.CreatedTeamAPIKey{ID: uuid.New(), Key: "other-key", Name: "other"},
			pathValues: map[string]string{"apiKeyID": keys.AdminKeyID.String()},
			expectError: &web.ApiError{
				Code:    http.StatusUnauthorized,
				Message: "You are not allowed to delete this API key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, apiError := controller.DeleteAPIKey(NewRequest(t, nil, nil, tt.pathValues, tt.user))
			if tt.expectError != nil {
				require.NotNil(t, apiError)
				assert.Equal(t, tt.expectError.Code, apiError.Code)
				assert.Contains(t, apiError.Message, tt.expectError.Message)
			} else {
				require.Nil(t, apiError)
				assert.Equal(t, tt.expectCode, resp.Code)
			}
		})
	}
}
