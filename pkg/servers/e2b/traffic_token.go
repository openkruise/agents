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
	"math"
	"net/http"
	"strconv"
	"time"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// RefreshTrafficAccessToken re-signs the current caller's Sandbox traffic JWT.
func (sc *Controller) RefreshTrafficAccessToken(r *http.Request) (web.ApiResponse[*models.TrafficAccessToken], *web.ApiError) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[*models.TrafficAccessToken]{}, trafficTokenAPIError(
			managererrors.NewError(managererrors.ErrorNotAllowed, "authenticated user is required"),
			sc.mgrOpts.TrafficAccessToken.RefreshMinInterval,
		)
	}
	result, err := sc.manager.RefreshTrafficAccessToken(r.Context(), sandboxmanager.RefreshTrafficAccessTokenOptions{
		Namespace: sc.getNamespaceOfUser(user),
		SandboxID: r.PathValue("sandboxID"),
		User:      user.ID.String(),
	})
	if err != nil {
		return web.ApiResponse[*models.TrafficAccessToken]{}, trafficTokenAPIError(err, sc.mgrOpts.TrafficAccessToken.RefreshMinInterval)
	}
	return web.ApiResponse[*models.TrafficAccessToken]{
		Code: http.StatusOK,
		Body: &models.TrafficAccessToken{
			TrafficAccessToken:           result.Token,
			TrafficAccessTokenExpiration: result.Expiration.UTC().Format(time.RFC3339),
		},
	}, nil
}

func trafficTokenAPIError(err error, retryAfter time.Duration) *web.ApiError {
	apiErr := &web.ApiError{Code: http.StatusInternalServerError, Message: "Failed to refresh traffic access token"}
	switch managererrors.GetErrCode(err) {
	case managererrors.ErrorBadRequest:
		apiErr.Code = http.StatusBadRequest
		apiErr.Message = "Invalid traffic access token refresh request"
	case managererrors.ErrorNotFound, managererrors.ErrorNotAllowed:
		apiErr.Code = http.StatusNotFound
		apiErr.Message = "Sandbox not found"
	case managererrors.ErrorConflict:
		apiErr.Code = http.StatusConflict
		apiErr.Message = "Sandbox cannot refresh its traffic access token"
	case managererrors.ErrorRateLimited:
		apiErr.Code = http.StatusTooManyRequests
		apiErr.Message = "Traffic access token refresh is rate limited"
		seconds := int(math.Ceil(retryAfter.Seconds()))
		if seconds < 1 {
			seconds = 1
		}
		apiErr.Headers = map[string]string{"Retry-After": strconv.Itoa(seconds)}
	case managererrors.ErrorUnavailable:
		apiErr.Code = http.StatusServiceUnavailable
		apiErr.Message = "Traffic access token issuer is unavailable"
	}
	return apiErr
}
