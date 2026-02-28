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
	sandbox_manager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockCodeExecutor creates a mock code executor for testing
func mockCodeExecutor(result *RunCodeResponse, err error) CodeExecutorFunc {
	return func(ctx context.Context, session *UserSession, code string, language string) (*RunCodeResponse, error) {
		if err != nil {
			return nil, err
		}
		return result, nil
	}
}

// mockHandlerSandboxCreator creates a mock sandbox creator for Handler testing
func mockHandlerSandboxCreator(sandboxID, accessToken, state string, err error) OneTimeSandboxCreatorFunc {
	return func(ctx context.Context, manager *sandbox_manager.SandboxManager, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
		if err != nil {
			return nil, err
		}
		return &SandboxInfo{
			SandboxID:   sandboxID,
			AccessToken: accessToken,
			State:       state,
		}, nil
	}
}

// mockSandboxDeleterWithCallback creates a mock sandbox deleter that tracks calls
func mockSandboxDeleterWithCallback(callback func(userID, sandboxID string)) SandboxDeleterFunc {
	return func(ctx context.Context, manager *sandbox_manager.SandboxManager, userID, sandboxID string) error {
		if callback != nil {
			callback(userID, sandboxID)
		}
		return nil
	}
}

// mockCommandExecutor creates a mock command executor for testing
func mockCommandExecutor(result *CommandResult, err error) CommandExecutorFunc {
	return func(ctx context.Context, manager *sandbox_manager.SandboxManager, userID, sandboxID string, cmd string, envs map[string]string, cwd *string, timeout time.Duration) (*CommandResult, error) {
		if err != nil {
			return nil, err
		}
		return result, nil
	}
}

// createTestHandler creates a Handler for testing without real dependencies
func createTestHandler() (*Handler, *SessionManager, *server.MCPServer) {
	config := DefaultServerConfig()
	deps := &mockSessionDeps{}
	sm := createTestSessionManager(deps)
	mcpSrv := server.NewMCPServer("test-server", "0.0.1")

	h := &Handler{
		sessionManager: sm,
		manager:        nil, // Not needed when using mock executor
		config:         config,
		codeExecutor:   nil, // Will be set in tests
	}

	return h, sm, mcpSrv
}

// createTestHandlerWithDeps creates a Handler for testing with custom SessionDependencies
func createTestHandlerWithDeps(deps *mockSessionDeps) (*Handler, *SessionManager, *server.MCPServer) {
	config := DefaultServerConfig()
	sm := createTestSessionManager(deps)
	mcpSrv := server.NewMCPServer("test-server", "0.0.1")

	h := &Handler{
		sessionManager: sm,
		manager:        nil,
		config:         config,
		codeExecutor:   nil,
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

func TestHandleRunCode(t *testing.T) {
	t.Run("executes code successfully", func(t *testing.T) {
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("sandbox-abc", "token-xyz", "running", nil),
		}
		h, _, mcpSrv := createTestHandlerWithDeps(deps)
		userID := uuid.New()
		sessionID := "session-123"

		// Setup mock code executor
		expectedResult := &RunCodeResponse{
			SandboxID: "sandbox-abc",
			Logs: ExecutionLogs{
				Stdout: []string{"Hello, World!"},
				Stderr: []string{},
			},
			Results: []ExecutionResult{
				{Text: "42"},
			},
		}
		h.SetCodeExecutor(mockCodeExecutor(expectedResult, nil))

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCodeRequest{
			Code:     "print('Hello, World!')",
			Language: "python",
		}

		result, err := h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		require.NoError(t, err)
		assert.Equal(t, "sandbox-abc", result.SandboxID)
		assert.Equal(t, []string{"Hello, World!"}, result.Logs.Stdout)
		assert.Len(t, result.Results, 1)
		assert.Equal(t, "42", result.Results[0].Text)
	})

	t.Run("returns error when user not in context", func(t *testing.T) {
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("sandbox-abc", "token-xyz", "running", nil),
		}
		h, _, mcpSrv := createTestHandlerWithDeps(deps)
		sessionID := "session-123"

		h.SetCodeExecutor(mockCodeExecutor(&RunCodeResponse{}, nil))

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
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("sandbox-abc", "token-xyz", "running", nil),
		}
		h, _, _ := createTestHandlerWithDeps(deps)
		userID := uuid.New()

		h.SetCodeExecutor(mockCodeExecutor(&RunCodeResponse{}, nil))

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
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("", "", "", errors.New("sandbox creation failed")),
		}
		h, _, mcpSrv := createTestHandlerWithDeps(deps)
		userID := uuid.New()
		sessionID := "session-fail"

		h.SetCodeExecutor(mockCodeExecutor(&RunCodeResponse{}, nil))

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

	t.Run("returns error when code execution fails", func(t *testing.T) {
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("sandbox-abc", "token-xyz", "Running", nil),
		}
		h, _, mcpSrv := createTestHandlerWithDeps(deps)
		userID := uuid.New()
		sessionID := "session-exec-fail"

		h.SetCodeExecutor(mockCodeExecutor(nil, errors.New("execution failed")))

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCodeRequest{
			Code:     "invalid code",
			Language: "python",
		}

		result, err := h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		assert.Error(t, err)
		assert.Contains(t, err.Error(), "code execution failed")
		assert.Equal(t, RunCodeResponse{}, result)
	})

	t.Run("reuses existing session", func(t *testing.T) {
		creatorCallCount := 0
		deps := &mockSessionDeps{
			createSandboxFunc: func(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
				creatorCallCount++
				return &SandboxInfo{SandboxID: "new-sandbox"}, nil
			},
		}
		h, sm, mcpSrv := createTestHandlerWithDeps(deps)
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

		h.SetCodeExecutor(mockCodeExecutor(&RunCodeResponse{SandboxID: "sandbox-existing"}, nil))

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCodeRequest{
			Code:     "print('test')",
			Language: "python",
		}

		result, err := h.HandleRunCode(ctx, mcpgo.CallToolRequest{}, args)

		require.NoError(t, err)
		assert.Equal(t, "sandbox-existing", result.SandboxID)
		assert.Equal(t, 0, creatorCallCount) // Creator should not be called
	})

}

