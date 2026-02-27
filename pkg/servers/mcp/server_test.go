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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mark3labs/mcp-go/server"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestMCPServerForRegisterTools creates an MCPServer for testing registerTools
func createTestMCPServerForRegisterTools() *MCPServer {
	config := DefaultServerConfig()
	deps := &mockSessionDeps{}
	sm := NewSessionManager(deps, config)

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

func TestInitPeers(t *testing.T) {
	t.Run("sets peers from IP list", func(t *testing.T) {
		s := createTestMCPServerForRegisterTools()

		ips := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"}
		s.InitPeers(ips)

		s.sessionManager.peerMu.RLock()
		defer s.sessionManager.peerMu.RUnlock()

		assert.Len(t, s.sessionManager.peers, 3)
		for _, ip := range ips {
			_, exists := s.sessionManager.peers[ip]
			assert.True(t, exists, "peer %s should be registered", ip)
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

	t.Run("MCP endpoint is registered", func(t *testing.T) {
		s := createTestMCPServerForRegisterTools()
		s.mux = http.NewServeMux()
		s.auth = NewAuth(nil)
		s.registerRoutes()

		req := httptest.NewRequest(http.MethodGet, s.config.MCPEndpointPath, nil)
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)

		// Should not be 404, meaning endpoint is registered
		assert.NotEqual(t, http.StatusNotFound, rec.Code)
	})
}
