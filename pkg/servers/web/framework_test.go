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

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/openkruise/agents/pkg/tracing"
)

func TestRegisterRoute(t *testing.T) {
	helloHandler := func(r *http.Request) (ApiResponse[string], *ApiError) {
		return ApiResponse[string]{
			Code: http.StatusOK,
			Body: "Hello",
		}, nil
	}
	helloChecker := func(t *testing.T, body string, err ApiError) {
		assert.Equal(t, "Hello", body)
	}
	authHeader := "Bearer token123"
	tests := []struct {
		name               string
		method             string
		path               string
		requestMethod      string
		requestPath        string
		expectedStatusCode int
		checkBody          func(t *testing.T, body string, err ApiError)
		handler            Handler[string]
		middlewares        []MiddleWare
	}{
		{
			name:               "Simple GET route",
			method:             "GET",
			path:               "/test",
			requestMethod:      "GET",
			requestPath:        "/test",
			expectedStatusCode: http.StatusOK,
			handler:            helloHandler,
			checkBody:          helloChecker,
		},
		{
			name:               "POST route with data",
			method:             "POST",
			path:               "/api/data",
			requestMethod:      "POST",
			requestPath:        "/api/data",
			expectedStatusCode: http.StatusOK,
			handler:            helloHandler,
			checkBody:          helloChecker,
		},
		{
			name:               "Route not found - mismatch path",
			method:             "GET",
			path:               "/test",
			requestMethod:      "GET",
			requestPath:        "/nonexistent",
			expectedStatusCode: http.StatusNotFound,
			handler:            helloHandler,
		},
		{
			name:               "Route not found - too many slashes",
			method:             "GET",
			path:               "/test",
			requestMethod:      "GET",
			requestPath:        "/test//",
			expectedStatusCode: http.StatusMovedPermanently,
			handler:            helloHandler,
		},
		{
			name:               "Route not found - mismatch method",
			method:             "POST",
			path:               "/test",
			requestMethod:      "GET",
			requestPath:        "/test",
			expectedStatusCode: http.StatusMethodNotAllowed,
			handler:            helloHandler,
		},
		{
			name:               "Route with middleware",
			method:             "GET",
			path:               "/protected",
			requestMethod:      "GET",
			requestPath:        "/protected",
			expectedStatusCode: http.StatusOK,
			handler:            helloHandler,
			middlewares: []MiddleWare{
				func(ctx context.Context, r *http.Request) (context.Context, *ApiError) {
					// Simple auth middleware for test
					auth := r.Header.Get("Authorization")
					if auth != authHeader {
						return ctx, &ApiError{
							Code:    http.StatusUnauthorized,
							Message: "Unauthorized",
						}
					}
					return ctx, nil
				},
			},
			checkBody: helloChecker,
		},
		{
			name:               "Route with failing middleware",
			method:             "GET",
			path:               "/protected",
			requestMethod:      "GET",
			requestPath:        "/protected",
			expectedStatusCode: http.StatusUnauthorized,
			handler:            helloHandler,
			middlewares: []MiddleWare{
				func(ctx context.Context, r *http.Request) (context.Context, *ApiError) {
					// Simple auth middleware for test
					auth := r.Header.Get("Authorization")
					if auth != "another auth header" {
						return ctx, &ApiError{
							Code:    http.StatusUnauthorized,
							Message: "Unauthorized",
						}
					}
					return ctx, nil
				},
			},
			checkBody: func(t *testing.T, body string, err ApiError) {
				assert.Equal(t, http.StatusUnauthorized, err.Code)
				assert.Equal(t, "Unauthorized", err.Message)
			},
		},
		{
			name:               "Route with trailing slash",
			method:             "GET",
			path:               "/trailing",
			requestMethod:      "GET",
			requestPath:        "/trailing/",
			expectedStatusCode: http.StatusOK,
			handler:            helloHandler,
			checkBody:          helloChecker,
		},
		{
			name:               "Route without trailing slash",
			method:             "GET",
			path:               "/trailing/",
			requestMethod:      "GET",
			requestPath:        "/trailing",
			expectedStatusCode: http.StatusOK,
			handler:            helloHandler,
			checkBody:          helloChecker,
		},
		{
			name:               "panics",
			method:             "GET",
			path:               "/panic",
			requestMethod:      "GET",
			requestPath:        "/panic/",
			expectedStatusCode: http.StatusInternalServerError,
			handler: func(r *http.Request) (ApiResponse[string], *ApiError) {
				panic("test panic")
			},
			checkBody: func(t *testing.T, body string, err ApiError) {
				assert.Equal(t, http.StatusInternalServerError, err.Code)
				assert.Equal(t, "Internal Server Error", err.Message)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a new ServeMux for each test
			mux := http.NewServeMux()

			// Register the route
			RegisterRoute(mux, tt.method, tt.path, tt.handler, tt.middlewares...)

			// Create a test request
			req := httptest.NewRequest(tt.requestMethod, tt.requestPath, nil)

			// Add authorization header for middleware tests
			if tt.name == "Route with middleware" {
				req.Header.Set("Authorization", authHeader)
			}

			// Create a ResponseRecorder to record the response
			w := httptest.NewRecorder()

			// Serve the request
			mux.ServeHTTP(w, req)

			// Check the status code
			if tt.name == "Route not found - too many slashes" {
				assert.Contains(t, []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}, w.Code, "Status code mismatch for too many slashes")
			} else {
				assert.Equal(t, tt.expectedStatusCode, w.Code, "Status code mismatch")
			}

			// For 2xx responses, check the body
			if tt.expectedStatusCode >= 200 && tt.expectedStatusCode < 300 {
				body := w.Body.Bytes()
				bodyStr := string(body)
				bodyStr = strings.Trim(bodyStr, "\n")
				bodyStr = strings.Trim(bodyStr, "\"")
				tt.checkBody(t, bodyStr, ApiError{})
			}
			if tt.expectedStatusCode >= 500 {
				var err ApiError
				assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &err))
				tt.checkBody(t, "", err)
			}
		})
	}
}