func TestHandleRunCodeOnce(t *testing.T) {
	t.Run("executes code and cleans up sandbox", func(t *testing.T) {
		h, _, mcpSrv := createTestHandler()
		userID := uuid.New()
		sessionID := "session-once-123"

		// Track if sandbox was deleted
		deleteCalled := false
		var deletedSandboxID string

		// Setup mock sandbox creator
		h.SetOneTimeSandboxCreator(mockHandlerSandboxCreator("sandbox-once-abc", "token-once-xyz", "Running", nil))

		// Setup mock sandbox deleter
		h.SetSandboxDeleter(mockSandboxDeleterWithCallback(func(userID, sandboxID string) {
			deleteCalled = true
			deletedSandboxID = sandboxID
		}))

		// Setup mock code executor
		expectedResult := &RunCodeResponse{
			SandboxID: "sandbox-once-abc",
			Logs: ExecutionLogs{
				Stdout: []string{"One-time execution"},
				Stderr: []string{},
			},
		}
		h.SetCodeExecutor(mockCodeExecutor(expectedResult, nil))

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCodeOnceRequest{
			Code:     "print('One-time execution')",
			Language: "python",
		}

		result, err := h.HandleRunCodeOnce(ctx, mcpgo.CallToolRequest{}, args)

		require.NoError(t, err)
		assert.Equal(t, "sandbox-once-abc", result.SandboxID)
		assert.Equal(t, []string{"One-time execution"}, result.Logs.Stdout)

		// Verify sandbox was cleaned up
		assert.True(t, deleteCalled, "sandbox should be deleted after execution")
		assert.Equal(t, "sandbox-once-abc", deletedSandboxID)
	})

}

