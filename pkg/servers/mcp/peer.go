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
	SessionSyncAPI  = "/session/sync"
	SessionHelloAPI = "/session/hello"
)

var SessionHeartBeatInterval = 5 * time.Second

// Peer represents a remote SessionManager peer
type Peer struct {
	IP            string
	LastHeartBeat time.Time
}

// SessionSyncMessage represents a session sync message between peers
type SessionSyncMessage struct {
	Session *UserSession `json:"session"`
	Deleted bool         `json:"deleted"`
}

// SetPeer registers or updates a peer
func (sm *SessionManager) SetPeer(ip string) {
	sm.peerMu.Lock()
	defer sm.peerMu.Unlock()
	sm.peers[ip] = Peer{
		IP:            ip,
		LastHeartBeat: time.Now(),
	}
}

// startPeerSync starts the peer synchronization HTTP server and heartbeat loop
func (sm *SessionManager) startPeerSync() error {
	mux := http.NewServeMux()
	web.RegisterRoute(mux, http.MethodPost, SessionSyncAPI, sm.handleSessionSync)
	web.RegisterRoute(mux, http.MethodGet, SessionHelloAPI, sm.handleHello)

	sm.httpSrv = &http.Server{
		Addr:              fmt.Sprintf(":%d", sm.config.SessionSyncPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	sm.peerMu.Lock()
	sm.heartBeatTicker = time.NewTicker(SessionHeartBeatInterval)
	sm.peerMu.Unlock()

	// Start HTTP server
	go func() {
		klog.InfoS("Starting session sync server", "address", sm.httpSrv.Addr)
		if err := sm.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Errorf("Session sync HTTP server failed: %v", err)
		}
	}()

	// Start heartbeat loop
	go sm.heartBeatLoop()

	return nil
}

// stopPeerSync stops the peer synchronization
func (sm *SessionManager) stopPeerSync() {
	sm.peerMu.Lock()
	defer sm.peerMu.Unlock()
	close(sm.heartBeatStopCh)
	if sm.heartBeatTicker != nil {
		sm.heartBeatTicker.Stop()
	}
	if sm.httpSrv != nil {
		_ = sm.httpSrv.Shutdown(context.Background())
	}
}

// heartBeatLoop periodically checks peer health and removes stale peers
func (sm *SessionManager) heartBeatLoop() {
	log := klog.Background().V(5).WithValues("component", "sessionManager", "task", "heartBeatLoop")

	for {
		select {
		case <-sm.heartBeatTicker.C:
			sm.peerMu.Lock()
			peersToCheck := make([]Peer, 0, len(sm.peers))
			peersToDelete := make([]string, 0)

			for ip, peer := range sm.peers {
				if time.Since(peer.LastHeartBeat) > 5*SessionHeartBeatInterval {
					peersToDelete = append(peersToDelete, ip)
				} else {
					peersToCheck = append(peersToCheck, peer)
				}
			}
			if len(peersToDelete) > 0 {
				for _, ip := range peersToDelete {
					delete(sm.peers, ip)
					log.Info("peer deleted for heartbeat timeout", "ip", ip)
				}
			}
			sm.peerMu.Unlock()

			for _, peer := range peersToCheck {
				if err := sm.HelloPeer(peer.IP); err != nil {
					log.Error(err, "failed to send heartbeat to peer", "ip", peer.IP)
				}
			}
		case <-sm.heartBeatStopCh:
			return
		}
	}
}

// HelloPeer sends a heartbeat to a peer
func (sm *SessionManager) HelloPeer(ip string) error {
	return sm.deps.RequestPeer(http.MethodGet, ip, SessionHelloAPI, nil)
}

// SyncSessionWithPeers syncs a session change to all peers
func (sm *SessionManager) SyncSessionWithPeers(session *UserSession, deleted bool) error {
	msg := SessionSyncMessage{
		Session: session,
		Deleted: deleted,
	}
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	sm.peerMu.RLock()
	peerIPs := make([]string, 0, len(sm.peers))
	for ip := range sm.peers {
		peerIPs = append(peerIPs, ip)
	}
	sm.peerMu.RUnlock()

	if len(peerIPs) == 0 {
		return nil
	}

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		errStrings []string
	)

	for _, ip := range peerIPs {
		wg.Add(1)
		go func(peerIP string) {
			defer wg.Done()
			if err := sm.deps.RequestPeer(http.MethodPost, peerIP, SessionSyncAPI, body); err != nil {
				mu.Lock()
				errStrings = append(errStrings, err.Error())
				mu.Unlock()
			}
		}(ip)
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

// handleHello handles heartbeat requests from peers
func (sm *SessionManager) handleHello(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	log := klog.FromContext(r.Context()).V(1)

	// For intra-cluster peers, use remote address directly
	ip, _, _ := strings.Cut(r.RemoteAddr, ":")
	if ip == "" {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "failed to get your ip",
		}
	}

	log.Info("hello from peer", "ip", ip)
	sm.SetPeer(ip)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
