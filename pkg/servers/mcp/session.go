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
	"fmt"
	"net/http"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// SessionDependencies defines the interface for SessionManager's external dependencies.
// This abstraction allows for dependency injection and easier testing.
type SessionDependencies interface {
	// CreateSandbox creates a new sandbox for the user
	CreateSandbox(ctx context.Context, userID, sessionID, templateID string, sandboxTTL time.Duration) (*SandboxInfo, error)
	// RequestPeer sends an HTTP request to a peer node
	RequestPeer(method, ip, path string, body []byte) error
}

// SessionManager manages user sessions and their associated sandboxes
// Similar to proxy.Server, it maintains local session-sandbox mappings
// and synchronizes changes with peer instances
type SessionManager struct {
	sessions sync.Map // map[sessionID]*UserSession
	config   *ServerConfig

	// deps holds all external dependencies
	deps SessionDependencies

	// Peer synchronization (similar to proxy.Server)
	peers           map[string]Peer
	peerMu          sync.RWMutex
	heartBeatTicker *time.Ticker
	heartBeatStopCh chan struct{}
	httpSrv         *http.Server
}

// NewSessionManager creates a new session manager with the given dependencies
func NewSessionManager(deps SessionDependencies, config *ServerConfig) *SessionManager {
	return &SessionManager{
		deps:            deps,
		config:          config,
		peers:           make(map[string]Peer),
		heartBeatStopCh: make(chan struct{}),
	}
}

// Start starts the session manager background tasks and peer sync server
func (sm *SessionManager) Start() error {
	return sm.startPeerSync()
}

// Stop stops the session manager
func (sm *SessionManager) Stop() {
	sm.stopPeerSync()
}

// GetOrCreateSession gets an existing session or creates a new one.
// Parameters:
//   - sessionID: the MCP protocol session identifier
//   - userID: the authenticated user identifier
//   - templateID: the sandbox template to use
//   - sandboxTTL: the TTL (time-to-live) for sandbox shutdownTime
func (sm *SessionManager) GetOrCreateSession(ctx context.Context, sessionID, userID, templateID string, sandboxTTL time.Duration) (*UserSession, error) {
	log := klog.FromContext(ctx).WithValues("sessionID", sessionID, "userID", userID, "templateID", templateID)

	// Check if session already exists
	if value, ok := sm.sessions.Load(sessionID); ok {
		session := value.(*UserSession)

		// Verify session belongs to the user
		if session.UserID != userID {
			log.Error(nil, "session does not belong to user")
			return nil, NewMCPError(ErrorCodeAuthFailed, "Session does not belong to the authenticated user", nil)
		}

		// Session exists and belongs to user, reuse it
		// TODO: Check sandbox status before reusing. If sandbox is in abnormal state
		// (e.g., Failed, Terminating), should clean up the stale session and create a new one
		log.Info("reusing existing sandbox", "sandboxID", session.SandboxID)
		return session, nil
	}

	// Create new sandbox
	log.Info("creating new sandbox")
	sandboxInfo, err := sm.deps.CreateSandbox(ctx, userID, sessionID, templateID, sandboxTTL)
	if err != nil {
		log.Error(err, "failed to create sandbox")
		return nil, NewMCPError(ErrorCodeSandboxCreation, fmt.Sprintf("Failed to create sandbox: %v", err), nil)
	}

	// Create new session
	session := &UserSession{
		SessionID:   sessionID,
		UserID:      userID,
		SandboxID:   sandboxInfo.SandboxID,
		TemplateID:  templateID,
		State:       sandboxInfo.State,
		AccessToken: sandboxInfo.AccessToken,
	}

	sm.sessions.Store(sessionID, session)
	log.Info("sandbox created successfully", "sandboxID", session.SandboxID)

	// Sync new session to peers
	if err := sm.SyncSessionWithPeers(session, false); err != nil {
		log.Error(err, "failed to sync session with peers")
	}

	return session, nil
}

// GetSession retrieves an existing session by sessionID
func (sm *SessionManager) GetSession(sessionID string) (*UserSession, bool) {
	value, ok := sm.sessions.Load(sessionID)
	if !ok {
		return nil, false
	}
	return value.(*UserSession), true
}

// OnSandboxAdd handles sandbox add events from cluster
func (sm *SessionManager) OnSandboxAdd(sessionID, sandboxID, userID, accessToken, state string) {
	if sessionID == "" {
		return
	}
	session := &UserSession{
		SessionID:   sessionID,
		UserID:      userID,
		SandboxID:   sandboxID,
		State:       state,
		AccessToken: accessToken,
	}
	sm.sessions.Store(sessionID, session)
	klog.InfoS("session added from cluster event", "sessionID", sessionID, "sandboxID", sandboxID, "state", state)
}

// OnSandboxDelete handles sandbox delete events from cluster
func (sm *SessionManager) OnSandboxDelete(sessionID string) {
	if sessionID == "" {
		return
	}
	if _, ok := sm.sessions.Load(sessionID); ok {
		sm.sessions.Delete(sessionID)
		klog.InfoS("session deleted from cluster event", "sessionID", sessionID)
	}
}

// OnSandboxUpdate handles sandbox update events from cluster
func (sm *SessionManager) OnSandboxUpdate(sessionID, sandboxID, userID, accessToken, state string) {
	if sessionID == "" {
		return
	}
	session := &UserSession{
		SessionID:   sessionID,
		UserID:      userID,
		SandboxID:   sandboxID,
		State:       state,
		AccessToken: accessToken,
	}
	sm.sessions.Store(sessionID, session)
	klog.InfoS("session updated from cluster event", "sessionID", sessionID, "sandboxID", sandboxID, "state", state)
}
