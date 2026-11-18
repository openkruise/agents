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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	sessionbinding "github.com/openkruise/agents/pkg/servingsession/binding"
)

const (
	servingAPIPath         = "/kruise/api/v1alpha1/sandboxservings/{servingID}"
	servingNextTokenHeader = "x-next-token"
	servingLimitParam      = "limit"
	servingNextTokenParam  = "nextToken"
)

func (sc *Controller) registerServingRoutes() {
	web.RegisterRoute(sc.mux, http.MethodPost, servingAPIPath+"/traffic-access-token", sc.RefreshServingTrafficAccessToken, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, http.MethodGet, servingAPIPath+"/sessions/{sessionID}", sc.GetServingSession, sc.CheckApiKey)
	web.RegisterRoute(sc.mux, http.MethodGet, servingAPIPath+"/sessions", sc.ListServingSessions, sc.CheckApiKey)
	// ServeMux requires a wildcard to occupy the complete path segment.
	web.RegisterRoute(sc.mux, http.MethodPost, servingAPIPath+"/sessions/{sessionAction}", sc.CloseServingSession, sc.CheckApiKey)
}

// RefreshServingTrafficAccessToken re-signs the caller's authorized Serving traffic JWT.
func (sc *Controller) RefreshServingTrafficAccessToken(r *http.Request) (web.ApiResponse[*models.TrafficAccessToken], *web.ApiError) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[*models.TrafficAccessToken]{}, servingUnauthenticated()
	}
	result, err := sc.manager.RefreshServingTrafficAccessToken(r.Context(), sandboxmanager.RefreshServingTrafficAccessTokenOptions{
		Namespace: sc.getNamespaceOfUser(user),
		ServingID: r.PathValue("servingID"),
		User:      user.ID.String(),
	})
	if err != nil {
		return web.ApiResponse[*models.TrafficAccessToken]{}, servingAPIError(err)
	}
	return web.ApiResponse[*models.TrafficAccessToken]{
		Code: http.StatusOK,
		Body: &models.TrafficAccessToken{
			TrafficAccessToken:           result.Token,
			TrafficAccessTokenExpiration: result.Expiration.UTC().Format(time.RFC3339),
		},
	}, nil
}

// GetServingSession returns the durable binding in the caller's team namespace.
func (sc *Controller) GetServingSession(r *http.Request) (web.ApiResponse[*models.ServingSession], *web.ApiError) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[*models.ServingSession]{}, servingUnauthenticated()
	}
	id := r.PathValue("sessionID")
	if !sessionbinding.ValidSessionID(id) {
		return web.ApiResponse[*models.ServingSession]{}, invalidServingSessionID()
	}
	session, err := sc.manager.GetServingSession(r.Context(), sc.getNamespaceOfUser(user), r.PathValue("servingID"), id)
	if err != nil {
		return web.ApiResponse[*models.ServingSession]{}, servingAPIError(err)
	}
	return web.ApiResponse[*models.ServingSession]{Body: servingSessionModel(session)}, nil
}

// ListServingSessions returns durable bindings and an optional continuation header.
func (sc *Controller) ListServingSessions(r *http.Request) (web.ApiResponse[[]*models.ServingSession], *web.ApiError) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[[]*models.ServingSession]{}, servingUnauthenticated()
	}
	opts, apiErr := parseServingSessionList(r)
	if apiErr != nil {
		return web.ApiResponse[[]*models.ServingSession]{}, apiErr
	}
	page, err := sc.manager.ListServingSessions(r.Context(), sc.getNamespaceOfUser(user), r.PathValue("servingID"), opts)
	if err != nil {
		return web.ApiResponse[[]*models.ServingSession]{}, servingAPIError(err)
	}
	sessions := make([]*models.ServingSession, 0, len(page.Sessions))
	for _, session := range page.Sessions {
		sessions = append(sessions, servingSessionModel(session))
	}
	response := web.ApiResponse[[]*models.ServingSession]{Body: sessions}
	if page.NextToken != "" {
		response.Headers = map[string]string{servingNextTokenHeader: page.NextToken}
	}
	return response, nil
}

