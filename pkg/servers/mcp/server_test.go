/*
Copyright 2026 The Kruise Authors.

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

package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestMCPServerForRegisterTools creates an MCPServer for testing registerTools
func createTestMCPServerForRegisterTools() *MCPServer {
	config := DefaultServerConfig()
	operator := &mockSandboxOperator{}
	sm := NewSessionManager(operator, config, nil)
	sm.SetRequestPeer(noopRequestPeer)

	s := &MCPServer{
		config:         config,
		mcpServer:      server.NewMCPServer("test-server", "0.0.1", server.WithToolCapabilities(true)),
		sessionManager: sm,
		handler:        NewHandler(sm, nil, config),
	}

	return s
}

func TestRegisterTools(t *testing.T) {
	t.Run("registers all expected tools", func(t *testing.T) {
		s := createTestMCPServerForRegisterTools()

		// Call registerTools
		s.registerTools()

		// Get registered tools
		tools := s.mcpServer.ListTools()

		// Verify expected tool count
		assert.Len(t, tools, 3, "should register exactly 3 tools")

		// Verify each tool is registered
		expectedTools := []string{ToolRunCode, ToolRunCodeOnce, ToolRunCommand}
		for _, toolName := range expectedTools {
			tool, exists := tools[toolName]
			require.True(t, exists, "tool %s should be registered", toolName)
			assert.NotNil(t, tool, "tool %s should not be nil", toolName)
		}
	})
}

func TestRegisterRoutes(t *testing.T) {
	t.Run("health endpoint returns OK", func(t *testing.T) {
		s := createTestMCPServerForRegisterTools()
		s.mux = http.NewServeMux()
		s.auth = NewAuth(nil) // No authentication
		s.registerRoutes()

		req := httptest.NewRequest(http.MethodGet, "/health", nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
		assert.Equal(t, "OK", rec.Body.String())
	})
}

func TestRunAndStop(t *testing.T) {
	t.Run("server starts and stops successfully", func(t *testing.T) {
		config := DefaultServerConfig()
		config.Port = 0 // Use random available port

		operator := &mockSandboxOperator{}
		sm := NewSessionManager(operator, config, nil)
		sm.SetRequestPeer(noopRequestPeer)

		s := &MCPServer{
			config:         config,
			mux:            http.NewServeMux(),
			mcpServer:      server.NewMCPServer("test-server", "0.0.1", server.WithToolCapabilities(true)),
			sessionManager: sm,
			auth:           NewAuth(nil),
			handler:        NewHandler(sm, nil, config),
		}

		s.httpServer = &http.Server{
			Addr:              ":0", // Use any available port
			Handler:           s.mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		s.registerRoutes()

		ctx := context.Background()

		// Start server
		err := s.Run(ctx)
		require.NoError(t, err)

		// Give server time to start
		time.Sleep(50 * time.Millisecond)

		// Stop server
		stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err = s.Stop(stopCtx)
		assert.NoError(t, err)
	})

	t.Run("stop handles shutdown timeout gracefully", func(t *testing.T) {
		config := DefaultServerConfig()
		config.Port = 0

		operator := &mockSandboxOperator{}
		sm := NewSessionManager(operator, config, nil)
		sm.SetRequestPeer(noopRequestPeer)

		s := &MCPServer{
			config:         config,
			mux:            http.NewServeMux(),
			mcpServer:      server.NewMCPServer("test-server", "0.0.1"),
			sessionManager: sm,
			auth:           NewAuth(nil),
			handler:        NewHandler(sm, nil, config),
		}

		s.httpServer = &http.Server{
			Addr:              ":0",
			Handler:           s.mux,
			ReadHeaderTimeout: 5 * time.Second,
		}
		s.registerRoutes()

		ctx := context.Background()

		// Start server
		err := s.Run(ctx)
		require.NoError(t, err)

		time.Sleep(50 * time.Millisecond)

		// Stop with very short timeout
		stopCtx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()

		// Should complete without error for a server with no active connections
		err = s.Stop(stopCtx)
		assert.NoError(t, err)
	})
}
