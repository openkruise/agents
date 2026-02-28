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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockSessionDeps implements SessionDependencies interface for testing
type mockSessionDeps struct {
	createSandboxFunc func(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error)
	requestPeerFunc   func(method, ip, path string, body []byte) error
}

func (m *mockSessionDeps) CreateSandbox(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
	if m.createSandboxFunc != nil {
		return m.createSandboxFunc(ctx, userID, sessionID, templateID, sandboxTTL)
	}
	return nil, errors.New("not implemented")
}

func (m *mockSessionDeps) RequestPeer(method, ip, path string, body []byte) error {
	if m.requestPeerFunc != nil {
		return m.requestPeerFunc(method, ip, path, body)
	}
	return nil
}

// newMockSandboxCreator creates a mock sandbox creator helper
func newMockSandboxCreator(sandboxID, accessToken, state string, err error) func(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
	return func(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
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

// createTestSessionManager creates a SessionManager for testing
func createTestSessionManager(deps SessionDependencies) *SessionManager {
	config := DefaultServerConfig()
	return NewSessionManager(deps, config)
}

func TestNewSessionManager(t *testing.T) {
	config := DefaultServerConfig()
	deps := &mockSessionDeps{}
	sm := NewSessionManager(deps, config)

	assert.NotNil(t, sm)
	assert.Equal(t, config, sm.config)
	assert.NotNil(t, sm.peers)
	assert.NotNil(t, sm.heartBeatStopCh)
	assert.NotNil(t, sm.deps)
}

func TestGetOrCreateSession(t *testing.T) {
	t.Run("creates new session successfully", func(t *testing.T) {
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("sandbox-123", "token-abc", "running", nil),
		}
		sm := createTestSessionManager(deps)

		ctx := context.Background()
		session, err := sm.GetOrCreateSession(ctx, "session-1", "user-1", "template-1", 5*time.Minute)

		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, "session-1", session.SessionID)
		assert.Equal(t, "user-1", session.UserID)
		assert.Equal(t, "sandbox-123", session.SandboxID)
		assert.Equal(t, "template-1", session.TemplateID)
		assert.Equal(t, "running", session.State)
		assert.Equal(t, "token-abc", session.AccessToken)
	})

	t.Run("reuses existing session for same user", func(t *testing.T) {
		callCount := 0
		deps := &mockSessionDeps{
			createSandboxFunc: func(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error) {
				callCount++
				return &SandboxInfo{SandboxID: "new-sandbox"}, nil
			},
		}
		sm := createTestSessionManager(deps)

		// Pre-store a session
		existingSession := &UserSession{
			SessionID:   "session-existing",
			UserID:      "user-1",
			SandboxID:   "sandbox-existing",
			TemplateID:  "template-1",
			State:       "Running",
			AccessToken: "existing-token",
		}
		sm.sessions.Store("session-existing", existingSession)

		ctx := context.Background()
		session, err := sm.GetOrCreateSession(ctx, "session-existing", "user-1", "template-2", 5*time.Minute)

		require.NoError(t, err)
		require.NotNil(t, session)
		assert.Equal(t, "sandbox-existing", session.SandboxID) // Should return existing sandbox
		assert.Equal(t, 0, callCount)                          // Creator should not be called
	})

	t.Run("rejects session belonging to different user", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		// Pre-store a session for user-1
		existingSession := &UserSession{
			SessionID:   "session-1",
			UserID:      "user-1",
			SandboxID:   "sandbox-1",
			TemplateID:  "template-1",
			State:       "Running",
			AccessToken: "token-1",
		}
		sm.sessions.Store("session-1", existingSession)

		ctx := context.Background()
		// Try to access with different user
		session, err := sm.GetOrCreateSession(ctx, "session-1", "user-2", "template-1", 5*time.Minute)

		assert.Error(t, err)
		assert.Nil(t, session)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeAuthFailed, err.(*MCPError).Code)
		assert.Contains(t, err.Error(), "does not belong to")
	})

	t.Run("handles sandbox creation failure", func(t *testing.T) {
		deps := &mockSessionDeps{
			createSandboxFunc: newMockSandboxCreator("", "", "", errors.New("sandbox creation failed")),
		}
		sm := createTestSessionManager(deps)

		ctx := context.Background()
		session, err := sm.GetOrCreateSession(ctx, "session-fail", "user-1", "template-1", 5*time.Minute)

		assert.Error(t, err)
		assert.Nil(t, session)
		assert.IsType(t, &MCPError{}, err)
		assert.Equal(t, ErrorCodeSandboxCreation, err.(*MCPError).Code)
	})
}

