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
	"net/http"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
)

// Route is re-exported from pkg/utils/proxyutils for backward compatibility.
// New code should import pkg/utils/proxyutils directly.
type Route = proxyutils.Route

func (s *Server) SetRoute(ctx context.Context, route Route) {
	log := klog.FromContext(ctx)
	applied, becameLive := s.routes.Set(route.ID, route)
	if !applied {
		log.Info("received route is not newer than the existing one, skip write", "route", route)
		return
	}
	if becameLive {
		routeCount.Inc()
	}
	log.Info("successfully set route", "route", route)
}

func (s *Server) SyncRouteWithPeers(route Route) error {
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}

	// Get peers from Peers - no manual locking needed
	var peerList []peers.Peer
	if s.peersManager != nil {
		peerList = s.peersManager.GetPeers()
	}

	peerCount.Set(float64(len(peerList)))

	if len(peerList) == 0 {
		return nil
	}

	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		errStrings []string
	)

	for _, peer := range peerList {
		wg.Add(1)
		go func(peerIP string) {
			defer wg.Done()
			requestErr := retry.OnError(wait.Backoff{
				Steps:    10,
				Duration: 10 * time.Millisecond,
				Factor:   1.0,
				Jitter:   1.2,
			}, func(err error) bool {
				return true
			}, func() error {
				return requestPeer(http.MethodPost, peerIP, RefreshAPI, body)
			})
			if requestErr != nil {
				mu.Lock()
				errStrings = append(errStrings, requestErr.Error())
				mu.Unlock()
			}
		}(peer.IP)
	}
	wg.Wait()

	if len(errStrings) == 0 {
		return nil
	}
	return errors.New(strings.Join(errStrings, ";"))
}

func (s *Server) LoadRoute(id string) (Route, bool) {
	return s.routes.Get(id)
}

func (s *Server) ListRoutes() []Route {
	m := s.routes.List()
	routes := make([]Route, 0, len(m))
	for _, route := range m {
		routes = append(routes, route)
	}
	return routes
}

// GC reclaims expired route tombstones. The periodic route reconciler calls it.
func (s *Server) GC() {
	s.routes.GC()
}

func (s *Server) ListPeers() []peers.Peer {
	if s.peersManager != nil {
		return s.peersManager.GetPeers()
	}
	return nil
}

// DeleteRoute removes a route, leaving a versioned tombstone so a stale write
// cannot resurrect it. resourceVersion is the version of the deleting event; an
// empty value falls back to the resourceVersion currently recorded for the route.
func (s *Server) DeleteRoute(id string, resourceVersion string) {
	if s.routes.Delete(id, resourceVersion) {
		routeCount.Dec()
	}
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
