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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

func TestServingNativeRoutes(t *testing.T) {
	sc := NewController(ControllerOptions{})
	sc.registerRoutes()
	tests := []struct{ method, path, pattern string }{
		{"POST", "/kruise/api/v1alpha1/sandboxservings/serving-a/traffic-access-token", "POST /kruise/api/v1alpha1/sandboxservings/{servingID}/traffic-access-token"},
		{"GET", "/kruise/api/v1alpha1/sandboxservings/serving-a/sessions/session-a", "GET /kruise/api/v1alpha1/sandboxservings/{servingID}/sessions/{sessionID}"},
		{"GET", "/kruise/api/v1alpha1/sandboxservings/serving-a/sessions", "GET /kruise/api/v1alpha1/sandboxservings/{servingID}/sessions"},
		{"POST", "/kruise/api/v1alpha1/sandboxservings/serving-a/sessions/session-a:close", "POST /kruise/api/v1alpha1/sandboxservings/{servingID}/sessions/{sessionAction}"},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.path, func(t *testing.T) {
			_, pattern := sc.mux.Handler(httptest.NewRequest(tt.method, tt.path, nil))
			require.Equal(t, tt.pattern, pattern)
		})
	}
	for _, path := range []string{"/sandboxservings/serving-a/sessions", "/api/v1alpha1/sandboxservings/serving-a/sessions", "/kruise/api/sandboxservings/serving-a/sessions"} {
		_, pattern := sc.mux.Handler(httptest.NewRequest("GET", path, nil))
		require.Empty(t, pattern)
	}
}

func TestServingLegacyErrorShape(t *testing.T) {
	mux := http.NewServeMux()
	web.RegisterRoute(mux, "GET", "/legacy", func(*http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
		return web.ApiResponse[struct{}]{}, &web.ApiError{Code: 404, Message: "Sandbox not found"}
	})
	request := httptest.NewRequest("GET", "/legacy", nil)
	request.Header.Set("X-Request-ID", "existing-request")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	require.Equal(t, 404, response.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, map[string]any{"code": float64(404), "headers": nil, "message": "Sandbox not found", "request_id": "existing-request"}, body)
}

func TestServingRequestValidation(t *testing.T) {
	sc := NewController(ControllerOptions{})
	sc.registerRoutes()
	tests := []struct {
		method, suffix, message string
		status                  int
	}{
		{"GET", "/sessions/bad.session", "Invalid Session ID", 400},
		{"GET", "/sessions/" + strings.Repeat("a", 65), "Invalid Session ID", 400},
		{"POST", "/sessions/session-a:stop", "", 404},
		{"POST", "/sessions/session-a:close/extra", "", 404},
		{"POST", "/sessions/session-a:close/sessions/session-a:close", "", 404},
		{"POST", "/sessions/session-a:close/", "", 404},
		{"POST", "/sessions/session-a:close:close", "", 404},
		{"POST", "/sessions/bad.session:close", "Invalid Session ID", 400},
		{"GET", "/sessions?limit=0", "Invalid pagination", 400},
		{"GET", "/sessions?limit=101", "Invalid pagination", 400},
		{"GET", "/sessions?limit=abc", "Invalid pagination", 400},
		{"GET", "/sessions?limit=1&limit=2", "Invalid pagination", 400},
		{"GET", "/sessions?nextToken=", "Invalid pagination", 400},
		{"GET", "/sessions?other=1", "Invalid pagination", 400},
	}
	for _, tt := range tests {
		t.Run(tt.method+tt.suffix, func(t *testing.T) {
			response := httptest.NewRecorder()
			sc.mux.ServeHTTP(response, httptest.NewRequest(tt.method, "/kruise/api/v1alpha1/sandboxservings/serving-a"+tt.suffix, nil))
			require.Equal(t, tt.status, response.Code, response.Body.String())
			var body web.ApiError
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
			if tt.message != "" {
				require.Equal(t, tt.message, body.Message)
			}
			var fields map[string]any
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fields))
			require.NotContains(t, fields, "reason")
		})
	}
}

