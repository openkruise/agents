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
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// createPeerTestSessionManager creates a SessionManager for peer testing
func createPeerTestSessionManager() *SessionManager {
	config := &ServerConfig{
		SessionSyncPort: 7090,
	}
	deps := &mockSessionDeps{}
	return NewSessionManager(deps, config)
}

// createPeerTestSessionManagerWithDeps creates a SessionManager for peer testing with custom deps
func createPeerTestSessionManagerWithDeps(deps *mockSessionDeps) *SessionManager {
	config := &ServerConfig{
		SessionSyncPort: 7090,
	}
	return NewSessionManager(deps, config)
}

func TestSetPeer(t *testing.T) {
	t.Run("registers new peer", func(t *testing.T) {
		sm := createPeerTestSessionManager()

		sm.SetPeer("1.1.1.1")

		sm.peerMu.RLock()
		defer sm.peerMu.RUnlock()
		peer, exists := sm.peers["1.1.1.1"]
		assert.True(t, exists)
		assert.Equal(t, "1.1.1.1", peer.IP)
		assert.WithinDuration(t, time.Now(), peer.LastHeartBeat, time.Second)
	})

	t.Run("updates existing peer heartbeat", func(t *testing.T) {
		sm := createPeerTestSessionManager()

		// Set peer with old timestamp
		oldTime := time.Now().Add(-time.Hour)
		sm.peers["1.1.1.1"] = Peer{IP: "1.1.1.1", LastHeartBeat: oldTime}

		// Update peer
		sm.SetPeer("1.1.1.1")

		sm.peerMu.RLock()
		defer sm.peerMu.RUnlock()
		peer := sm.peers["1.1.1.1"]
		assert.True(t, peer.LastHeartBeat.After(oldTime))
	})

	t.Run("handles multiple peers", func(t *testing.T) {
		sm := createPeerTestSessionManager()

		sm.SetPeer("1.1.1.1")
		sm.SetPeer("1.1.1.2")
		sm.SetPeer("1.1.1.3")

		sm.peerMu.RLock()
		defer sm.peerMu.RUnlock()
		assert.Len(t, sm.peers, 3)
	})
}

func TestHelloPeer(t *testing.T) {
	t.Run("sends heartbeat successfully", func(t *testing.T) {
		var capturedMethod, capturedIP, capturedPath string
		deps := &mockSessionDeps{
			requestPeerFunc: func(method, ip, path string, body []byte) error {
				capturedMethod = method
				capturedIP = ip
				capturedPath = path
				return nil
			},
		}
		sm := createPeerTestSessionManagerWithDeps(deps)

		err := sm.HelloPeer("1.1.1.1")

		require.NoError(t, err)
		assert.Equal(t, http.MethodGet, capturedMethod)
		assert.Equal(t, "1.1.1.1", capturedIP)
		assert.Equal(t, SessionHelloAPI, capturedPath)
	})
}

func TestSyncSessionWithPeers(t *testing.T) {
	t.Run("syncs session to all peers", func(t *testing.T) {
		var mu sync.Mutex
		requests := make(map[string][]byte)
		deps := &mockSessionDeps{
			requestPeerFunc: func(method, ip, path string, body []byte) error {
				mu.Lock()
				requests[ip] = body
				mu.Unlock()
				return nil
			},
		}
		sm := createPeerTestSessionManagerWithDeps(deps)

		// Setup peers
		sm.peers["1.1.1.1"] = Peer{IP: "1.1.1.1", LastHeartBeat: time.Now()}
		sm.peers["1.1.1.2"] = Peer{IP: "1.1.1.2", LastHeartBeat: time.Now()}

		session := &UserSession{
			SessionID: "session-123",
			UserID:    "user-456",
			SandboxID: "sandbox-789",
		}

		err := sm.SyncSessionWithPeers(session, false)

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
		deps := &mockSessionDeps{
			requestPeerFunc: func(method, ip, path string, body []byte) error {
				return errors.New("peer error: " + ip)
			},
		}
		sm := createPeerTestSessionManagerWithDeps(deps)
		sm.peers["1.1.1.1"] = Peer{IP: "1.1.1.1", LastHeartBeat: time.Now()}
		sm.peers["1.1.1.2"] = Peer{IP: "1.1.1.2", LastHeartBeat: time.Now()}

		session := &UserSession{SessionID: "session-123"}

		err := sm.SyncSessionWithPeers(session, false)

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

func TestHandleHello(t *testing.T) {
	t.Run("registers peer from remote address", func(t *testing.T) {
		sm := createPeerTestSessionManager()

		req := httptest.NewRequest(http.MethodGet, SessionHelloAPI, nil)
		req.RemoteAddr = "1.1.1.1:12345"

		resp, apiErr := sm.handleHello(req)

		assert.Nil(t, apiErr)
		assert.Equal(t, http.StatusNoContent, resp.Code)

		// Verify peer was registered
		sm.peerMu.RLock()
		defer sm.peerMu.RUnlock()
		_, exists := sm.peers["1.1.1.1"]
		assert.True(t, exists)
	})
}
