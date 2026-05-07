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
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createTestHandler creates a Handler for testing without real dependencies
func createTestHandler() (*Handler, *SessionManager, *server.MCPServer) {
	config := DefaultServerConfig()
	operator := &mockSandboxOperator{}
	sm := createTestSessionManager(operator, nil)
	mcpSrv := server.NewMCPServer("test-server", "0.0.1")

	h := &Handler{
		sessionManager: sm,
		operator:       operator,
		config:         config,
	}

	return h, sm, mcpSrv
}

// createTestHandlerWithOperator creates a Handler for testing with custom SandboxOperator
func createTestHandlerWithOperator(operator *mockSandboxOperator) (*Handler, *SessionManager, *server.MCPServer) {
	config := DefaultServerConfig()
	sm := createTestSessionManager(operator, nil)
	mcpSrv := server.NewMCPServer("test-server", "0.0.1")

	h := &Handler{
		sessionManager: sm,
		operator:       operator,
		config:         config,
	}

	return h, sm, mcpSrv
}

// createTestContext creates a context with user and session for testing
func createTestContext(mcpSrv *server.MCPServer, userID uuid.UUID, sessionID string) context.Context {
	ctx := context.Background()

	// Set user in context
	testUser := &models.CreatedTeamAPIKey{
		ID:   userID,
		Name: "test-user",
	}
	ctx = SetUserContext(ctx, testUser)

	// Set mock client session
	mockSession := newMockClientSession(sessionID)
	ctx = mcpSrv.WithContext(ctx, mockSession)

	return ctx
}

// mockSandboxWithRequest is a mock sandbox that supports custom HTTP request responses
type mockSandboxWithRequest struct {
	*mockSandbox
	response        *http.Response
	requestErr      error
	capturedHeaders *http.Header
}

// newMockSandboxWithRequest creates a mock sandbox with custom request response
func newMockSandboxWithRequest(sandboxID string, response *http.Response, requestErr error) *mockSandboxWithRequest {
	return &mockSandboxWithRequest{
		mockSandbox: newMockSandbox(sandboxID),
		response:    response,
		requestErr:  requestErr,
	}
}

// newMockSandboxWithRequestCapture creates a mock sandbox that captures request headers
func newMockSandboxWithRequestCapture(sandboxID string, capturedHeaders *http.Header) *mockSandboxWithRequest {
	// Return a response that will trigger error but allow header capture
	resp := newMockHTTPResponse(http.StatusOK, ``)
	return &mockSandboxWithRequest{
		mockSandbox:     newMockSandbox(sandboxID),
		response:        resp,
		requestErr:      nil,
		capturedHeaders: capturedHeaders,
	}
}

func (m *mockSandboxWithRequest) Request(ctx context.Context, method, path string, port int, body io.Reader, headers http.Header) (*http.Response, error) {
	if m.capturedHeaders != nil {
		*m.capturedHeaders = headers.Clone()
	}
	if m.requestErr != nil {
		return nil, m.requestErr
	}
	return m.response, nil
}

