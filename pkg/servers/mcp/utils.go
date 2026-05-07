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
	"strings"

	"github.com/mark3labs/mcp-go/server"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

type contextKey string

const (
	userContextKey       contextKey = "user"
	userSessionConfigKey contextKey = "userSessionConfig"
)

// GetSessionID extracts MCP session ID from context
// SessionManagementMiddleware guarantees sessionID exists, so this should always succeed
func GetSessionID(ctx context.Context) (string, error) {
	// Get MCP client session from context (guaranteed by middleware)
	if clientSession := server.ClientSessionFromContext(ctx); clientSession != nil {
		sessionID := clientSession.SessionID()
		if sessionID != "" {
			return sessionID, nil
		}
	}

	// Should never reach here due to middleware validation
	return "", NewMCPError(ErrorCodeInternalError, "sessionID not found in context despite middleware validation", nil)
}

// SetUserContext sets user in context
func SetUserContext(ctx context.Context, user *models.CreatedTeamAPIKey) context.Context {
	return context.WithValue(ctx, userContextKey, user)
}

// GetUserFromContext extracts user from context
func GetUserFromContext(ctx context.Context) (*models.CreatedTeamAPIKey, error) {
	value := ctx.Value(userContextKey)
	if value == nil {
		return nil, NewMCPError(ErrorCodeAuthFailed, "User not found in context", nil)
	}

	user, ok := value.(*models.CreatedTeamAPIKey)
	if !ok {
		return nil, NewMCPError(ErrorCodeAuthFailed, "Invalid user in context", nil)
	}

	return user, nil
}

// SetUserSessionConfig sets user session configuration in context
func SetUserSessionConfig(ctx context.Context, config *UserSessionConfig) context.Context {
	return context.WithValue(ctx, userSessionConfigKey, config)
}

// GetUserSessionConfig extracts user session configuration from context
// Returns nil if not set (will fallback to server defaults)
func GetUserSessionConfig(ctx context.Context) *UserSessionConfig {
	if value := ctx.Value(userSessionConfigKey); value != nil {
		if config, ok := value.(*UserSessionConfig); ok {
			return config
		}
	}
	return nil
}

// MaskAPIKey returns the first 4 characters of an API key for logging
func MaskAPIKey(apiKey string) string {
	if len(apiKey) <= 4 {
		return "****"
	}
	return apiKey[:4] + strings.Repeat("*", len(apiKey)-4)
}
