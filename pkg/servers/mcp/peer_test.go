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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockPeers is a simple in-memory Peers implementation for testing
type mockPeers struct {
	mu      sync.RWMutex
	members []peers.Peer
}

func newMockPeers(members ...peers.Peer) *mockPeers {
	return &mockPeers{members: members}
}

func (m *mockPeers) Start(_ context.Context, _ int) error { return nil }
func (m *mockPeers) Stop() error                          { return nil }
func (m *mockPeers) GetPeers() []peers.Peer {
	m.mu.RLock()
	defer m.mu.RUnlock()
	result := make([]peers.Peer, len(m.members))
	copy(result, m.members)
	return result
}
func (m *mockPeers) GetAllMembers() []peers.Peer                 { return m.GetPeers() }
func (m *mockPeers) WaitForPeers(_ context.Context, _ int) error { return nil }
func (m *mockPeers) LocalAddr() net.IP                           { return net.ParseIP("127.0.0.1") }
func (m *mockPeers) LocalPort() int                              { return 0 }

func (m *mockPeers) SetMembers(members ...peers.Peer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.members = members
}

// createPeerTestSessionManager creates a SessionManager for peer testing
func createPeerTestSessionManager() *SessionManager {
	config := &ServerConfig{
		SessionSyncPort: 7090,
	}
	operator := &mockSandboxOperator{}
	sm := NewSessionManager(operator, config, nil)
	sm.SetRequestPeer(noopRequestPeer)
	return sm
}

// createPeerTestSessionManagerWithPeers creates a SessionManager for peer testing with mockPeers and custom requestPeer
func createPeerTestSessionManagerWithPeers(mp *mockPeers, requestPeer RequestPeerFunc) *SessionManager {
	config := &ServerConfig{
		SessionSyncPort: 7090,
	}
	operator := &mockSandboxOperator{}
	sm := NewSessionManager(operator, config, mp)
	sm.SetRequestPeer(requestPeer)
	return sm
}

func TestSyncSessionWithPeers(t *testing.T) {
	t.Run("syncs session to all peers", func(t *testing.T) {
		var mu sync.Mutex
		requests := make(map[string][]byte)
		requestPeer := func(ctx context.Context, method, ip, path string, body []byte) error {
			mu.Lock()
			requests[ip] = body
			mu.Unlock()
			return nil
		}
		mp := newMockPeers(
			peers.Peer{IP: "1.1.1.1", Name: "node-1"},
			peers.Peer{IP: "1.1.1.2", Name: "node-2"},
		)
		sm := createPeerTestSessionManagerWithPeers(mp, requestPeer)

		session := &UserSession{
			SessionID: "session-123",
			UserID:    "user-456",
			SandboxID: "sandbox-789",
		}

		err := sm.SyncSessionWithPeers(context.Background(), session, false)

		require.NoError(t, err)
		assert.Len(t, requests, 2)

		// Verify message content
		for _, body := range requests {
			var msg SessionSyncMessage
			err := json.Unmarshal(body, &msg)
			require.NoError(t, err)
			assert.Equal(t, "session-123", msg.Session.SessionID)
			assert.False(t, msg.Deleted)
		}
	})

	t.Run("aggregates errors from multiple peers", func(t *testing.T) {
		requestPeer := func(ctx context.Context, method, ip, path string, body []byte) error {
			return errors.New("peer error: " + ip)
		}
		mp := newMockPeers(
			peers.Peer{IP: "1.1.1.1", Name: "node-1"},
			peers.Peer{IP: "1.1.1.2", Name: "node-2"},
		)
		sm := createPeerTestSessionManagerWithPeers(mp, requestPeer)

		session := &UserSession{SessionID: "session-123"}

		err := sm.SyncSessionWithPeers(context.Background(), session, false)

		assert.Error(t, err)
		// Both errors should be aggregated
		assert.Contains(t, err.Error(), "peer error")
	})
}

func TestHandleSessionSync(t *testing.T) {
	t.Run("stores session from peer", func(t *testing.T) {
		sm := createPeerTestSessionManager()

		session := &UserSession{
			SessionID: "session-from-peer",
			UserID:    "user-123",
			SandboxID: "sandbox-456",
		}
		msg := SessionSyncMessage{Session: session, Deleted: false}
		body, _ := json.Marshal(msg)

		req := httptest.NewRequest(http.MethodPost, SessionSyncAPI, bytes.NewReader(body))

		resp, apiErr := sm.handleSessionSync(req)

		assert.Nil(t, apiErr)
		assert.Equal(t, http.StatusNoContent, resp.Code)

		// Verify session was stored
		stored, ok := sm.sessions.Load("session-from-peer")
		assert.True(t, ok)
		assert.Equal(t, "user-123", stored.(*UserSession).UserID)
	})
}

func TestSyncSessionWithPeers_NoPeers(t *testing.T) {
	t.Run("returns nil when no peers", func(t *testing.T) {
		mp := newMockPeers() // empty members
		sm := createPeerTestSessionManagerWithPeers(mp, noopRequestPeer)

		session := &UserSession{
			SessionID: "session-123",
			UserID:    "user-456",
		}

		err := sm.SyncSessionWithPeers(context.Background(), session, false)

		assert.NoError(t, err)
	})

	t.Run("returns nil when peersManager is nil", func(t *testing.T) {
		sm := createPeerTestSessionManager() // nil peersManager

		session := &UserSession{
			SessionID: "session-123",
			UserID:    "user-456",
		}

		err := sm.SyncSessionWithPeers(context.Background(), session, false)

		assert.NoError(t, err)
	})
}

