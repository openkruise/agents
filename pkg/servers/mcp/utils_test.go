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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
)

func TestMaskAPIKey(t *testing.T) {
	tests := []struct {
		name     string
		apiKey   string
		expected string
	}{
		{
			name:     "empty string",
			apiKey:   "",
			expected: "****",
		},
		{
			name:     "1 character",
			apiKey:   "a",
			expected: "****",
		},
		{
			name:     "4 characters",
			apiKey:   "abcd",
			expected: "****",
		},
		{
			name:     "5 characters",
			apiKey:   "abcde",
			expected: "abcd*",
		},
		{
			name:     "normal api key",
			apiKey:   "sk-abc123def456",
			expected: "sk-a***********",
		},
		{
			name:     "uuid format key",
			apiKey:   "550e8400-e29b-41d4-a716-446655440000",
			expected: "550e********************************",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := MaskAPIKey(tt.apiKey)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestSetGetUserContext(t *testing.T) {
	testUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Name: "test-user",
		Key:  "test-key",
	}

	t.Run("set and get user from context", func(t *testing.T) {
		ctx := context.Background()
		ctx = SetUserContext(ctx, testUser)

		user, err := GetUserFromContext(ctx)
		assert.NoError(t, err)
		assert.NotNil(t, user)
		assert.Equal(t, testUser.ID, user.ID)
		assert.Equal(t, testUser.Name, user.Name)
	})

	t.Run("get user from empty context", func(t *testing.T) {
		ctx := context.Background()

		user, err := GetUserFromContext(ctx)
		assert.Error(t, err)
		assert.Nil(t, user)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeAuthFailed, err.(*MCPError).Code)
	})

	t.Run("get user with wrong type in context", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), userContextKey, "invalid-type")

		user, err := GetUserFromContext(ctx)
		assert.Error(t, err)
		assert.Nil(t, user)
		assert.Contains(t, err.Error(), "Invalid user")
	})
}

func TestSetGetUserSessionConfig(t *testing.T) {
	sandboxTTL := 5 * time.Minute
	execTimeout := 60 * time.Second

	testConfig := &UserSessionConfig{
		Template:         "test-template",
		SandboxTTL:       &sandboxTTL,
		ExecutionTimeout: &execTimeout,
	}

	t.Run("set and get session config", func(t *testing.T) {
		ctx := context.Background()
		ctx = SetUserSessionConfig(ctx, testConfig)

		config := GetUserSessionConfig(ctx)
		assert.NotNil(t, config)
		assert.Equal(t, "test-template", config.Template)
		assert.Equal(t, sandboxTTL, *config.SandboxTTL)
		assert.Equal(t, execTimeout, *config.ExecutionTimeout)
	})

	t.Run("get config from empty context returns nil", func(t *testing.T) {
		ctx := context.Background()

		config := GetUserSessionConfig(ctx)
		assert.Nil(t, config)
	})

	t.Run("get config with wrong type returns nil", func(t *testing.T) {
		ctx := context.WithValue(context.Background(), userSessionConfigKey, "invalid-type")

		config := GetUserSessionConfig(ctx)
		assert.Nil(t, config)
	})

	t.Run("config with nil fields", func(t *testing.T) {
		emptyConfig := &UserSessionConfig{
			Template: "only-template",
		}
		ctx := context.Background()
		ctx = SetUserSessionConfig(ctx, emptyConfig)

		config := GetUserSessionConfig(ctx)
		assert.NotNil(t, config)
		assert.Equal(t, "only-template", config.Template)
		assert.Nil(t, config.SandboxTTL)
		assert.Nil(t, config.ExecutionTimeout)
	})
}
