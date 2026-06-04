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
	"errors"
	"fmt"
	"net/http"

	"github.com/openkruise/agents/pkg/servers/e2b/keys"
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
	apiKeys, err := sc.keys.ListByOwnerTeam(ctx, user)
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
	ctx := r.Context()
	request, ok := GetNewAPIKeyRequestFromContext(ctx)
	if !ok {
		// CheckCreateAPIKeyPermission middleware is required but was not in the chain.
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "request not found in context",
		}
	}

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Message: "User not found",
		}
	}

	createdAPIKey, err := sc.keys.CreateKey(ctx, user, keys.CreateKeyOptions{
		Name:     request.Name,
		TeamName: request.TeamName,
	})
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

func validateCreateAPIKeyRequest(request *models.NewTeamAPIKey) *web.ApiError {
	if request == nil || request.Name == "" {
		return &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "api-key name is required",
		}
	}
	return nil
}

func (sc *Controller) ListTeams(r *http.Request) (web.ApiResponse[[]*models.ListedTeam], *web.ApiError) {
	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[[]*models.ListedTeam]{}, &web.ApiError{
			Message: "User not found",
		}
	}
	teams, err := sc.keys.ListTeams(ctx, user)
	if err != nil {
		return web.ApiResponse[[]*models.ListedTeam]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to list teams: %v", err),
		}
	}
	return web.ApiResponse[[]*models.ListedTeam]{Body: teams}, nil
}

func (sc *Controller) DeleteAPIKey(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "User not found",
		}
	}

	key := GetTargetAPIKeyFromContext(ctx)
	if key != nil {
		if err := sc.keys.DeleteKey(ctx, key); err != nil {
			if errors.Is(err, keys.ErrAdminKeyUndeletable) {
				return web.ApiResponse[struct{}]{}, &web.ApiError{
					Code:    http.StatusForbidden,
					Message: err.Error(),
				}
			}
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: fmt.Sprintf("Failed to delete API key: %v", err),
			}
		}
	}

	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
