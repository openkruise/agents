package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
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
			assert.Equal(t, tt.expectedStatusCode, w.Code, "Status code mismatch")

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
