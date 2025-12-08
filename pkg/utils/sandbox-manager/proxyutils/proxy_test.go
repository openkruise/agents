package proxyutils

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

//goland:noinspection DuplicatedCode
func TestSandbox_ProxyRequest(t *testing.T) {
	// Create a test HTTP server
	server := http.NewServeMux()
	server.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent) // Return 204 status code
	})

	// Add a handler for a specific path, for testing path forwarding
	server.HandleFunc("/test-path", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("test response"))
	})

	// Add a handler that returns an error status code
	server.HandleFunc("/error", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("internal server error"))
	})

	// Start HTTP server listening on local port
	listener, err := net.Listen("tcp", "127.0.0.1:11111")
	require.NoError(t, err)

	httpServer := &http.Server{Handler: server}
	go func() {
		_ = httpServer.Serve(listener)
	}()

	// Ensure server has started
	time.Sleep(100 * time.Millisecond)

	defer func() {
		_ = httpServer.Shutdown(context.Background())
	}()

	tests := []struct {
		name            string
		path            string
		ip              string
		wantErr         bool
		wantStatus      int
		wantErrContains string
	}{
		{
			name:       "valid pod IP",
			path:       "/",
			ip:         "127.0.0.1",
			wantErr:    false,
			wantStatus: http.StatusNoContent,
		},
		{
			name:       "specific path",
			path:       "/test-path",
			ip:         "127.0.0.1",
			wantErr:    false,
			wantStatus: http.StatusOK,
		},
		{
			name:            "error response",
			path:            "/error",
			ip:              "127.0.0.1",
			wantErr:         true, // Should return error because status code is 5xx
			wantErrContains: "internal server error",
		},
		{
			name:            "unreachable server",
			path:            "/",
			ip:              "192.168.100.100", // Use an unreachable IP address
			wantErr:         true,              // Should return error when server is unreachable
			wantErrContains: "failed to proxy request to sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test request
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:11111"+tt.path, nil)
			require.NoError(t, err)
			req.Header.Set("Content-Type", "application/json")

			// Call ProxyRequest method
			resp, err := ProxyRequest(req, tt.path, 11111, tt.ip)

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