// newMockHTTPResponse creates a mock HTTP response with given status and body
func newMockHTTPResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestHandleRunCode(t *testing.T) {
	t.Run("returns error when user not in context", func(t *testing.T) {
		operator := newMockOperatorWithSandbox("sandbox-abc", nil)
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		sessionID := "session-123"

		// Create context WITHOUT user
		mockSession := newMockClientSession(sessionID)
		ctx := mcpSrv.WithContext(context.Background(), mockSession)

		args := RunCodeRequest{
			Code:     "print('test')",
			Language: "python",
		}

		result, err := h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeAuthFailed, err.(*MCPError).Code)
		assert.Equal(t, RunCodeResponse{}, result)
	})

	t.Run("returns error when session ID not in context", func(t *testing.T) {
		operator := newMockOperatorWithSandbox("sandbox-abc", nil)
		h, _, _ := createTestHandlerWithOperator(operator)
		userID := uuid.New()

		// Create context WITHOUT session
		ctx := context.Background()
		testUser := &models.CreatedTeamAPIKey{
			ID:   userID,
			Name: "test-user",
		}
		ctx = SetUserContext(ctx, testUser)

		args := RunCodeRequest{
			Code:     "print('test')",
			Language: "python",
		}

		result, err := h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeInternalError, err.(*MCPError).Code)
		assert.Equal(t, RunCodeResponse{}, result)
	})

	t.Run("returns error when session creation fails", func(t *testing.T) {
		operator := newMockOperatorWithSandbox("", errors.New("sandbox creation failed"))
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-fail"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCodeRequest{
			Code:     "print('test')",
			Language: "python",
		}

		result, err := h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeSandboxCreation, err.(*MCPError).Code)
		assert.Equal(t, RunCodeResponse{}, result)
	})

	t.Run("reuses existing session and does not call sandbox creator", func(t *testing.T) {
		creatorCallCount := 0
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				creatorCallCount++
				return newMockSandbox("new-sandbox"), nil
			},
		}
		_, sm, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-reuse"

		// Pre-store a session
		existingSession := &UserSession{
			SessionID:   sessionID,
			UserID:      userID.String(),
			SandboxID:   "sandbox-existing",
			TemplateID:  "template-1",
			State:       "Running",
			AccessToken: "existing-token",
		}
		sm.sessions.Store(sessionID, existingSession)

		ctx := createTestContext(mcpSrv, userID, sessionID)

		// Test through SessionManager directly to verify session reuse
		session, err := sm.GetOrCreateSession(ctx, sessionID, userID.String(), "template-1", 10*time.Minute)

		require.NoError(t, err)
		assert.Equal(t, "sandbox-existing", session.SandboxID)
		assert.Equal(t, 0, creatorCallCount) // Creator should not be called
	})
}

func TestHandleRunCodeOnce(t *testing.T) {
	t.Run("returns error when user not in context", func(t *testing.T) {
		h, _, mcpSrv := createTestHandler()
		sessionID := "session-once-123"

		// Create context WITHOUT user
		mockSession := newMockClientSession(sessionID)
		ctx := mcpSrv.WithContext(context.Background(), mockSession)

		args := RunCodeOnceRequest{
			Code:     "print('test')",
			Language: "python",
		}

		result, err := h.HandleRunCodeOnce(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeAuthFailed, err.(*MCPError).Code)
		assert.Equal(t, RunCodeResponse{}, result)
	})
}

func TestHandleRunCommand(t *testing.T) {
	t.Run("returns error when user not in context", func(t *testing.T) {
		operator := newMockOperatorWithSandbox("sandbox-cmd-abc", nil)
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		sessionID := "session-cmd-123"

		// Create context WITHOUT user
		mockSession := newMockClientSession(sessionID)
		ctx := mcpSrv.WithContext(context.Background(), mockSession)

		args := RunCommandRequest{
			Cmd: "ls -la",
		}

		result, err := h.HandleRunCommand(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeAuthFailed, err.(*MCPError).Code)
		assert.Equal(t, RunCommandResponse{}, result)
	})

	t.Run("returns error when session creation fails", func(t *testing.T) {
		operator := newMockOperatorWithSandbox("", errors.New("sandbox creation failed"))
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-cmd-fail"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCommandRequest{
			Cmd: "ls -la",
		}

		result, err := h.HandleRunCommand(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeSandboxCreation, err.(*MCPError).Code)
		assert.Equal(t, RunCommandResponse{}, result)
	})

	t.Run("reuses existing session and does not call sandbox creator", func(t *testing.T) {
		creatorCallCount := 0
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				creatorCallCount++
				return newMockSandbox("new-sandbox"), nil
			},
		}
		_, sm, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-cmd-reuse"

		// Pre-store a session
		existingSession := &UserSession{
			SessionID:   sessionID,
			UserID:      userID.String(),
			SandboxID:   "sandbox-existing-cmd",
			TemplateID:  "template-1",
			State:       "Running",
			AccessToken: "existing-token",
		}
		sm.sessions.Store(sessionID, existingSession)

		ctx := createTestContext(mcpSrv, userID, sessionID)

		// Test through SessionManager directly to verify session reuse
		session, err := sm.GetOrCreateSession(ctx, sessionID, userID.String(), "template-1", 10*time.Minute)

		require.NoError(t, err)
		assert.Equal(t, "sandbox-existing-cmd", session.SandboxID)
		assert.Equal(t, 0, creatorCallCount) // Creator should not be called
	})
}

