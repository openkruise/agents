/*
Copyright 2026.

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

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
)

func (s *Server) SetRoute(route sandboxroute.Route) sandboxroute.MutationResult {
	result := s.store.Upsert(route)
	s.updateRouteCount()
	return result
}

// SyncRouteWithPeers sends a route update to all peer gateways via HTTP POST /refresh.
// outbound is this process's peer-owner client. Callers must not construct a
// second client with an independent security decision.
func SyncRouteWithPeers(ctx context.Context, peersManager peers.Peers, route sandboxroute.Route, outbound *PeerOutbound) error {
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}

	// Get peers from Peers - no manual locking needed
	var peerList []peers.Peer
	if peersManager != nil {
		peerList = peersManager.GetPeers()
	}

	peerCount.Set(float64(len(peerList)))

	if len(peerList) == 0 {
		return nil
	}
	if outbound == nil {
		return fmt.Errorf("peer outbound client is not configured")
	}

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		peerErrs []error
	)

	for _, peer := range peerList {
		wg.Add(1)
		go func(peerIP string) {
			defer wg.Done()
			if requestErr := outbound.requestWithRetry(ctx, http.MethodPost, peerIP, refresh.Path, body); requestErr != nil {
				mu.Lock()
				peerErrs = append(peerErrs, requestErr)
				mu.Unlock()
			}
		}(peer.IP)
	}
	wg.Wait()

	return errors.Join(peerErrs...)
}

func (s *Server) SyncRouteWithPeers(ctx context.Context, route sandboxroute.Route) error {
	return SyncRouteWithPeers(ctx, s.peersManager, route, s.peerOutbound)
}

func (s *Server) LoadRoute(id string) (sandboxroute.Route, bool) {
	return s.store.Get(id)
}

func (s *Server) ListPeers() []peers.Peer {
	if s.peersManager != nil {
		return s.peersManager.GetPeers()
	}
	return nil
}

// Delete applies an authoritative route deletion.
func (s *Server) Delete(route sandboxroute.Route) sandboxroute.MutationResult {
	result := s.store.Delete(route)
	s.updateRouteCount()
	return result
}

// RequestAdapter is used to register the mapping from business-side sandbox requests to internal logic
type RequestAdapter interface {
	// ParseRequest normalizes raw HTTP headers into a ParsedRequest.
	// Each data plane should convert its native header format to map[string]string
	// (using HTTP/2 pseudo-header keys: :scheme, :authority, :path, plus "host"),
	// then call this method to get normalized request info.
	ParseRequest(headers map[string]string) *adapters.ParsedRequest
	// Map extracts sandbox ID, port and other information from the request
	Map(req *adapters.ParsedRequest) (
		sandboxID string, sandboxPort int, extraHeaders map[string]string, err error)
	// IsSandboxRequest determines whether the request is a sandbox request. If it returns true, it's a sandbox request,
	// otherwise it's an API Server request. Only sandbox requests are processed by the Adapter.
	IsSandboxRequest(authority, path string, port int) bool
	// Entry gets the entry address of the service process, such as "127.0.0.1:8080"
	Entry() string
}
