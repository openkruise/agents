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
	"time"
)

// ToolNames defines the names of all available MCP tools
const (
	ToolRunCode     = "run_code"
	ToolRunCodeOnce = "run_code_once"
	ToolRunCommand  = "run_command"
)

// ServerConfig contains configuration for the MCP server
type ServerConfig struct {
	// Port is the port MCP server listens on
	Port int
	// DefaultTemplate is the default sandbox template ID
	DefaultTemplate string
	// SandboxTTL is the TTL (time-to-live) for sandbox auto-reclamation
	// This value will be used to configure MaxIdleTime when claiming sandboxes
	// When configured via HTTP headers or environment variables, values are specified in seconds.
	SandboxTTL time.Duration
	// ExecutionTimeout is the timeout for code/command execution
	// When configured via HTTP headers or environment variables, values are specified in seconds.
	ExecutionTimeout time.Duration
	// MCPEndpointPath is the MCP service endpoint path
	MCPEndpointPath string
	// SessionSyncPort is the port for session peer synchronization
	SessionSyncPort int
}

// UserSessionConfig contains user-customizable session configuration
// Users can override default values via HTTP headers
type UserSessionConfig struct {
	// Template overrides DefaultTemplate if specified
	Template string
	// SandboxTTL overrides default SandboxTTL if specified (in seconds)
	SandboxTTL *time.Duration
	// ExecutionTimeout overrides default ExecutionTimeout if specified (in seconds)
	ExecutionTimeout *time.Duration
}

// DefaultServerConfig returns default configuration
func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		Port:             8082,
		DefaultTemplate:  "code-interpreter",
		SandboxTTL:       5 * time.Minute,
		ExecutionTimeout: 60 * time.Second,
		MCPEndpointPath:  "/mcp",
		SessionSyncPort:  7790,
	}
}

// UserSession represents a user's session with associated sandbox
type UserSession struct {
	// SessionID is the MCP protocol session identifier
	SessionID string
	// UserID is the unique identifier of the user
	UserID string
	// SandboxID is the ID of the sandbox associated with this session
	SandboxID string
	// TemplateID is the template used to create the sandbox
	TemplateID string
	// State is the current state of the sandbox
	State string
	// AccessToken is the token for accessing the sandbox
	AccessToken string
}

// RunCodeRequest represents a request to execute code
// Aligned with E2B Code Interpreter Python client interface
type RunCodeRequest struct {
	// Code is the code to execute (required)
	Code string `json:"code" jsonschema:"required" jsonschema_description:"The code to execute in Jupyter Notebook cell format"`
	// Language is the programming language (optional, defaults to python, supports: python, javascript)
	Language string `json:"language,omitempty" jsonschema_description:"Programming language for code execution (python or javascript, defaults to python)"`
}

// RunCodeOnceRequest represents a request to execute code in a one-time sandbox
// Each call creates a new sandbox and cleans it up after execution
type RunCodeOnceRequest struct {
	// Code is the code to execute (required)
	Code string `json:"code" jsonschema:"required" jsonschema_description:"The code to execute in Jupyter Notebook cell format"`
	// Language is the programming language (optional, defaults to python, supports: python, javascript)
	Language string `json:"language,omitempty" jsonschema_description:"Programming language for code execution (python or javascript, defaults to python)"`
}

// ExecutionLogs represents the logs from code execution
type ExecutionLogs struct {
	Stdout []string `json:"stdout" jsonschema_description:"Standard output lines from code execution"`
	Stderr []string `json:"stderr" jsonschema_description:"Standard error lines from code execution"`
}

// ExecutionError represents an error during code execution
type ExecutionError struct {
	Name      string `json:"name" jsonschema_description:"Error name/type"`
	Value     string `json:"value" jsonschema_description:"Error message"`
	Traceback string `json:"traceback" jsonschema_description:"Error traceback"`
}

