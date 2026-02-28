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
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"k8s.io/klog/v2"

	sandbox_manager "github.com/openkruise/agents/pkg/sandbox-manager"
)

// CodeExecutorFunc is a function type for executing code in sandbox
// This abstraction allows for dependency injection and easier testing
type CodeExecutorFunc func(ctx context.Context, session *UserSession, code string, language string) (*RunCodeResponse, error)

// OneTimeSandboxCreatorFunc is a function type for creating one-time sandboxes
// This abstraction allows for dependency injection and easier testing
type OneTimeSandboxCreatorFunc func(ctx context.Context, manager *sandbox_manager.SandboxManager, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error)

// SandboxDeleterFunc is a function type for deleting sandboxes
// This abstraction allows for dependency injection and easier testing
type SandboxDeleterFunc func(ctx context.Context, manager *sandbox_manager.SandboxManager, userID, sandboxID string) error

// CommandExecutorFunc is a function type for executing commands in sandbox
// This abstraction allows for dependency injection and easier testing
type CommandExecutorFunc func(ctx context.Context, manager *sandbox_manager.SandboxManager, userID, sandboxID string, cmd string, envs map[string]string, cwd *string, timeout time.Duration) (*CommandResult, error)

// SandboxRequesterFunc is a function type for sending HTTP requests to sandbox
// This abstraction allows for dependency injection and easier testing
type SandboxRequesterFunc func(ctx context.Context, manager *sandbox_manager.SandboxManager, userID, sandboxID string, method, path string, port int, body io.Reader, headers http.Header) (*http.Response, error)

// Handler handles MCP tool calls
type Handler struct {
	sessionManager *SessionManager
	manager        *sandbox_manager.SandboxManager
	config         *ServerConfig

	// codeExecutor is the function used to execute code in sandbox
	// Defaults to executeCodeInSandbox, can be overridden for testing
	codeExecutor CodeExecutorFunc

	// oneTimeSandboxCreator is the function used to create one-time sandboxes
	// Defaults to CreateSandbox, can be overridden for testing
	oneTimeSandboxCreator OneTimeSandboxCreatorFunc

	// sandboxDeleter is the function used to delete sandboxes
	// Defaults to DeleteSandbox, can be overridden for testing
	sandboxDeleter SandboxDeleterFunc

	// commandExecutor is the function used to execute commands in sandbox
	// Defaults to ExecuteCommand, can be overridden for testing
	commandExecutor CommandExecutorFunc

	// sandboxRequester is the function used to send HTTP requests to sandbox
	// Defaults to RequestToSandboxWithHeaders, can be overridden for testing
	sandboxRequester SandboxRequesterFunc
}

func NewHandler(sessionManager *SessionManager, manager *sandbox_manager.SandboxManager, config *ServerConfig) *Handler {
	h := &Handler{
		sessionManager: sessionManager,
		manager:        manager,
		config:         config,
	}
	// Set default functions
	h.codeExecutor = h.executeCodeInSandbox
	h.oneTimeSandboxCreator = CreateSandbox
	h.sandboxDeleter = DeleteSandbox
	h.commandExecutor = ExecuteCommand
	h.sandboxRequester = RequestToSandboxWithHeaders
	return h
}

// SetCodeExecutor sets a custom code executor function (for testing)
func (h *Handler) SetCodeExecutor(executor CodeExecutorFunc) {
	h.codeExecutor = executor
}

// SetOneTimeSandboxCreator sets a custom one-time sandbox creator function (for testing)
func (h *Handler) SetOneTimeSandboxCreator(creator OneTimeSandboxCreatorFunc) {
	h.oneTimeSandboxCreator = creator
}

// SetSandboxDeleter sets a custom sandbox deleter function (for testing)
func (h *Handler) SetSandboxDeleter(deleter SandboxDeleterFunc) {
	h.sandboxDeleter = deleter
}

// SetCommandExecutor sets a custom command executor function (for testing)
func (h *Handler) SetCommandExecutor(executor CommandExecutorFunc) {
	h.commandExecutor = executor
}