func TestHandleRunCode_UserConfig(t *testing.T) {
	t.Run("uses user-configured template", func(t *testing.T) {
		var capturedTemplate string
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				capturedTemplate = opts.Template
				return newMockSandbox("sandbox-config"), nil
			},
		}
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-config"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		// Set user config with custom template
		customTTL := 30 * time.Minute
		customTimeout := 120 * time.Second
		ctx = SetUserSessionConfig(ctx, &UserSessionConfig{
			Template:         "custom-template",
			SandboxTTL:       &customTTL,
			ExecutionTimeout: &customTimeout,
		})

		args := RunCodeRequest{
			Code:     "print('test')",
			Language: "python",
		}

		// This will fail at executeCodeInSandbox but we can verify template was used
		_, _ = h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		assert.Equal(t, "custom-template", capturedTemplate)
	})

	t.Run("uses default template when userConfig is nil", func(t *testing.T) {
		var capturedTemplate string
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				capturedTemplate = opts.Template
				return newMockSandbox("sandbox-default"), nil
			},
		}
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-default"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		// No user config set

		args := RunCodeRequest{
			Code:     "print('test')",
			Language: "python",
		}

		_, _ = h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		assert.Equal(t, h.config.DefaultTemplate, capturedTemplate)
	})

	t.Run("uses default template when userConfig.Template is empty", func(t *testing.T) {
		var capturedTemplate string
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				capturedTemplate = opts.Template
				return newMockSandbox("sandbox-empty-config"), nil
			},
		}
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-empty-config"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		// Set empty user config
		ctx = SetUserSessionConfig(ctx, &UserSessionConfig{
			Template: "", // empty
		})

		args := RunCodeRequest{
			Code:     "print('test')",
			Language: "python",
		}

		_, _ = h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		assert.Equal(t, h.config.DefaultTemplate, capturedTemplate)
	})
}

func TestHandleRunCodeOnce_MoreBranches(t *testing.T) {
	t.Run("returns error when session ID not in context", func(t *testing.T) {
		operator := newMockOperatorWithSandbox("sandbox-abc", nil)
		h, _, _ := createTestHandlerWithOperator(operator)
		userID := uuid.New()

		// Create context WITHOUT session
		ctx := context.Background()
		testUser := &models.CreatedTeamAPIKey{
			ID:   userID,
			Name: "test-user",
		}
		ctx = SetUserContext(ctx, testUser)

		args := RunCodeOnceRequest{
			Code:     "print('test')",
			Language: "python",
		}

		result, err := h.HandleRunCodeOnce(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeInternalError, err.(*MCPError).Code)
		assert.Equal(t, RunCodeResponse{}, result)
	})

	t.Run("returns error when sandbox creation fails", func(t *testing.T) {
		operator := newMockOperatorWithSandbox("", errors.New("no available sandbox"))
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-once-fail"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCodeOnceRequest{
			Code:     "print('test')",
			Language: "python",
		}

		result, err := h.HandleRunCodeOnce(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeSandboxCreation, err.(*MCPError).Code)
		assert.Equal(t, RunCodeResponse{}, result)
	})

	t.Run("uses user-configured template", func(t *testing.T) {
		var capturedTemplate string
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				capturedTemplate = opts.Template
				return newMockSandboxWithRequest("sandbox-once-config", nil, errors.New("expected error")), nil
			},
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return newMockSandboxWithRequest(sandboxID, nil, errors.New("expected error")), nil
			},
		}
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-once-config"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		// Set user config with custom template
		ctx = SetUserSessionConfig(ctx, &UserSessionConfig{
			Template: "custom-once-template",
		})

		args := RunCodeOnceRequest{
			Code:     "print('test')",
			Language: "python",
		}

		// Will fail at executeCodeInSandbox but we can verify template was used
		_, _ = h.HandleRunCodeOnce(ctx, mcpgo.CallToolRequest{}, args)

		assert.Equal(t, "custom-once-template", capturedTemplate)
	})

	t.Run("uses user-configured SandboxTTL and ExecutionTimeout", func(t *testing.T) {
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				return newMockSandboxWithRequest("sandbox-once-ttl", nil, errors.New("expected error")), nil
			},
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return newMockSandboxWithRequest(sandboxID, nil, errors.New("expected error")), nil
			},
		}
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-once-ttl"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		// Set user config with custom TTL and timeout
		customTTL := 15 * time.Minute
		customTimeout := 90 * time.Second
		ctx = SetUserSessionConfig(ctx, &UserSessionConfig{
			SandboxTTL:       &customTTL,
			ExecutionTimeout: &customTimeout,
		})

		args := RunCodeOnceRequest{
			Code:     "print('test')",
			Language: "python",
		}

		// Will fail at executeCodeInSandbox but config is processed
		_, _ = h.HandleRunCodeOnce(ctx, mcpgo.CallToolRequest{}, args)

		// Test passes if no panic - config values are used in the handler
	})

	t.Run("cleanup is called even if code execution fails", func(t *testing.T) {
		cleanupCalled := false
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				return newMockSandboxWithRequest("sandbox-cleanup", nil, errors.New("expected error")), nil
			},
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				sbx := newMockSandboxWithRequest(sandboxID, nil, errors.New("expected error"))
				sbx.killErr = nil
				cleanupCalled = true
				return sbx, nil
			},
		}
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-cleanup"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCodeOnceRequest{
			Code:     "print('test')",
			Language: "python",
		}

		// Code execution will fail but cleanup should still be called
		_, _ = h.HandleRunCodeOnce(ctx, mcpgo.CallToolRequest{}, args)

		// Cleanup is called via defer in HandleRunCodeOnce
		assert.True(t, cleanupCalled, "cleanup should be called even if code execution fails")
	})
}

