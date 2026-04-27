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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/servers/web"
	"k8s.io/klog/v2"
)

const (
	SessionSyncAPI = "/session/sync"
)

// SessionSyncMessage represents a session sync message between peers
type SessionSyncMessage struct {
	Session *UserSession `json:"session"`
	Deleted bool         `json:"deleted"`
}

// startPeerSync starts the peer synchronization HTTP server
func (sm *SessionManager) startPeerSync() error {
	mux := http.NewServeMux()
	web.RegisterRoute(mux, http.MethodPost, SessionSyncAPI, sm.handleSessionSync)

	sm.httpSrv = &http.Server{
		Addr:              fmt.Sprintf(":%d", sm.config.SessionSyncPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Start HTTP server
	go func() {
		klog.InfoS("Starting session sync server", "address", sm.httpSrv.Addr)
		if err := sm.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("Session sync HTTP server failed: %v", err)
		}
	}()

	return nil
}

// stopPeerSync stops the peer synchronization
func (sm *SessionManager) stopPeerSync() {
	sm.stopOnce.Do(func() {
		if sm.httpSrv != nil {
			_ = sm.httpSrv.Shutdown(context.Background())
		}
	})
}

// SyncSessionWithPeers syncs a session change to all peers
func (sm *SessionManager) SyncSessionWithPeers(ctx context.Context, session *UserSession, deleted bool) error {
	if sm.peersManager == nil {
		return nil
	}

	peerList := sm.peersManager.GetPeers()
	if len(peerList) == 0 {
		return nil
	}

	msg := SessionSyncMessage{
		Session: session,
		Deleted: deleted,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		errStrings []string
	)

	for _, p := range peerList {
		wg.Add(1)
		go func(peerIP string) {
			defer wg.Done()
			if err := sm.requestPeer(ctx, http.MethodPost, peerIP, SessionSyncAPI, body); err != nil {
				mu.Lock()
				errStrings = append(errStrings, err.Error())
				mu.Unlock()
			}
		}(p.IP)
	}
	wg.Wait()

	if len(errStrings) == 0 {
		return nil
	}
	return errors.New(strings.Join(errStrings, ";"))
}

// handleSessionSync handles session sync requests from peers
func (sm *SessionManager) handleSessionSync(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	log := klog.FromContext(r.Context())

	var msg SessionSyncMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("failed to unmarshal body: %s", err.Error()),
		}
	}

	if msg.Session == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "session is nil",
		}
	}

	if msg.Deleted {
		sm.sessions.Delete(msg.Session.SessionID)
		log.Info("session deleted from peer sync", "sessionID", msg.Session.SessionID)
	} else {
		sm.sessions.Store(msg.Session.SessionID, msg.Session)
		log.Info("session synced from peer", "sessionID", msg.Session.SessionID, "sandboxID", msg.Session.SandboxID)
	}

	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