// requestIDPattern matches the representation required by the tracing scheme:
// 32 lowercase hex characters, directly usable as an OTel TraceID.
var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// TestRequestIDHandling verifies the X-Request-ID contract of RegisterRoute:
// caller-provided IDs are never rewritten (not even case-normalized, so a
// caller can always grep logs for the exact value it sent); absent IDs are
// generated directly in the tracing representation; when tracing is enabled,
// IDs unusable as an OTel TraceID are rejected with 400 instead of being
// silently replaced.
func TestRequestIDHandling(t *testing.T) {
	okHandler := func(r *http.Request) (ApiResponse[string], *ApiError) {
		return ApiResponse[string]{Code: http.StatusOK, Body: "ok"}, nil
	}
	validID := "0123456789abcdef0123456789abcdef"
	allZeroID := strings.Repeat("0", 32)

	tests := []struct {
		name           string
		tracingEnabled bool
		requestID      string // "" means the header is absent
		expectedStatus int
		// expectedHeaderID is the exact X-Request-ID expected on the response;
		// empty means a server-generated ID matching requestIDPattern is expected.
		expectedHeaderID string
	}{
		{
			name:           "absent ID is generated in tracing representation",
			tracingEnabled: false,
			requestID:      "",
			expectedStatus: http.StatusOK,
		},
		{
			name:             "arbitrary ID passes through untouched when tracing disabled",
			tracingEnabled:   false,
			requestID:        "my-custom-request-id",
			expectedStatus:   http.StatusOK,
			expectedHeaderID: "my-custom-request-id",
		},
		{
			name:             "all-zero ID passes through untouched when tracing disabled",
			tracingEnabled:   false,
			requestID:        allZeroID,
			expectedStatus:   http.StatusOK,
			expectedHeaderID: allZeroID,
		},
		{
			name:             "uppercase hex ID passes through untouched when tracing disabled",
			tracingEnabled:   false,
			requestID:        strings.ToUpper(validID),
			expectedStatus:   http.StatusOK,
			expectedHeaderID: strings.ToUpper(validID),
		},
		{
			name:           "absent ID is generated when tracing enabled",
			tracingEnabled: true,
			requestID:      "",
			expectedStatus: http.StatusOK,
		},
		{
			name:             "valid ID passes through untouched when tracing enabled",
			tracingEnabled:   true,
			requestID:        validID,
			expectedStatus:   http.StatusOK,
			expectedHeaderID: validID,
		},
		{
			name:             "uppercase hex ID passes through untouched when tracing enabled",
			tracingEnabled:   true,
			requestID:        strings.ToUpper(validID),
			expectedStatus:   http.StatusOK,
			expectedHeaderID: strings.ToUpper(validID),
		},
		{
			name:           "invalid ID rejected with 400 when tracing enabled",
			tracingEnabled: true,
			requestID:      "not-a-trace-id",
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "UUID with hyphens rejected with 400 when tracing enabled",
			tracingEnabled: true,
			requestID:      "01234567-89ab-cdef-0123-456789abcdef",
			expectedStatus: http.StatusBadRequest,
		},
		{
			name:           "all-zero ID rejected with 400 when tracing enabled",
			tracingEnabled: true,
			requestID:      allZeroID,
			expectedStatus: http.StatusBadRequest,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Install the tracing state for this case. Mode "none" resets to a
			// noop provider (tracing.Enabled() == false); mode "file" installs
			// a real provider writing to a temp file.
			cfg := tracing.Config{Mode: tracing.TracingModeNone, ServiceName: "framework-test"}
			if tt.tracingEnabled {
				cfg = tracing.Config{
					Mode:          tracing.TracingModeFile,
					FilePath:      filepath.Join(t.TempDir(), "traces.json"),
					ServiceName:   "framework-test",
					SamplingRatio: 1.0,
				}
			}
			shutdown, err := tracing.InitTracerProvider(context.Background(), cfg)
			require.NoError(t, err)
			defer func() { _ = shutdown(context.Background()) }()

			mux := http.NewServeMux()
			RegisterRoute(mux, "GET", "/ping", okHandler)

			req := httptest.NewRequest("GET", "/ping", nil)
			if tt.requestID != "" {
				req.Header.Set("X-Request-ID", tt.requestID)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedStatus, w.Code, "status code mismatch")

			switch {
			case tt.expectedStatus == http.StatusOK && tt.expectedHeaderID != "":
				assert.Equal(t, tt.expectedHeaderID, w.Header().Get("X-Request-ID"),
					"caller-provided X-Request-ID must never be rewritten")
			case tt.expectedStatus == http.StatusOK:
				assert.Regexp(t, requestIDPattern, w.Header().Get("X-Request-ID"),
					"server-generated X-Request-ID must be 32 lowercase hex chars")
			default:
				// Rejected requests echo the offending ID in the error body.
				var apiErr ApiError
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &apiErr))
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Contains(t, apiErr.Message, "invalid X-Request-ID")
				assert.Equal(t, tt.requestID, apiErr.RequestID,
					"error body should carry the original request ID")
			}
		})
	}
}
