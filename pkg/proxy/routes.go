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

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/utils/expectations"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"
)

// Route represents an internal sandbox routing rule
type Route struct {
	IP              string    `json:"ip"`
	ID              string    `json:"id"`
	UID             types.UID `json:"uid"`
	Owner           string    `json:"owner"`
	State           string    `json:"state"`
	ResourceVersion string    `json:"resourceVersion"`
}

func (s *Server) SetRoute(ctx context.Context, route Route) {
	log := klog.FromContext(ctx)
	log.Info("try to set route", "new", route)
	for {
		old, loaded := s.routes.LoadOrStore(route.ID, route)
		if !loaded {
			// First write, success directly
			return
		}

		oldRoute := old.(Route)
		if !expectations.IsResourceVersionNewer(oldRoute.ResourceVersion, route.ResourceVersion) {
			// New version is not newer than old version, skip write
			log.Info("received route is not newer than the existing one, skip write", "old", oldRoute)
			return
		}

		// Attempt CAS update
		if s.routes.CompareAndSwap(route.ID, old, route) {
			// Successfully replaced
			log.Info("successfully set route", "route", route)
			return
		}
		// CAS failed, modified by another goroutine, retry
	}
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
	raw, ok := s.routes.Load(id)
	if !ok {
		return Route{}, false
	}
	return raw.(Route), true
}

func (s *Server) ListRoutes() []Route {
	routes := make([]Route, 0)
	s.routes.Range(func(key, value any) bool {
		routes = append(routes, value.(Route))
		return true
	})
	return routes
}

func (s *Server) ListPeers() []peers.Peer {
	if s.peersManager != nil {
		return s.peersManager.GetPeers()
	}
	return nil
}

func (s *Server) DeleteRoute(id string) {
	s.routes.Delete(id)
}

// RequestAdapter is used to register the mapping from business-side sandbox requests to internal logic
type RequestAdapter interface {
	// Map extracts sandbox ID, port and other information from the request
	Map(scheme, authority, path string, port int, headers map[string]string) (
		sandboxID string, sandboxPort int, extraHeaders map[string]string, err error)
	// IsSandboxRequest determines whether the request is a sandbox request. If it returns true, it's a sandbox request,
	// otherwise it's an API Server request. Only sandbox requests are processed by the Adapter.
	IsSandboxRequest(authority, path string, port int) bool
	// Entry gets the entry address of the service process, such as "127.0.0.1:8080"
	Entry() string
}