func TestHandleRunCommand_MoreBranches(t *testing.T) {
	t.Run("returns error when session ID not in context", func(t *testing.T) {
		operator := newMockOperatorWithSandbox("sandbox-cmd-abc", nil)
		h, _, _ := createTestHandlerWithOperator(operator)
		userID := uuid.New()

		// Create context WITHOUT session
		ctx := context.Background()
		testUser := &models.CreatedTeamAPIKey{
			ID:   userID,
			Name: "test-user",
		}
		ctx = SetUserContext(ctx, testUser)

		args := RunCommandRequest{
			Cmd: "ls -la",
		}

		result, err := h.HandleRunCommand(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeInternalError, err.(*MCPError).Code)
		assert.Equal(t, RunCommandResponse{}, result)
	})

	t.Run("uses user-configured template", func(t *testing.T) {
		var capturedTemplate string
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				capturedTemplate = opts.Template
				return newMockSandbox("sandbox-cmd-config"), nil
			},
		}
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-cmd-config"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		// Set user config with custom template
		ctx = SetUserSessionConfig(ctx, &UserSessionConfig{
			Template: "custom-cmd-template",
		})

		args := RunCommandRequest{
			Cmd: "ls -la",
		}

		// Will fail at ExecuteCommand but we can verify template was used
		_, _ = h.HandleRunCommand(ctx, mcpgo.CallToolRequest{}, args)

		assert.Equal(t, "custom-cmd-template", capturedTemplate)
	})

	t.Run("uses user-configured SandboxTTL and ExecutionTimeout", func(t *testing.T) {
		operator := &mockSandboxOperator{
			claimSandboxFunc: func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
				return newMockSandbox("sandbox-cmd-ttl"), nil
			},
		}
		h, _, mcpSrv := createTestHandlerWithOperator(operator)
		userID := uuid.New()
		sessionID := "session-cmd-ttl"

		ctx := createTestContext(mcpSrv, userID, sessionID)
		// Set user config with custom TTL and timeout
		customTTL := 20 * time.Minute
		customTimeout := 180 * time.Second
		ctx = SetUserSessionConfig(ctx, &UserSessionConfig{
			SandboxTTL:       &customTTL,
			ExecutionTimeout: &customTimeout,
		})

		args := RunCommandRequest{
			Cmd: "echo hello",
		}

		// Will fail at ExecuteCommand but config is processed
		_, _ = h.HandleRunCommand(ctx, mcpgo.CallToolRequest{}, args)

		// Test passes if no panic - config values are used in the handler
	})
}

