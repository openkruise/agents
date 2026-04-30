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
	"testing"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockClientSession is a mock implementation of server.ClientSession for testing
type mockClientSession struct {
	sessionID           string
	initialized         bool
	notificationChannel chan mcpgo.JSONRPCNotification
}

func newMockClientSession(sessionID string) *mockClientSession {
	return &mockClientSession{
		sessionID:           sessionID,
		initialized:         true,
		notificationChannel: make(chan mcpgo.JSONRPCNotification, 10),
	}
}

func (m *mockClientSession) Initialize() {
	m.initialized = true
}

func (m *mockClientSession) Initialized() bool {
	return m.initialized
}

func (m *mockClientSession) NotificationChannel() chan<- mcpgo.JSONRPCNotification {
	return m.notificationChannel
}

func (m *mockClientSession) SessionID() string {
	return m.sessionID
}

// createTestMCPServer creates a minimal MCPServer for testing middlewares
func createTestMCPServer() *MCPServer {
	config := DefaultServerConfig()
	return &MCPServer{
		config:    config,
		mcpServer: server.NewMCPServer("test-server", "0.0.1"),
	}
}

// setClientSessionInContext uses the mcp-go server's WithContext to set a ClientSession
func setClientSessionInContext(ctx context.Context, mcpSrv *server.MCPServer, session server.ClientSession) context.Context {
	return mcpSrv.WithContext(ctx, session)
}

// TestSessionManagementMiddleware tests the session management middleware
func TestSessionManagementMiddleware(t *testing.T) {
	mcpServer := createTestMCPServer()

	t.Run("with valid session ID", func(t *testing.T) {
		mockSession := newMockClientSession("test-session-123")
		ctx := setClientSessionInContext(context.Background(), mcpServer.mcpServer, mockSession)

		handlerCalled := false
		expectedResult := &mcpgo.CallToolResult{
			Content: []mcpgo.Content{mcpgo.NewTextContent("success")},
		}

		handler := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			handlerCalled = true
			return expectedResult, nil
		}

		wrappedHandler := mcpServer.SessionManagementMiddleware(handler)
		result, err := wrappedHandler(ctx, mcpgo.CallToolRequest{
			Params: mcpgo.CallToolParams{Name: "test_tool"},
		})

		assert.NoError(t, err)
		assert.True(t, handlerCalled)
		assert.Equal(t, expectedResult, result)
	})

	t.Run("with empty session ID", func(t *testing.T) {
		// Create mock session with empty session ID
		mockSession := newMockClientSession("")
		ctx := setClientSessionInContext(context.Background(), mcpServer.mcpServer, mockSession)

		handlerCalled := false
		handler := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			handlerCalled = true
			return nil, nil
		}

		wrappedHandler := mcpServer.SessionManagementMiddleware(handler)
		result, err := wrappedHandler(ctx, mcpgo.CallToolRequest{
			Params: mcpgo.CallToolParams{Name: "test_tool"},
		})

		assert.Error(t, err)
		assert.False(t, handlerCalled)
		assert.Nil(t, result)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeAuthFailed, err.(*MCPError).Code)
		assert.Contains(t, err.Error(), "Session ID is required")
	})
}

// TestLoggingMiddleware tests the logging middleware
func TestLoggingMiddleware(t *testing.T) {
	mcpServer := createTestMCPServer()

	t.Run("logs successful tool call", func(t *testing.T) {
		mockSession := newMockClientSession("test-session-123")
		ctx := setClientSessionInContext(context.Background(), mcpServer.mcpServer, mockSession)

		// Set user in context
		testUser := &models.CreatedTeamAPIKey{
			ID:   uuid.New(),
			Name: "test-user",
		}
		ctx = SetUserContext(ctx, testUser)

		expectedResult := &mcpgo.CallToolResult{
			Content: []mcpgo.Content{mcpgo.NewTextContent("success")},
		}

		handler := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			return expectedResult, nil
		}

		wrappedHandler := mcpServer.LoggingMiddleware(handler)
		result, err := wrappedHandler(ctx, mcpgo.CallToolRequest{
			Params: mcpgo.CallToolParams{
				Name:      "test_tool",
				Arguments: map[string]any{"arg1": "value1"},
			},
		})

		assert.NoError(t, err)
		assert.Equal(t, expectedResult, result)
	})
}

// TestConfigMiddleware tests the config middleware
func TestConfigMiddleware(t *testing.T) {
	mcpServer := createTestMCPServer()

	t.Run("parses all headers correctly", func(t *testing.T) {
		ctx := context.Background()

		var capturedConfig *UserSessionConfig
		handler := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			capturedConfig = GetUserSessionConfig(ctx)
			return &mcpgo.CallToolResult{}, nil
		}

		headers := http.Header{}
		headers.Set("X-Template", "custom-template")
		headers.Set("X-Sandbox-TTL", "300")       // 300 seconds = 5 minutes
		headers.Set("X-Execution-Timeout", "120") // 120 seconds = 2 minutes

		wrappedHandler := mcpServer.ConfigMiddleware(handler)
		_, err := wrappedHandler(ctx, mcpgo.CallToolRequest{
			Header: headers,
			Params: mcpgo.CallToolParams{Name: "test_tool"},
		})

		require.NoError(t, err)
		require.NotNil(t, capturedConfig)
		assert.Equal(t, "custom-template", capturedConfig.Template)
		assert.NotNil(t, capturedConfig.SandboxTTL)
		assert.Equal(t, 5*time.Minute, *capturedConfig.SandboxTTL)
		assert.NotNil(t, capturedConfig.ExecutionTimeout)
		assert.Equal(t, 2*time.Minute, *capturedConfig.ExecutionTimeout)
	})

	t.Run("handles empty headers", func(t *testing.T) {
		ctx := context.Background()

		var capturedConfig *UserSessionConfig
		handler := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			capturedConfig = GetUserSessionConfig(ctx)
			return &mcpgo.CallToolResult{}, nil
		}

		wrappedHandler := mcpServer.ConfigMiddleware(handler)
		_, err := wrappedHandler(ctx, mcpgo.CallToolRequest{
			Header: http.Header{},
			Params: mcpgo.CallToolParams{Name: "test_tool"},
		})

		require.NoError(t, err)
		require.NotNil(t, capturedConfig)
		assert.Empty(t, capturedConfig.Template)
		assert.Nil(t, capturedConfig.SandboxTTL)
		assert.Nil(t, capturedConfig.ExecutionTimeout)
	})
}

// TestApplyMiddlewares tests the middleware chain
func TestApplyMiddlewares(t *testing.T) {
	mcpServer := createTestMCPServer()
	t.Run("session validation fails before handler is called", func(t *testing.T) {
		// Context without session
		ctx := context.Background()

		handlerCalled := false
		handler := func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
			handlerCalled = true
			return &mcpgo.CallToolResult{}, nil
		}

		wrappedHandler := mcpServer.applyMiddlewares(handler)
		result, err := wrappedHandler(ctx, mcpgo.CallToolRequest{
			Params: mcpgo.CallToolParams{Name: "test_tool"},
		})

		assert.Error(t, err)
		assert.False(t, handlerCalled)
		assert.Nil(t, result)
	})
}