// SetSandboxRequester sets a custom sandbox requester function (for testing)
func (h *Handler) SetSandboxRequester(requester SandboxRequesterFunc) {
	h.sandboxRequester = requester
}

// HandleRunCode handles the run_code tool
func (h *Handler) HandleRunCode(ctx context.Context, req mcpgo.CallToolRequest, args RunCodeRequest) (RunCodeResponse, error) {
	log := klog.FromContext(ctx).WithValues("tool", ToolRunCode)

	// Get user from context
	user, err := GetUserFromContext(ctx)
	if err != nil {
		return RunCodeResponse{}, err
	}

	// Get or determine session ID
	sessionID, err := GetSessionID(ctx)
	if err != nil {
		return RunCodeResponse{}, err
	}
	log = log.WithValues("sessionID", sessionID, "userID", user.ID.String())

	// Get user session configuration (with fallback to defaults)
	userConfig := GetUserSessionConfig(ctx)
	template := h.config.DefaultTemplate
	if userConfig != nil && userConfig.Template != "" {
		template = userConfig.Template
	}

	// Determine sandbox TTL (user config or default)
	sandboxTTL := h.config.SandboxTTL
	if userConfig != nil && userConfig.SandboxTTL != nil {
		sandboxTTL = *userConfig.SandboxTTL
	}

	// Get or create session
	session, err := h.sessionManager.GetOrCreateSession(ctx, sessionID, user.ID.String(), template, sandboxTTL)
	if err != nil {
		return RunCodeResponse{}, err
	}

	log.Info(fmt.Sprintf("session msg: %v ", session))

	log.Info("executing code", "sandboxID", session.SandboxID, "codeLength", len(args.Code))

	// Determine code execution timeout (user config or default)
	execTimeout := h.config.ExecutionTimeout
	if userConfig != nil && userConfig.ExecutionTimeout != nil {
		execTimeout = *userConfig.ExecutionTimeout
	}

	// Execute code through sandbox
	ctx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	result, err := h.codeExecutor(ctx, session, args.Code, args.Language)
	if err != nil {
		log.Error(err, "code execution failed")
		return RunCodeResponse{}, fmt.Errorf("code execution failed: %w", err)
	}
	resTmp, _ := json.Marshal(result)
	log.Info("code executed successfully", "sandboxID", session.SandboxID, "resultLength", len(resTmp))

	return *result, nil
}

// HandleRunCodeOnce handles the run_code_once tool
// This creates a new sandbox for each call and cleans it up after execution
func (h *Handler) HandleRunCodeOnce(ctx context.Context, req mcpgo.CallToolRequest, args RunCodeOnceRequest) (RunCodeResponse, error) {
	log := klog.FromContext(ctx).WithValues("tool", ToolRunCodeOnce)

	// Get user from context
	user, err := GetUserFromContext(ctx)
	if err != nil {
		return RunCodeResponse{}, err
	}

	// Get or determine session ID
	sessionID, err := GetSessionID(ctx)
	if err != nil {
		return RunCodeResponse{}, err
	}
	log = log.WithValues("sessionID", sessionID, "userID", user.ID.String())

	// Get user session configuration (with fallback to defaults)
	userConfig := GetUserSessionConfig(ctx)
	template := h.config.DefaultTemplate
	if userConfig != nil && userConfig.Template != "" {
		template = userConfig.Template
	}

	// Determine sandbox TTL (user config or default)
	sandboxTTL := h.config.SandboxTTL
	if userConfig != nil && userConfig.SandboxTTL != nil {
		sandboxTTL = *userConfig.SandboxTTL
	}

	log.Info("creating one-time sandbox for code execution", "codeLength", len(args.Code))
	// Create a new sandbox for this one-time execution
	sandboxInfo, err := h.oneTimeSandboxCreator(ctx, h.manager, user.ID.String(), sessionID, template, sandboxTTL)
	if err != nil {
		log.Error(err, "failed to create one-time sandbox")
		return RunCodeResponse{}, NewMCPError(ErrorCodeSandboxCreation, fmt.Sprintf("Failed to create sandbox: %v", err), nil)
	}

	log = log.WithValues("sandboxID", sandboxInfo.SandboxID)
	log.Info("one-time sandbox created successfully")

	// Ensure sandbox is cleaned up after execution, regardless of success or failure
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if cleanupErr := h.sandboxDeleter(cleanupCtx, h.manager, user.ID.String(), sandboxInfo.SandboxID); cleanupErr != nil {
			log.Error(cleanupErr, "failed to cleanup one-time sandbox")
		} else {
			log.Info("one-time sandbox cleaned up successfully")
		}
	}()

	// Create a temporary session for execution
	tempSession := &UserSession{
		UserID:      user.ID.String(),
		SandboxID:   sandboxInfo.SandboxID,
		AccessToken: sandboxInfo.AccessToken,
	}

	// Determine code execution timeout (user config or default)
	execTimeout := h.config.ExecutionTimeout
	if userConfig != nil && userConfig.ExecutionTimeout != nil {
		execTimeout = *userConfig.ExecutionTimeout
	}

	// Execute code through sandbox with timeout
	execCtx, cancel := context.WithTimeout(ctx, execTimeout)
	defer cancel()

	result, err := h.codeExecutor(execCtx, tempSession, args.Code, args.Language)
	if err != nil {
		log.Error(err, "code execution failed in one-time sandbox")
		return RunCodeResponse{}, fmt.Errorf("code execution failed: %w", err)
	}

	resTmp, _ := json.Marshal(result)
	log.Info("code executed successfully in one-time sandbox", "result", resTmp)

	return *result, nil
}