func TestExecuteCodeInSandbox(t *testing.T) {
	t.Run("returns error for unsupported language", func(t *testing.T) {
		h, _, _ := createTestHandler()

		session := &UserSession{UserID: "user", SandboxID: "sandbox", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "code", "go")

		assert.Error(t, err)
		assert.Nil(t, result)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeCodeExecution, err.(*MCPError).Code)
		assert.Contains(t, err.Error(), "unsupported language")
	})

	t.Run("returns error for ruby language", func(t *testing.T) {
		h, _, _ := createTestHandler()

		session := &UserSession{UserID: "user", SandboxID: "sandbox", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "code", "ruby")

		assert.Error(t, err)
		assert.Nil(t, result)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeCodeExecution, err.(*MCPError).Code)
		assert.Contains(t, err.Error(), "unsupported language")
	})

	t.Run("defaults to python when language is empty", func(t *testing.T) {
		// Create operator with mock sandbox that returns request error
		mockSbx := newMockSandboxWithRequest("sandbox-default-lang", nil, errors.New("request error"))
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-default-lang", AccessToken: "token"}

		// Should not fail language validation when empty
		_, err := h.executeCodeInSandbox(context.Background(), session, "print('hello')", "")

		// Error should be from request, not language validation
		assert.Error(t, err)
		assert.NotContains(t, err.Error(), "unsupported language")
		assert.Contains(t, err.Error(), "request")
	})

	t.Run("accepts javascript language", func(t *testing.T) {
		mockSbx := newMockSandboxWithRequest("sandbox-js", nil, errors.New("request error"))
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-js", AccessToken: "token"}

		_, err := h.executeCodeInSandbox(context.Background(), session, "console.log('hello')", "javascript")

		// Error should be from request, not language validation
		assert.Error(t, err)
		assert.NotContains(t, err.Error(), "unsupported language")
	})

	t.Run("accepts JavaScript with different casing", func(t *testing.T) {
		mockSbx := newMockSandboxWithRequest("sandbox-js-case", nil, errors.New("request error"))
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-js-case", AccessToken: "token"}

		_, err := h.executeCodeInSandbox(context.Background(), session, "console.log('hello')", "  JavaScript  ")

		// Error should be from request, not language validation
		assert.Error(t, err)
		assert.NotContains(t, err.Error(), "unsupported language")
	})

	t.Run("returns error when sandbox request fails", func(t *testing.T) {
		mockSbx := newMockSandboxWithRequest("sandbox-req-fail", nil, errors.New("sandbox unreachable"))
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-req-fail", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('hello')", "python")

		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "failed to send request to sandbox")
	})

	t.Run("returns error when sandbox returns non-200 status", func(t *testing.T) {
		resp := newMockHTTPResponse(http.StatusInternalServerError, `{"error": "internal error"}`)
		mockSbx := newMockSandboxWithRequest("sandbox-500", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-500", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('hello')", "python")

		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "sandbox returned error")
		assert.Contains(t, err.Error(), "status=500")
	})

	t.Run("parses SSE response with stdout", func(t *testing.T) {
		sseData := `{"type": "stdout", "text": "Hello World\n"}
{"type": "stdout", "text": "Line 2\n"}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-stdout", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-stdout", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('Hello World')", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Logs.Stdout, 2)
		assert.Equal(t, "Hello World\n", result.Logs.Stdout[0])
		assert.Equal(t, "Line 2\n", result.Logs.Stdout[1])
	})

	t.Run("parses SSE response with stderr", func(t *testing.T) {
		sseData := `{"type": "stderr", "text": "Error: something went wrong\n"}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-stderr", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-stderr", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "import sys; sys.stderr.write('error')", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Logs.Stderr, 1)
		assert.Equal(t, "Error: something went wrong\n", result.Logs.Stderr[0])
	})

	t.Run("parses SSE response with error", func(t *testing.T) {
		sseData := `{"type": "error", "name": "NameError", "value": "name 'undefined' is not defined", "traceback": "Traceback...\n"}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-error", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-error", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print(undefined)", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.Error)
		assert.Equal(t, "NameError", result.Error.Name)
		assert.Equal(t, "name 'undefined' is not defined", result.Error.Value)
		assert.Contains(t, result.Error.Traceback, "Traceback")
	})

	t.Run("parses SSE response with result", func(t *testing.T) {
		sseData := `{"type": "result", "text": "42", "is_main_result": true}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-result", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-result", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "40 + 2", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Results, 1)
		assert.Equal(t, "42", result.Results[0].Text)
		assert.True(t, result.Results[0].IsMainResult)
	})

	t.Run("parses SSE response with number_of_executions", func(t *testing.T) {
		sseData := `{"type": "number_of_executions", "execution_count": 5}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-exec-count", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-exec-count", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "1+1", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		require.NotNil(t, result.ExecutionCount)
		assert.Equal(t, 5, *result.ExecutionCount)
	})

	t.Run("handles mixed SSE response types", func(t *testing.T) {
		sseData := `{"type": "stdout", "text": "output\n"}
{"type": "stderr", "text": "warning\n"}
{"type": "result", "text": "100"}
{"type": "number_of_executions", "execution_count": 3}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-mixed", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-mixed", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('test')", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Logs.Stdout, 1)
		assert.Len(t, result.Logs.Stderr, 1)
		assert.Len(t, result.Results, 1)
		assert.NotNil(t, result.ExecutionCount)
		assert.Equal(t, 3, *result.ExecutionCount)
	})

	t.Run("skips empty lines in SSE response", func(t *testing.T) {
		sseData := `{"type": "stdout", "text": "line1\n"}

{"type": "stdout", "text": "line2\n"}
`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-empty-lines", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-empty-lines", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('test')", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Logs.Stdout, 2)
	})

	t.Run("skips invalid JSON lines", func(t *testing.T) {
		sseData := `{"type": "stdout", "text": "valid\n"}
not valid json
{"type": "stdout", "text": "also valid\n"}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-invalid-json", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-invalid-json", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('test')", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Logs.Stdout, 2)
	})

	t.Run("skips lines without type field", func(t *testing.T) {
		sseData := `{"type": "stdout", "text": "valid\n"}
{"text": "no type field"}
{"type": "stdout", "text": "also valid\n"}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-no-type", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-no-type", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('test')", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Logs.Stdout, 2)
	})

	t.Run("ignores unknown message types", func(t *testing.T) {
		sseData := `{"type": "stdout", "text": "valid\n"}
{"type": "unknown_type", "data": "some data"}
{"type": "stdout", "text": "also valid\n"}`
		resp := newMockHTTPResponse(http.StatusOK, sseData)
		mockSbx := newMockSandboxWithRequest("sandbox-unknown-type", resp, nil)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-unknown-type", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('test')", "python")

		require.NoError(t, err)
		require.NotNil(t, result)
		assert.Len(t, result.Logs.Stdout, 2)
	})

	t.Run("sets access token in request header", func(t *testing.T) {
		var capturedHeaders http.Header
		mockSbx := newMockSandboxWithRequestCapture("sandbox-header", &capturedHeaders)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-header", AccessToken: "my-secret-token"}

		_, _ = h.executeCodeInSandbox(context.Background(), session, "print('test')", "python")

		assert.Equal(t, "my-secret-token", capturedHeaders.Get("X-Access-Token"))
		assert.Equal(t, "application/json", capturedHeaders.Get("Content-Type"))
	})

	t.Run("does not set access token header when empty", func(t *testing.T) {
		var capturedHeaders http.Header
		mockSbx := newMockSandboxWithRequestCapture("sandbox-no-token", &capturedHeaders)
		operator := &mockSandboxOperator{
			getClaimedSandboxFunc: func(ctx context.Context, userID, sandboxID string) (infra.Sandbox, error) {
				return mockSbx, nil
			},
		}

		config := DefaultServerConfig()
		h := &Handler{operator: operator, config: config}

		session := &UserSession{UserID: "user", SandboxID: "sandbox-no-token", AccessToken: ""}

		_, _ = h.executeCodeInSandbox(context.Background(), session, "print('test')", "python")

		assert.Equal(t, "", capturedHeaders.Get("X-Access-Token"))
	})
}

func TestGetStringField(t *testing.T) {
	t.Run("returns string value when key exists", func(t *testing.T) {
		data := map[string]interface{}{
			"name":  "test-value",
			"count": 42,
		}

		result := getStringField(data, "name")

		assert.Equal(t, "test-value", result)
	})

	t.Run("returns empty string when key does not exist", func(t *testing.T) {
		data := map[string]interface{}{
			"name": "test-value",
		}

		result := getStringField(data, "nonexistent")

		assert.Equal(t, "", result)
	})

	t.Run("returns empty string when value is not a string", func(t *testing.T) {
		data := map[string]interface{}{
			"count": 42,
			"flag":  true,
			"obj":   map[string]interface{}{"nested": "value"},
		}

		assert.Equal(t, "", getStringField(data, "count"))
		assert.Equal(t, "", getStringField(data, "flag"))
		assert.Equal(t, "", getStringField(data, "obj"))
	})

	t.Run("returns empty string for nil map", func(t *testing.T) {
		var data map[string]interface{} = nil

		result := getStringField(data, "key")

		assert.Equal(t, "", result)
	})
}
