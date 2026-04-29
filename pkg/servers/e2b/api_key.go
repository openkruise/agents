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

package e2b

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

func (sc *Controller) ListAPIKeys(r *http.Request) (web.ApiResponse[[]*models.TeamAPIKey], *web.ApiError) {
	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[[]*models.TeamAPIKey]{}, &web.ApiError{
			Message: "User not found",
		}
	}
	apiKeys, err := sc.keys.ListByOwner(ctx, user.ID)
	if err != nil {
		return web.ApiResponse[[]*models.TeamAPIKey]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to list API keys: %v", err),
		}
	}

	return web.ApiResponse[[]*models.TeamAPIKey]{
		Body: apiKeys,
	}, nil
}

func (sc *Controller) CreateAPIKey(r *http.Request) (web.ApiResponse[*models.CreatedTeamAPIKey], *web.ApiError) {
	var request models.NewTeamAPIKey
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Message: err.Error(),
		}
	}

	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Message: "User not found",
		}
	}
	createdAPIKey, err := sc.keys.CreateKey(ctx, user, request.Name)
	if err != nil {
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to create API key: %v", err),
		}
	}

	return web.ApiResponse[*models.CreatedTeamAPIKey]{
		Code: http.StatusCreated,
		Body: createdAPIKey,
	}, nil
}

func (sc *Controller) DeleteAPIKey(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	apiKeyID := r.PathValue("apiKeyID")

	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "User not found",
		}
	}

	key, ok := sc.keys.LoadByID(ctx, apiKeyID)
	if !ok {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: "API key not found",
		}
	}
	if key.CreatedBy == nil || key.CreatedBy.ID != user.ID && key.ID != user.ID {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "You are not allowed to delete this API key",
		}
	}
	if err := sc.keys.DeleteKey(ctx, key); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to delete API key: %v", err),
		}
	}

	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