// HandleRunCommand handles the run_command tool
func (h *Handler) HandleRunCommand(ctx context.Context, req mcpgo.CallToolRequest, args RunCommandRequest) (RunCommandResponse, error) {
	log := klog.FromContext(ctx).WithValues("tool", ToolRunCommand)

	// Get user from context
	user, err := GetUserFromContext(ctx)
	if err != nil {
		return RunCommandResponse{}, err
	}

	// Get or determine session ID
	sessionID, err := GetSessionID(ctx)
	if err != nil {
		return RunCommandResponse{}, err
	}
	log = log.WithValues("sessionID", sessionID, "userID", user.ID.String())

	// Get user session configuration (with fallback to defaults)
	userConfig := GetUserSessionConfig(ctx)
	template := h.config.DefaultTemplate
	if userConfig != nil && userConfig.Template != "" {
		template = userConfig.Template
	}

	// Get or create session
	// NOTE: sandbox TTL is determined by the first request that creates the session.
	sandboxTTL := h.config.SandboxTTL
	if userConfig != nil && userConfig.SandboxTTL != nil {
		sandboxTTL = *userConfig.SandboxTTL
	}

	session, err := h.sessionManager.GetOrCreateSession(ctx, sessionID, user.ID.String(), template, sandboxTTL)
	if err != nil {
		return RunCommandResponse{}, err
	}

	log.Info("executing command", "sandboxID", session.SandboxID, "cmd", args.Cmd)

	// Determine timeout (user config or default)
	execTimeout := h.config.ExecutionTimeout
	if userConfig != nil && userConfig.ExecutionTimeout != nil {
		execTimeout = *userConfig.ExecutionTimeout
	}

	result, err := h.commandExecutor(
		ctx,
		h.manager,
		user.ID.String(),
		session.SandboxID,
		args.Cmd,
		args.Envs,
		args.Cwd,
		execTimeout,
	)
	if err != nil {
		log.Error(err, "command execution failed")
		return RunCommandResponse{
			SandboxID: session.SandboxID,
			Error:     err.Error(),
			ExitCode:  -1,
		}, nil // Return response with error info, not error
	}

	resTmp, _ := json.Marshal(result)
	log.Info("command res", string(resTmp))

	response := RunCommandResponse{
		Stdout:    strings.Join(result.Stdout, ""),
		Stderr:    strings.Join(result.Stderr, ""),
		ExitCode:  int(result.ExitCode),
		SandboxID: session.SandboxID,
	}

	if result.Error != nil {
		response.Error = result.Error.Error()
	}

	log.Info("command executed successfully", "sandboxID", session.SandboxID, "exitCode", response.ExitCode)
	return response, nil
}

