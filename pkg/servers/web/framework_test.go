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
// silently replaced. Success, handler errors, middleware errors, and panic
// recovery all return that same ID in the X-Request-ID header, and error
// bodies carry it in request_id. Headers set on ApiError are preserved.
func TestRequestIDHandling(t *testing.T) {
	okHandler := func(r *http.Request) (ApiResponse[string], *ApiError) {
		return ApiResponse[string]{Code: http.StatusOK, Body: "ok"}, nil
	}
	notFound := func(headers map[string]string) Handler[string] {
		return func(r *http.Request) (ApiResponse[string], *ApiError) {
			return ApiResponse[string]{}, &ApiError{
				Code:    http.StatusNotFound,
				Message: "missing",
				Headers: headers,
			}
		}
	}
	unauthorized := func(headers map[string]string) MiddleWare {
		return func(ctx context.Context, r *http.Request) (context.Context, *ApiError) {
			return ctx, &ApiError{
				Code:    http.StatusUnauthorized,
				Message: "unauthorized",
				Headers: headers,
			}
		}
	}
	panicHandler := func(r *http.Request) (ApiResponse[string], *ApiError) {
		panic("test panic")
	}
	validID := "0123456789abcdef0123456789abcdef"
	allZeroID := strings.Repeat("0", 32)
	customHeader := map[string]string{"X-Custom": "keep"}

	tests := []struct {
		name           string
		tracingEnabled bool
		requestID      string // "" means the header is absent
		expectedStatus int
		// expectedHeaderID is the exact X-Request-ID expected on the response;
		// empty means a server-generated ID matching requestIDPattern is expected.
		expectedHeaderID string
		handler          Handler[string]
		middlewares      []MiddleWare
		// expectErrorBody requires a JSON ApiError whose request_id equals the
		// X-Request-ID response header.
		expectErrorBody bool
		// invalidRequestID marks tracing-enabled rejection of an unusable ID.
		invalidRequestID bool
		// extraHeaders must be present on the response in addition to X-Request-ID.
		extraHeaders map[string]string
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
			name:             "invalid ID rejected with 400 when tracing enabled",
			tracingEnabled:   true,
			requestID:        "not-a-trace-id",
			expectedStatus:   http.StatusBadRequest,
			expectedHeaderID: "not-a-trace-id",
			invalidRequestID: true,
		},
		{
			name:             "UUID with hyphens rejected with 400 when tracing enabled",
			tracingEnabled:   true,
			requestID:        "01234567-89ab-cdef-0123-456789abcdef",
			expectedStatus:   http.StatusBadRequest,
			expectedHeaderID: "01234567-89ab-cdef-0123-456789abcdef",
			invalidRequestID: true,
		},
		{
			name:             "all-zero ID rejected with 400 when tracing enabled",
			tracingEnabled:   true,
			requestID:        allZeroID,
			expectedStatus:   http.StatusBadRequest,
			expectedHeaderID: allZeroID,
			invalidRequestID: true,
		},
		{
			name:             "handler error carries caller request ID in header and body",
			requestID:        "my-custom-request-id",
			expectedStatus:   http.StatusNotFound,
			expectedHeaderID: "my-custom-request-id",
			handler:          notFound(nil),
			expectErrorBody:  true,
		},
		{
			name:            "handler error carries generated request ID in header and body",
			expectedStatus:  http.StatusNotFound,
			handler:         notFound(nil),
			expectErrorBody: true,
		},
		{
			name:             "handler error keeps ApiError headers and sets request ID",
			requestID:        validID,
			expectedStatus:   http.StatusNotFound,
			expectedHeaderID: validID,
			handler:          notFound(customHeader),
			expectErrorBody:  true,
			extraHeaders:     customHeader,
		},
		{
			name:             "middleware error carries caller request ID in header and body",
			requestID:        "my-custom-request-id",
			expectedStatus:   http.StatusUnauthorized,
			expectedHeaderID: "my-custom-request-id",
			middlewares:      []MiddleWare{unauthorized(nil)},
			expectErrorBody:  true,
		},
		{
			name:             "middleware error keeps ApiError headers and sets request ID",
			requestID:        validID,
			expectedStatus:   http.StatusUnauthorized,
			expectedHeaderID: validID,
			middlewares:      []MiddleWare{unauthorized(customHeader)},
			expectErrorBody:  true,
			extraHeaders:     customHeader,
		},
		{
			name:             "panic recovery carries caller request ID in header and body",
			requestID:        "my-custom-request-id",
			expectedStatus:   http.StatusInternalServerError,
			expectedHeaderID: "my-custom-request-id",
			handler:          panicHandler,
			expectErrorBody:  true,
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
			handler := tt.handler
			if handler == nil {
				handler = okHandler
			}
			RegisterRoute(mux, "GET", "/ping", handler, tt.middlewares...)

			req := httptest.NewRequest("GET", "/ping", nil)
			if tt.requestID != "" {
				req.Header.Set("X-Request-ID", tt.requestID)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			assert.Equal(t, tt.expectedStatus, w.Code, "status code mismatch")

			headerID := w.Header().Get("X-Request-ID")
			if tt.expectedHeaderID != "" {
				assert.Equal(t, tt.expectedHeaderID, headerID,
					"caller-provided X-Request-ID must never be rewritten")
			} else {
				assert.Regexp(t, requestIDPattern, headerID,
					"server-generated X-Request-ID must be 32 lowercase hex chars")
			}
			for k, v := range tt.extraHeaders {
				assert.Equal(t, v, w.Header().Get(k), "ApiError headers must be preserved")
			}

			if tt.invalidRequestID {
				// Rejected requests echo the offending ID in the error body.
				var apiErr ApiError
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &apiErr))
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Contains(t, apiErr.Message, "invalid X-Request-ID")
				assert.Equal(t, tt.requestID, apiErr.RequestID,
					"error body should carry the original request ID")
				assert.Equal(t, headerID, apiErr.RequestID,
					"error body request_id must match the X-Request-ID header")
			}
			if tt.expectErrorBody {
				var apiErr ApiError
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &apiErr))
				assert.Equal(t, tt.expectedStatus, apiErr.Code)
				assert.Equal(t, headerID, apiErr.RequestID,
					"error body request_id must match the X-Request-ID header")
				if tt.expectedHeaderID == "" {
					assert.Regexp(t, requestIDPattern, apiErr.RequestID,
						"generated error request_id must be 32 lowercase hex chars")
				}
			}
		})
	}
}
