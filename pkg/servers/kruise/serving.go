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

package kruise

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/apiauth"
	"github.com/openkruise/agents/pkg/servers/kruise/models"
	"github.com/openkruise/agents/pkg/servers/web"
	sessionbinding "github.com/openkruise/agents/pkg/sessionservice/binding"
)

const servingAPIPath = "/v1/sessionservice/{sessionServiceID}"

func (sc *Controller) registerSessionServiceRoutes() {
	web.RegisterRoute(sc.mux, http.MethodPost, servingAPIPath+"/traffic-access-token", sc.RefreshSessionServiceTrafficAccessToken, sc.checkAPIKey)
	web.RegisterRoute(sc.mux, http.MethodGet, servingAPIPath+"/sessions/{sessionID}", sc.GetSessionServiceSession, sc.checkAPIKey)
	web.RegisterRoute(sc.mux, http.MethodGet, servingAPIPath+"/sessions", sc.ListSessionServiceSessions, sc.checkAPIKey)
	// ServeMux requires a wildcard to occupy the complete path segment.
	web.RegisterRoute(sc.mux, http.MethodPost, servingAPIPath+"/sessions/{sessionAction}", sc.CloseSessionServiceSession, sc.checkAPIKey)
}

// RefreshSessionServiceTrafficAccessToken re-signs the caller's authorized SessionService traffic JWT.
func (sc *Controller) RefreshSessionServiceTrafficAccessToken(r *http.Request) (web.ApiResponse[*models.TrafficAccessToken], *web.ApiError) {
	user := apiauth.User(r.Context())
	if user == nil {
		return web.ApiResponse[*models.TrafficAccessToken]{}, servingUnauthenticated()
	}
	result, err := sc.manager.RefreshSessionServiceTrafficAccessToken(r.Context(), sandboxmanager.RefreshSessionServiceTrafficAccessTokenOptions{
		Namespace:        apiauth.Namespace(user),
		SessionServiceID: r.PathValue("sessionServiceID"),
		User:             user.ID.String(),
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

// GetSessionServiceSession returns the durable binding in the caller's team namespace.
func (sc *Controller) GetSessionServiceSession(r *http.Request) (web.ApiResponse[*models.SessionServiceSession], *web.ApiError) {
	user := apiauth.User(r.Context())
	if user == nil {
		return web.ApiResponse[*models.SessionServiceSession]{}, servingUnauthenticated()
	}
	id := r.PathValue("sessionID")
	if !sessionbinding.ValidSessionID(id) {
		return web.ApiResponse[*models.SessionServiceSession]{}, invalidSessionServiceSessionID()
	}
	session, err := sc.manager.GetSessionServiceSession(r.Context(), apiauth.Namespace(user), r.PathValue("sessionServiceID"), id)
	if err != nil {
		return web.ApiResponse[*models.SessionServiceSession]{}, servingAPIError(err)
	}
	return web.ApiResponse[*models.SessionServiceSession]{Body: servingSessionModel(session)}, nil
}

// ListSessionServiceSessions returns durable bindings and an optional continuation header.
func (sc *Controller) ListSessionServiceSessions(r *http.Request) (web.ApiResponse[[]*models.SessionServiceSession], *web.ApiError) {
	user := apiauth.User(r.Context())
	if user == nil {
		return web.ApiResponse[[]*models.SessionServiceSession]{}, servingUnauthenticated()
	}
	opts, apiErr := parseSessionServiceSessionList(r)
	if apiErr != nil {
		return web.ApiResponse[[]*models.SessionServiceSession]{}, apiErr
	}
	page, err := sc.manager.ListSessionServiceSessions(r.Context(), apiauth.Namespace(user), r.PathValue("sessionServiceID"), opts)
	if err != nil {
		return web.ApiResponse[[]*models.SessionServiceSession]{}, servingAPIError(err)
	}
	sessions := make([]*models.SessionServiceSession, 0, len(page.Sessions))
	for _, session := range page.Sessions {
		sessions = append(sessions, servingSessionModel(session))
	}
	response := web.ApiResponse[[]*models.SessionServiceSession]{Body: sessions}
	if page.NextToken != "" {
		response.Headers = map[string]string{"x-next-token": page.NextToken}
	}
	return response, nil
}

// CloseSessionServiceSession acknowledges binding deletion without stopping its Sandbox.
func (sc *Controller) CloseSessionServiceSession(r *http.Request) (web.ApiResponse[*models.ClosedSessionServiceSession], *web.ApiError) {
	user := apiauth.User(r.Context())
	if user == nil {
		return web.ApiResponse[*models.ClosedSessionServiceSession]{}, servingUnauthenticated()
	}
	action := r.PathValue("sessionAction")
	id, ok := strings.CutSuffix(action, ":close")
	expectedPath := "/v1/sessionservice/" + r.PathValue("sessionServiceID") + "/sessions/" + action
	if !ok || strings.Contains(id, ":") || r.URL.Path != expectedPath {
		return web.ApiResponse[*models.ClosedSessionServiceSession]{}, &web.ApiError{Code: http.StatusNotFound, Message: "Route not found"}
	}
	if !sessionbinding.ValidSessionID(id) {
		return web.ApiResponse[*models.ClosedSessionServiceSession]{}, invalidSessionServiceSessionID()
	}
	session, err := sc.manager.CloseSessionServiceSession(r.Context(), apiauth.Namespace(user), r.PathValue("sessionServiceID"), id)
	if err != nil {
		return web.ApiResponse[*models.ClosedSessionServiceSession]{}, servingAPIError(err)
	}
	return web.ApiResponse[*models.ClosedSessionServiceSession]{Body: &models.ClosedSessionServiceSession{
		SessionID:        session.SessionID,
		SessionServiceID: session.SessionServiceID,
	}}, nil
}

func servingSessionModel(session sandboxmanager.SessionServiceSession) *models.SessionServiceSession {
	return &models.SessionServiceSession{
		SessionID:        session.SessionID,
		SessionServiceID: session.SessionServiceID,
		SandboxID:        session.SandboxID,
		LastRequestAt:    session.LastRequestAt,
	}
}

func parseSessionServiceSessionList(r *http.Request) (sandboxmanager.ListSessionServiceSessionsOptions, *web.ApiError) {
	opts := sandboxmanager.ListSessionServiceSessionsOptions{}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return opts, invalidSessionServicePagination()
	}
	for key, values := range query {
		if len(values) != 1 {
			return opts, invalidSessionServicePagination()
		}
		switch key {
		case "limit":
			limit, err := strconv.Atoi(values[0])
			if err != nil || limit < 1 || limit > 100 {
				return opts, invalidSessionServicePagination()
			}
			opts.Limit = &limit
		case "nextToken":
			if values[0] == "" {
				return opts, invalidSessionServicePagination()
			}
			opts.ContinueToken = values[0]
		default:
			return opts, invalidSessionServicePagination()
		}
	}
	return opts, nil
}

func servingUnauthenticated() *web.ApiError {
	return &web.ApiError{Code: http.StatusUnauthorized, Message: "Invalid API Key"}
}

func invalidSessionServiceSessionID() *web.ApiError {
	return &web.ApiError{Code: http.StatusBadRequest, Message: "Invalid Session ID"}
}

func invalidSessionServicePagination() *web.ApiError {
	return &web.ApiError{Code: http.StatusBadRequest, Message: "Invalid pagination"}
}

func servingAPIError(err error) *web.ApiError {
	result := &web.ApiError{Code: http.StatusInternalServerError, Message: "SessionService operation failed"}
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
		return &web.ApiError{Code: http.StatusNotFound, Message: "SessionService not found"}
	default:
		return result
	}
	result.Message = domainErr.Message
	return result
}