// Helper functions

func (h *Handler) executeCodeInSandbox(ctx context.Context, session *UserSession, code string, language string) (*RunCodeResponse, error) {
	log := klog.FromContext(ctx)

	if language == "" {
		language = "python"
	}
	language = strings.ToLower(strings.TrimSpace(language))

	// Validate language is supported
	if language != "python" && language != "javascript" {
		msg := fmt.Sprintf("unsupported language: %s, only python and javascript are supported", language)
		log.Error(nil, msg, "language", language)
		return nil, NewMCPError(ErrorCodeCodeExecution, msg, nil)
	}

	// Prepare E2B compliant request body with all required fields
	requestBody, err := json.Marshal(map[string]interface{}{
		"code":       code,
		"context_id": nil,      // Reserved for future multi-context support
		"language":   language, // User-specified or default: python or javascript
		"env_vars":   nil,      // Reserved for future environment variable support
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Send request to sandbox /execute endpoint with authentication headers
	// Following E2B Python client format: X-Access-Token for envd authentication
	headers := http.Header{}
	headers.Set("Content-Type", "application/json")
	if session.AccessToken != "" {
		headers.Set("X-Access-Token", session.AccessToken)
	}

	resp, err := h.sandboxRequester(ctx, h.manager, session.UserID, session.SandboxID, http.MethodPost, "/execute", 49999, bytes.NewReader(requestBody), headers)
	if err != nil {
		return nil, fmt.Errorf("failed to send request to sandbox: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sandbox returned error: status=%d, body=%s", resp.StatusCode, string(body))
	}

	// Initialize execution result structure
	execution := &RunCodeResponse{
		Logs: ExecutionLogs{
			Stdout: []string{},
			Stderr: []string{},
		},
		Results:   []ExecutionResult{},
		SandboxID: session.SandboxID,
	}

	// Parse streaming SSE response line by line
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue // Skip empty lines
		}

		// Parse each line as JSON
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(line), &data); err != nil {
			log.V(1).Info("failed to parse JSON line, skipping", "line", line, "error", err)
			continue
		}

		// Extract type field to determine message type
		msgType, ok := data["type"].(string)
		if !ok {
			log.V(1).Info("missing or invalid type field, skipping", "data", data)
			continue
		}

		// Process different message types according to E2B specification
		switch msgType {
		case "stdout":
			if text, ok := data["text"].(string); ok {
				execution.Logs.Stdout = append(execution.Logs.Stdout, text)
			}
		case "stderr":
			if text, ok := data["text"].(string); ok {
				execution.Logs.Stderr = append(execution.Logs.Stderr, text)
			}
		case "result":
			// Parse complete result object with all MIME types
			var result ExecutionResult
			if resultBytes, err := json.Marshal(data); err == nil {
				if err := json.Unmarshal(resultBytes, &result); err == nil {
					execution.Results = append(execution.Results, result)
				} else {
					log.V(1).Info("failed to parse result object", "error", err)
				}
			}
		case "error":
			// Parse execution error with name, value, and traceback
			execution.Error = &ExecutionError{
				Name:      getStringField(data, "name"),
				Value:     getStringField(data, "value"),
				Traceback: getStringField(data, "traceback"),
			}
		case "number_of_executions":
			// Extract execution count
			if count, ok := data["execution_count"].(float64); ok {
				intCount := int(count)
				execution.ExecutionCount = &intCount
			}
		default:
			log.V(2).Info("unknown message type, ignoring", "type", msgType, "data", data)
		}
	}

	// Check for scanner errors (I/O issues during streaming)
	if err := scanner.Err(); err != nil {
		log.Error(err, "error reading streaming response", "sandboxID", session.SandboxID)
		// Return partial results even if streaming was interrupted
		return execution, fmt.Errorf("streaming interrupted: %w", err)
	}

	return execution, nil
}

// getStringField safely extracts a string field from a map
func getStringField(data map[string]interface{}, key string) string {
	if val, ok := data[key].(string); ok {
		return val
	}
	return ""
}