func TestHandleRunCommand(t *testing.T) {
	t.Run("executes command successfully", func(t *testing.T) {
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("sandbox-cmd-abc", "token-xyz", "running", nil),
		}
		h, _, mcpSrv := createTestHandlerWithDeps(deps)
		userID := uuid.New()
		sessionID := "session-cmd-123"

		// Setup mock command executor
		expectedResult := &CommandResult{
			PID:      12345,
			Stdout:   []string{"command output"},
			Stderr:   []string{},
			ExitCode: 0,
			Exited:   true,
			Error:    nil,
		}
		h.SetCommandExecutor(mockCommandExecutor(expectedResult, nil))

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCommandRequest{
			Cmd:  "ls -la",
			Envs: map[string]string{"PATH": "/usr/bin"},
		}

		result, err := h.HandleRunCommand(ctx, mcpgo.CallToolRequest{}, args)

		require.NoError(t, err)
		assert.Equal(t, "sandbox-cmd-abc", result.SandboxID)
		assert.Equal(t, "command output", result.Stdout)
		assert.Equal(t, "", result.Stderr)
		assert.Equal(t, 0, result.ExitCode)
		assert.Empty(t, result.Error)
	})

	t.Run("returns response with error info when command fails", func(t *testing.T) {
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("sandbox-cmd-fail", "token", "running", nil),
		}
		h, _, mcpSrv := createTestHandlerWithDeps(deps)
		userID := uuid.New()
		sessionID := "session-cmd-exec-fail"

		// Command executor returns error
		h.SetCommandExecutor(mockCommandExecutor(nil, errors.New("command execution failed")))

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCommandRequest{Cmd: "invalid-cmd"}

		result, err := h.HandleRunCommand(ctx, mcpgo.CallToolRequest{}, args)

		// Note: HandleRunCommand returns response with error info, not Go error
		require.NoError(t, err)
		assert.Equal(t, "sandbox-cmd-fail", result.SandboxID)
		assert.Equal(t, -1, result.ExitCode)
		assert.Contains(t, result.Error, "command execution failed")
	})

	t.Run("reuses existing session", func(t *testing.T) {
		creatorCallCount := 0
		deps := &mockSessionDeps{
			createSandboxFunc: func(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
				creatorCallCount++
				return &SandboxInfo{SandboxID: "new-sandbox"}, nil
			},
		}
		h, sm, mcpSrv := createTestHandlerWithDeps(deps)
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

		h.SetCommandExecutor(mockCommandExecutor(&CommandResult{
			Stdout:   []string{"output"},
			ExitCode: 0,
		}, nil))

		ctx := createTestContext(mcpSrv, userID, sessionID)
		args := RunCommandRequest{Cmd: "echo hello"}

		result, err := h.HandleRunCommand(ctx, mcpgo.CallToolRequest{}, args)

		require.NoError(t, err)
		assert.Equal(t, "sandbox-existing-cmd", result.SandboxID)
		assert.Equal(t, 0, creatorCallCount) // Creator should not be called
	})

}

// mockSandboxRequester creates a mock sandbox requester that returns a mock HTTP response
func mockSandboxRequester(body string, statusCode int, err error) SandboxRequesterFunc {
	return func(ctx context.Context, manager *sandbox_manager.SandboxManager, userID, sandboxID string, method, path string, port int, reqBody io.Reader, headers http.Header) (*http.Response, error) {
		if err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: statusCode,
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	}
}

func TestExecuteCodeInSandbox(t *testing.T) {
	t.Run("executes python code successfully", func(t *testing.T) {
		h, _, _ := createTestHandler()

		// Mock SSE response with stdout and result
		sseResponse := `{"type":"stdout","text":"Hello, World!\n"}
{"type":"result","text":"42"}
{"type":"number_of_executions","execution_count":1}`

		h.SetSandboxRequester(mockSandboxRequester(sseResponse, 200, nil))

		session := &UserSession{
			UserID:      "user-123",
			SandboxID:   "sandbox-123",
			AccessToken: "token-abc",
		}

		result, err := h.executeCodeInSandbox(context.Background(), session, "print('Hello, World!')", "python")

		require.NoError(t, err)
		assert.Equal(t, "sandbox-123", result.SandboxID)
		assert.Equal(t, []string{"Hello, World!\n"}, result.Logs.Stdout)
		assert.Len(t, result.Results, 1)
		assert.Equal(t, "42", result.Results[0].Text)
		require.NotNil(t, result.ExecutionCount)
		assert.Equal(t, 1, *result.ExecutionCount)
	})

	t.Run("returns error for unsupported language", func(t *testing.T) {
		h, _, _ := createTestHandler()

		h.SetSandboxRequester(mockSandboxRequester("", 200, nil))

		session := &UserSession{UserID: "user", SandboxID: "sandbox", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "code", "go")

		assert.Error(t, err)
		assert.Nil(t, result)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeCodeExecution, err.(*MCPError).Code)
		assert.Contains(t, err.Error(), "unsupported language")
	})

	t.Run("returns error when request fails", func(t *testing.T) {
		h, _, _ := createTestHandler()

		h.SetSandboxRequester(mockSandboxRequester("", 0, errors.New("connection refused")))

		session := &UserSession{UserID: "user", SandboxID: "sandbox", AccessToken: "token"}

		result, err := h.executeCodeInSandbox(context.Background(), session, "code", "python")

		assert.Error(t, err)
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), "failed to send request to sandbox")
	})

}