func TestServingListOptions(t *testing.T) {
	for _, tt := range []struct {
		query string
		limit int
		token string
	}{
		{query: "", limit: 0}, {query: "?limit=1", limit: 1}, {query: "?limit=100&nextToken=opaque", limit: 100, token: "opaque"},
	} {
		t.Run(tt.query, func(t *testing.T) {
			opts, apiErr := parseServingSessionList(httptest.NewRequest("GET", "/sessions"+tt.query, nil))
			require.Nil(t, apiErr)
			if tt.limit == 0 {
				require.Nil(t, opts.Limit)
			} else {
				require.Equal(t, tt.limit, *opts.Limit)
			}
			require.Equal(t, tt.token, opts.ContinueToken)
		})
	}
}

func TestServingErrorMapping(t *testing.T) {
	for _, tt := range []struct {
		code        managererrors.ErrorCode
		message     string
		status      int
		wantMessage string
	}{
		{managererrors.ErrorNotFound, "SandboxServing not found", 404, "SandboxServing not found"},
		{managererrors.ErrorNotFound, "Session not found", 404, "Session not found"},
		{managererrors.ErrorUnavailable, "Session manager is unavailable", 503, "Session manager is unavailable"},
		{managererrors.ErrorBadRequest, "Invalid session ID", 400, "Invalid session ID"},
		{managererrors.ErrorBadRequest, "Limit must be between 1 and 100", 400, "Limit must be between 1 and 100"},
		{managererrors.ErrorConflict, "Serving identity changed", 409, "Serving identity changed"},
		{managererrors.ErrorNotAllowed, "private ownership details", 404, "SandboxServing not found"},
		{managererrors.ErrorInternal, "private internal details", 500, "Serving operation failed"},
		{managererrors.ErrorUnknown, "private unknown details", 500, "Serving operation failed"},
	} {
		t.Run(string(tt.code)+tt.message, func(t *testing.T) {
			domainErr := managererrors.WrapError(tt.code, errors.New("private backend details"), "%s", tt.message)
			apiErr := servingAPIError(errors.Join(errors.New("wrapper"), domainErr))
			require.Equal(t, tt.status, apiErr.Code)
			require.Equal(t, tt.wantMessage, apiErr.Message)
			require.NotContains(t, apiErr.Message, "private")
			data, err := json.Marshal(apiErr)
			require.NoError(t, err)
			var fields map[string]any
			require.NoError(t, json.Unmarshal(data, &fields))
			require.NotContains(t, fields, "reason")
		})
	}
	apiErr := servingAPIError(errors.New("private backend details"))
	require.Equal(t, http.StatusInternalServerError, apiErr.Code)
	require.Equal(t, "Serving operation failed", apiErr.Message)
}

func TestServingUnauthenticatedHandlers(t *testing.T) {
	sc := NewController(ControllerOptions{})
	request := httptest.NewRequest("GET", "/", nil)
	_, getErr := sc.GetServingSession(request)
	_, listErr := sc.ListServingSessions(request)
	_, closeErr := sc.CloseServingSession(request)
	_, tokenErr := sc.RefreshServingTrafficAccessToken(request)
	for _, err := range []*web.ApiError{getErr, listErr, closeErr, tokenErr} {
		require.NotNil(t, err)
		require.Equal(t, 401, err.Code)
	}
	// A caller in the auth context is authenticated even before a Manager lookup.
	request = request.WithContext(context.WithValue(request.Context(), userContextKey, &models.CreatedTeamAPIKey{}))
	_, getErr = sc.GetServingSession(request)
	require.Equal(t, http.StatusBadRequest, getErr.Code)
	require.Equal(t, "Invalid Session ID", getErr.Message)
}
