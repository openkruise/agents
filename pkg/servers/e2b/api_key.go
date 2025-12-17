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
	apiKeys := sc.keys.ListByOwner(user.ID)

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

	key, ok := sc.keys.LoadByID(apiKeyID)
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