// ExecutionResult represents a result from code execution
// Supports multiple MIME types aligned with E2B Code Interpreter Execution model
type ExecutionResult struct {
	Text         string                 `json:"text,omitempty" jsonschema_description:"Text representation of the result"`
	HTML         string                 `json:"html,omitempty" jsonschema_description:"HTML representation of the result"`
	Markdown     string                 `json:"markdown,omitempty" jsonschema_description:"Markdown representation of the result"`
	JSON         map[string]interface{} `json:"json,omitempty" jsonschema_description:"JSON representation of the result"`
	PNG          string                 `json:"png,omitempty" jsonschema_description:"PNG image (base64 encoded)"`
	SVG          string                 `json:"svg,omitempty" jsonschema_description:"SVG image representation"`
	LaTeX        string                 `json:"latex,omitempty" jsonschema_description:"LaTeX representation of the result"`
	IsMainResult bool                   `json:"is_main_result,omitempty" jsonschema_description:"Whether this is the main result of the cell"`
	Extra        map[string]interface{} `json:"extra,omitempty" jsonschema_description:"Additional result metadata"`
}

// RunCodeResponse represents the response from code execution
// This structure conforms to E2B Code Interpreter Server response format
type RunCodeResponse struct {
	Error          *ExecutionError   `json:"error,omitempty" jsonschema_description:"Error information if execution failed"`
	Logs           ExecutionLogs     `json:"logs" jsonschema_description:"Execution logs containing stdout and stderr"`
	Results        []ExecutionResult `json:"results" jsonschema_description:"Execution results (rich output display)"`
	SandboxID      string            `json:"sandbox_id" jsonschema_description:"ID of the sandbox where code was executed"`
	ExecutionCount *int              `json:"execution_count,omitempty" jsonschema_description:"Execution count of the cell"`
}

// RunCommandRequest represents a request to execute a shell command
// Aligned with E2B Commands Python client interface
type RunCommandRequest struct {
	// Cmd is the command to execute (required)
	Cmd string `json:"cmd" jsonschema:"required" jsonschema_description:"The shell command to execute"`
	// Envs specifies environment variables for the command
	Envs map[string]string `json:"envs,omitempty" jsonschema_description:"Environment variables used for the command"`
	// Cwd specifies the working directory for the command
	Cwd *string `json:"cwd,omitempty" jsonschema_description:"Working directory to run the command"`
}

// RunCommandResponse represents the response from command execution
// Aligned with E2B CommandResult model
type RunCommandResponse struct {
	// Stdout contains the standard output from the command
	Stdout string `json:"stdout" jsonschema_description:"Standard output from the command execution"`
	// Stderr contains the standard error output from the command
	Stderr string `json:"stderr" jsonschema_description:"Standard error output from the command execution"`
	// ExitCode is the exit code of the command
	ExitCode int `json:"exit_code" jsonschema_description:"Exit code of the command (0 typically indicates success)"`
	// SandboxID is the ID of the sandbox where command was executed
	SandboxID string `json:"sandbox_id" jsonschema_description:"ID of the sandbox where the command was executed"`
	// Error contains error message if command execution failed
	Error string `json:"error,omitempty" jsonschema_description:"Error message if command execution failed"`
}

// MCP error codes (aligned with JSON-RPC / mcp-go definitions)
// See: https://github.com/mark3labs/mcp-go/blob/main/mcp/types.go#L403-L422
const (
	ErrorCodeAuthFailed       = -32001
	ErrorCodeSandboxCreation  = -32002
	ErrorCodeCodeExecution    = -32003
	ErrorCodeFileOperation    = -32004
	ErrorCodeTimeout          = -32005
	ErrorCodeInternalError    = -32603
	ErrorCodeCommandExecution = -32006
)

// MCPError represents an MCP protocol error
type MCPError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// Error implements the error interface
func (e *MCPError) Error() string {
	return e.Message
}

// NewMCPError creates a new MCP error
func NewMCPError(code int, message string, data interface{}) *MCPError {
	return &MCPError{
		Code:    code,
		Message: message,
		Data:    data,
	}
}
