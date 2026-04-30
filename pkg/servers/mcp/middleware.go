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
	"strconv"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"k8s.io/klog/v2"
)

// SessionManagementMiddleware creates middleware for session management
func (s *MCPServer) SessionManagementMiddleware(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		// Extract session ID from MCP client session
		var sessionID string
		if clientSession := server.ClientSessionFromContext(ctx); clientSession != nil {
			sessionID = clientSession.SessionID()
		}

		// Session ID is required
		if sessionID == "" {
			klog.FromContext(ctx).Error(nil, "session ID is required but not found")
			return nil, NewMCPError(ErrorCodeAuthFailed, "Session ID is required", nil)
		}

		// Call next handler
		return next(ctx, req)
	}
}

// LoggingMiddleware creates middleware for logging tool calls
func (s *MCPServer) LoggingMiddleware(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		log := klog.FromContext(ctx).WithValues("middleware", "Logging", "tool", req.Params.Name)

		// Get user info if available
		userID := "unknown"
		if user, err := GetUserFromContext(ctx); err == nil {
			userID = user.ID.String()
		}

		// Get session ID if available
		sessionID := "unknown"
		if clientSession := server.ClientSessionFromContext(ctx); clientSession != nil {
			if sid := clientSession.SessionID(); sid != "" {
				sessionID = sid
			}
		}

		log.Info("tool call started", "userID", userID, "sessionID", sessionID, "arguments", req.Params.Arguments)
		start := time.Now()

		// Call next handler
		result, err := next(ctx, req)

		// Log completion
		duration := time.Since(start)
		if err != nil {
			log.Error(err, "tool call failed", "duration", duration, "userID", userID, "sessionID", sessionID)
		} else {
			log.Info("tool call completed", "duration", duration, "userID", userID, "sessionID", sessionID)
		}

		return result, err
	}
}

// ConfigMiddleware parses user session configuration from HTTP headers
func (s *MCPServer) ConfigMiddleware(next server.ToolHandlerFunc) server.ToolHandlerFunc {
	return func(ctx context.Context, req mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
		// Extract configuration from HTTP headers
		config := &UserSessionConfig{}

		// Parse Template
		if template := req.Header.Get("X-Template"); template != "" {
			config.Template = template
		}

		// Parse SandboxTTL (in seconds)
		if ttlStr := req.Header.Get("X-Sandbox-TTL"); ttlStr != "" {
			if ttlSec, err := strconv.ParseInt(ttlStr, 10, 64); err == nil && ttlSec >= 0 {
				ttl := time.Duration(ttlSec) * time.Second
				config.SandboxTTL = &ttl
			}
		}

		// Parse ExecutionTimeout (in seconds)
		if timeoutStr := req.Header.Get("X-Execution-Timeout"); timeoutStr != "" {
			if timeoutSec, err := strconv.ParseInt(timeoutStr, 10, 64); err == nil && timeoutSec > 0 {
				timeout := time.Duration(timeoutSec) * time.Second
				config.ExecutionTimeout = &timeout
			}
		}

		// Set config in context
		ctx = SetUserSessionConfig(ctx, config)

		// Call next handler
		return next(ctx, req)
	}
}

// applyMiddlewares applies all middlewares to a tool handler
func (s *MCPServer) applyMiddlewares(handler server.ToolHandlerFunc) server.ToolHandlerFunc {
	// Execution order: Config -> Session -> Logging -> Tool Handler
	handler = s.LoggingMiddleware(handler)
	handler = s.SessionManagementMiddleware(handler)
	handler = s.ConfigMiddleware(handler)

	return handler
}
