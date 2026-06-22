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

package proxyutils

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func NewTestServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.WriteHeader(http.StatusNoContent) // Return 204 status code
		case "/test-path":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("test response"))
		case "/error":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal server error"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

//goland:noinspection DuplicatedCode
func TestProxyRequest(t *testing.T) {
	// Create test servers using httptest
	testServer := NewTestServer()
	defer testServer.Close()

	tests := []struct {
		name            string
		path            string
		url             string
		wantErr         bool
		wantStatus      int
		wantErrContains string
	}{
		{
			name:       "valid server URL",
			path:       "/",
			url:        testServer.URL,
			wantErr:    false,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "specific path",
			path:       "/test-path",
			url:        testServer.URL,
			wantErr:    false,
			wantStatus: http.StatusOK,
		},
		{
			name:            "error response",
			path:            "/error",
			url:             testServer.URL,
			wantErr:         true, // Should return error because status code is 5xx
			wantErrContains: "internal server error",
		},
		{
			name:            "unreachable server",
			path:            "/",
			url:             "http://192.168.100.100:8080", // Use an unreachable URL
			wantErr:         true,                          // Should return error when server is unreachable
			wantErrContains: "failed to proxy request to sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test request
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s%s", tt.url, tt.path), nil)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			// Call ProxyRequest method
			resp, err := ProxyRequest(req)

			// Check errors
			if tt.wantErr {
				assert.Error(t, err)
				if tt.wantErrContains != "" {
					assert.True(t, strings.Contains(err.Error(), tt.wantErrContains),
						"Expected error to contain '%s', but got '%s'", tt.wantErrContains, err.Error())
				}
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, resp)
			assert.Equal(t, tt.wantStatus, resp.StatusCode)

			// Close response body
			if resp.Body != nil {
				_ = resp.Body.Close()
			}
		})
	}
}