// CloseServingSession acknowledges binding deletion without stopping its Sandbox.
func (sc *Controller) CloseServingSession(r *http.Request) (web.ApiResponse[*models.ClosedServingSession], *web.ApiError) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[*models.ClosedServingSession]{}, servingUnauthenticated()
	}
	action := r.PathValue("sessionAction")
	id, ok := strings.CutSuffix(action, ":close")
	expectedPath := "/kruise/api/v1alpha1/sandboxservings/" + r.PathValue("servingID") + "/sessions/" + action
	if !ok || strings.Contains(id, ":") || r.URL.Path != expectedPath {
		return web.ApiResponse[*models.ClosedServingSession]{}, &web.ApiError{Code: http.StatusNotFound, Message: "Route not found"}
	}
	if !sessionbinding.ValidSessionID(id) {
		return web.ApiResponse[*models.ClosedServingSession]{}, invalidServingSessionID()
	}
	session, err := sc.manager.CloseServingSession(r.Context(), sc.getNamespaceOfUser(user), r.PathValue("servingID"), id)
	if err != nil {
		return web.ApiResponse[*models.ClosedServingSession]{}, servingAPIError(err)
	}
	return web.ApiResponse[*models.ClosedServingSession]{Body: &models.ClosedServingSession{
		SessionID: session.SessionID,
		ServingID: session.ServingID,
		State:     session.State,
	}}, nil
}

func servingSessionModel(session sandboxmanager.ServingSession) *models.ServingSession {
	return &models.ServingSession{
		SessionID:     session.SessionID,
		ServingID:     session.ServingID,
		SandboxID:     session.SandboxID,
		State:         session.State,
		LastRequestAt: session.LastRequestAt,
	}
}

func parseServingSessionList(r *http.Request) (sandboxmanager.ListServingSessionsOptions, *web.ApiError) {
	opts := sandboxmanager.ListServingSessionsOptions{}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return opts, invalidServingPagination()
	}
	for key, values := range query {
		if len(values) != 1 {
			return opts, invalidServingPagination()
		}
		switch key {
		case servingLimitParam:
			limit, err := strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > 100 {
				return opts, invalidServingPagination()
			}
			opts.Limit = &limit
		case servingNextTokenParam:
			if values[0] == "" {
				return opts, invalidServingPagination()
			}
			opts.ContinueToken = values[0]
		default:
			return opts, invalidServingPagination()
		}
	}
	return opts, nil
}

func servingUnauthenticated() *web.ApiError {
	return &web.ApiError{Code: http.StatusUnauthorized, Message: "Invalid API Key"}
}

func invalidServingSessionID() *web.ApiError {
	return &web.ApiError{Code: http.StatusBadRequest, Message: "Invalid Session ID"}
}

func invalidServingPagination() *web.ApiError {
	return &web.ApiError{Code: http.StatusBadRequest, Message: "Invalid pagination"}
}

func servingAPIError(err error) *web.ApiError {
	result := &web.ApiError{Code: http.StatusInternalServerError, Message: "Serving operation failed"}
	var domainErr *managererrors.Error
	if !errors.As(err, &domainErr) {
		return result
	}
	switch domainErr.Code {
	case managererrors.ErrorBadRequest:
		result.Code = http.StatusBadRequest
	case managererrors.ErrorNotFound:
		result.Code = http.StatusNotFound
	case managererrors.ErrorUnavailable:
		result.Code = http.StatusServiceUnavailable
	case managererrors.ErrorConflict:
		result.Code = http.StatusConflict
	case managererrors.ErrorNotAllowed:
		return &web.ApiError{Code: http.StatusNotFound, Message: "SandboxServing not found"}
	default:
		return result
	}
	result.Message = domainErr.Message
	return result
}