func TestGetSession(t *testing.T) {
	t.Run("returns existing session", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		existingSession := &UserSession{
			SessionID:   "session-get",
			UserID:      "user-1",
			SandboxID:   "sandbox-get",
			TemplateID:  "template-1",
			State:       "Running",
			AccessToken: "token-get",
		}
		sm.sessions.Store("session-get", existingSession)

		session, ok := sm.GetSession("session-get")

		assert.True(t, ok)
		require.NotNil(t, session)
		assert.Equal(t, "session-get", session.SessionID)
		assert.Equal(t, "sandbox-get", session.SandboxID)
	})

	t.Run("returns false for non-existing session", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		session, ok := sm.GetSession("non-existing")

		assert.False(t, ok)
		assert.Nil(t, session)
	})
}

func TestOnSandboxAdd(t *testing.T) {
	t.Run("adds session from cluster event", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		sm.OnSandboxAdd("session-add", "sandbox-add", "user-add", "token-add", "Running")

		session, ok := sm.GetSession("session-add")
		assert.True(t, ok)
		require.NotNil(t, session)
		assert.Equal(t, "session-add", session.SessionID)
		assert.Equal(t, "sandbox-add", session.SandboxID)
		assert.Equal(t, "user-add", session.UserID)
		assert.Equal(t, "token-add", session.AccessToken)
		assert.Equal(t, "Running", session.State)
	})

	t.Run("ignores empty session ID", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		sm.OnSandboxAdd("", "sandbox-1", "user-1", "token-1", "Running")

		// Should not store anything
		_, ok := sm.GetSession("")
		assert.False(t, ok)
	})
}

func TestOnSandboxDelete(t *testing.T) {
	t.Run("deletes existing session", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		// Pre-store a session
		sm.sessions.Store("session-delete", &UserSession{
			SessionID: "session-delete",
			SandboxID: "sandbox-delete",
		})

		sm.OnSandboxDelete("session-delete")

		_, ok := sm.GetSession("session-delete")
		assert.False(t, ok)
	})

	t.Run("handles non-existing session gracefully", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		// Should not panic
		sm.OnSandboxDelete("non-existing")
	})

	t.Run("ignores empty session ID", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		// Pre-store a session
		sm.sessions.Store("valid-session", &UserSession{
			SessionID: "valid-session",
		})

		sm.OnSandboxDelete("")

		// Other sessions should not be affected
		_, ok := sm.GetSession("valid-session")
		assert.True(t, ok)
	})
}

func TestOnSandboxUpdate(t *testing.T) {
	t.Run("updates existing session", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		// Pre-store a session
		sm.sessions.Store("session-update", &UserSession{
			SessionID:   "session-update",
			UserID:      "user-1",
			SandboxID:   "sandbox-1",
			State:       "pending",
			AccessToken: "old-token",
		})

		// Update the session
		sm.OnSandboxUpdate("session-update", "sandbox-1", "user-1", "new-token", "running")

		session, ok := sm.GetSession("session-update")
		assert.True(t, ok)
		require.NotNil(t, session)
		assert.Equal(t, "running", session.State)
		assert.Equal(t, "new-token", session.AccessToken)
	})

	t.Run("creates session if not exists", func(t *testing.T) {
		deps := &mockSessionDeps{}
		sm := createTestSessionManager(deps)

		sm.OnSandboxUpdate("new-session", "sandbox-new", "user-new", "token-new", "Running")

		session, ok := sm.GetSession("new-session")
		assert.True(t, ok)
		require.NotNil(t, session)
		assert.Equal(t, "new-session", session.SessionID)
		assert.Equal(t, "sandbox-new", session.SandboxID)
	})
}