func TestSyncSessionWithPeers_Delete(t *testing.T) {
	t.Run("syncs session deletion to peers", func(t *testing.T) {
		var mu sync.Mutex
		requests := make(map[string]SessionSyncMessage)
		requestPeer := func(ctx context.Context, method, ip, path string, body []byte) error {
			var msg SessionSyncMessage
			if err := json.Unmarshal(body, &msg); err != nil {
				return err
			}
			mu.Lock()
			requests[ip] = msg
			mu.Unlock()
			return nil
		}
		mp := newMockPeers(peers.Peer{IP: "1.1.1.1", Name: "node-1"})
		sm := createPeerTestSessionManagerWithPeers(mp, requestPeer)

		session := &UserSession{
			SessionID: "session-to-delete",
			UserID:    "user-456",
		}

		err := sm.SyncSessionWithPeers(context.Background(), session, true)

		require.NoError(t, err)
		assert.Len(t, requests, 1)
		assert.True(t, requests["1.1.1.1"].Deleted)
		assert.Equal(t, "session-to-delete", requests["1.1.1.1"].Session.SessionID)
	})
}

func TestHandleSessionSync_InvalidJSON(t *testing.T) {
	t.Run("returns error for invalid JSON", func(t *testing.T) {
		sm := createPeerTestSessionManager()

		req := httptest.NewRequest(http.MethodPost, SessionSyncAPI, bytes.NewReader([]byte("invalid-json")))

		_, apiErr := sm.handleSessionSync(req)

		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "failed to unmarshal body")
	})
}

func TestHandleSessionSync_NilSession(t *testing.T) {
	t.Run("returns error for nil session", func(t *testing.T) {
		sm := createPeerTestSessionManager()

		msg := SessionSyncMessage{Session: nil, Deleted: false}
		body, _ := json.Marshal(msg)

		req := httptest.NewRequest(http.MethodPost, SessionSyncAPI, bytes.NewReader(body))

		_, apiErr := sm.handleSessionSync(req)

		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Equal(t, "session is nil", apiErr.Message)
	})
}

func TestHandleSessionSync_DeleteSession(t *testing.T) {
	t.Run("deletes session from peer sync", func(t *testing.T) {
		sm := createPeerTestSessionManager()

		// Pre-populate session
		existingSession := &UserSession{
			SessionID: "session-to-delete",
			UserID:    "user-123",
		}
		sm.sessions.Store("session-to-delete", existingSession)

		// Send delete message
		msg := SessionSyncMessage{
			Session: &UserSession{SessionID: "session-to-delete"},
			Deleted: true,
		}
		body, _ := json.Marshal(msg)

		req := httptest.NewRequest(http.MethodPost, SessionSyncAPI, bytes.NewReader(body))

		resp, apiErr := sm.handleSessionSync(req)

		assert.Nil(t, apiErr)
		assert.Equal(t, http.StatusNoContent, resp.Code)

		// Verify session was deleted
		_, ok := sm.sessions.Load("session-to-delete")
		assert.False(t, ok)
	})
}

func TestStartAndStopPeerSync(t *testing.T) {
	t.Run("starts and stops peer sync server", func(t *testing.T) {
		config := &ServerConfig{
			SessionSyncPort: 17091, // Use a unique port to avoid conflicts
		}
		operator := &mockSandboxOperator{}
		sm := NewSessionManager(operator, config, nil)
		sm.SetRequestPeer(noopRequestPeer)

		// Start the peer sync
		err := sm.startPeerSync()
		require.NoError(t, err)

		// Give the server time to start
		time.Sleep(100 * time.Millisecond)

		// Verify server is running
		assert.NotNil(t, sm.httpSrv)

		// Stop the peer sync
		sm.stopPeerSync()

		// Give it time to stop
		time.Sleep(100 * time.Millisecond)
	})
}

func TestSyncSessionWithPeers_PartialFailure(t *testing.T) {
	t.Run("continues syncing even when some peers fail", func(t *testing.T) {
		var mu sync.Mutex
		successfulPeers := make([]string, 0)
		requestPeer := func(ctx context.Context, method, ip, path string, body []byte) error {
			if ip == "fail-peer" {
				return errors.New("connection failed")
			}
			mu.Lock()
			successfulPeers = append(successfulPeers, ip)
			mu.Unlock()
			return nil
		}

		mp := newMockPeers(
			peers.Peer{IP: "success-peer-1", Name: "node-1"},
			peers.Peer{IP: "fail-peer", Name: "node-2"},
			peers.Peer{IP: "success-peer-2", Name: "node-3"},
		)
		sm := createPeerTestSessionManagerWithPeers(mp, requestPeer)

		session := &UserSession{SessionID: "session-123"}

		err := sm.SyncSessionWithPeers(context.Background(), session, false)

		// Should return error because one peer failed
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "connection failed")

		// But successful peers should have been synced
		mu.Lock()
		defer mu.Unlock()
		assert.Len(t, successfulPeers, 2)
	})
}
