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
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"k8s.io/klog/v2"

	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

const (
	apiKeyQuotaCleanupTimeout        = 10 * time.Second
	apiKeyQuotaCleanupInitialBackoff = 100 * time.Millisecond
	apiKeyQuotaCleanupMaxBackoff     = 500 * time.Millisecond
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
		Quota:    request.QuotaSpec.DeepCopy(),
	})
	if err != nil {
		return web.ApiResponse[*models.CreatedTeamAPIKey]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to create API key: %v", err),
		}
	}

	return web.ApiResponse[*models.CreatedTeamAPIKey]{
		Code: http.StatusCreated,
		Body: keys.ConvertToE2BCompatibleCreatedAPIKey(createdAPIKey),
	}, nil
}

// GetCompatibleAPIKey returns the E2B SDK-compatible form of the caller's API key.
//
// We intentionally read the raw key directly from the request header here instead of
// retrieving it from a value stashed into the request context by CheckApiKey. Keeping
// the lookup local to this low-frequency endpoint improves readability and avoids
// scattering the key-passing logic across the auth middleware and the handler.
func (sc *Controller) GetCompatibleAPIKey(r *http.Request) (web.ApiResponse[*models.CompatibleAPIKey], *web.ApiError) {
	rawAPIKey := keys.ToStoredRawAPIKey(r.Header.Get(models.HeaderApiKey))

	return web.ApiResponse[*models.CompatibleAPIKey]{
		Body: &models.CompatibleAPIKey{Key: keys.EncodeForE2BSDK(rawAPIKey)},
	}, nil
}

func validateCreateAPIKeyRequest(request *models.NewTeamAPIKey) *web.ApiError {
	if request == nil || request.Name == "" {
		return &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "api-key name is required",
		}
	}
	normalizedQuota, err := quotaspec.NormalizeQuotaSpec(request.QuotaSpec)
	if err != nil {
		return &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}
	request.QuotaSpec = normalizedQuota
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
		if key.QuotaSpec != nil && key.QuotaSpec.IsLimited() {
			// Deleted-key quota cleanup is bounded best-effort. If Redis stays
			// unavailable past the retry window, the leftover q:live/q:sum keys become
			// dead memory because API-key IDs are never reused and deleted keys are no
			// longer enumerated by anti-drift. A stronger tombstone/TTL cleanup policy is
			// intentionally deferred to a later PR.
			go sc.cleanupDeletedAPIKeyQuota(ctx, key.ID.String())
		}
	}

	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

func (sc *Controller) cleanupDeletedAPIKeyQuota(ctx context.Context, apiKeyID string) {
	if sc == nil || apiKeyID == "" {
		return
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), apiKeyQuotaCleanupTimeout)
	defer cancel()
	log := klog.FromContext(cleanupCtx)
	backoff := apiKeyQuotaCleanupInitialBackoff
	var lastErr error

	for {
		err := sc.manager.CleanupQuota(cleanupCtx, apiKeyID)
		if err == nil {
			return
		}
		lastErr = err

		select {
		case <-cleanupCtx.Done():
			log.Error(lastErr, "failed to cleanup quota live set for deleted api-key", "apiKeyID", apiKeyID, "contextError", cleanupCtx.Err())
			return
		case <-time.After(backoff):
		}
		if backoff < apiKeyQuotaCleanupMaxBackoff {
			backoff *= 2
			if backoff > apiKeyQuotaCleanupMaxBackoff {
				backoff = apiKeyQuotaCleanupMaxBackoff
			}
		}
	}
}
