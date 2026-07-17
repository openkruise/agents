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

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/servers/e2b/adapters"
)

// Route is re-exported from pkg/sandboxroute for backward compatibility.
type Route = sandboxroute.Route

func (s *Server) SetRoute(ctx context.Context, route Route) sandboxroute.MutationResult {
	log := klog.FromContext(ctx)
	log.Info("try to set route", "new", route)
	shape, err := validateRouteShape(route)
	if err != nil {
		log.Error(err, "rejected invalid route mutation")
		return s.store.RecordInvalid()
	}
	var result sandboxroute.MutationResult
	if shape == sandboxroute.ShapeFull {
		result = s.store.UpsertFull(route)
	} else {
		result = s.store.UpsertIDOnly(route)
	}
	s.enqueueMutation(result)
	s.updateRouteCount()
	log.V(5).Info("route mutation completed", "result", result.Result, "reason", result.Reason)
	return result
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
	return s.store.Get(id)
}

// RouteResourceVersion exposes the version of an opaque route key without
// leaking route projection into infra.
func (s *Server) RouteResourceVersion(id string) (string, bool) {
	route, ok := s.store.Get(id)
	return route.ResourceVersion, ok
}

func (s *Server) ListRoutes() []Route {
	return s.store.List()
}

func (s *Server) ListPeers() []peers.Peer {
	if s.peersManager != nil {
		return s.peersManager.GetPeers()
	}
	return nil
}

func (s *Server) DeleteRoute(id string) {
	route, ok := s.store.Get(id)
	if !ok {
		return
	}
	shape, err := validateRouteShape(route)
	if err != nil {
		return
	}
	var result sandboxroute.MutationResult
	if shape == sandboxroute.ShapeFull {
		key, _ := route.ObjectKey()
		result = s.store.DeleteAuthoritativeByObjectKey(key, "")
	} else {
		result = s.store.DeleteIDOnlyConditionally(route)
	}
	s.enqueueMutation(result)
	s.updateRouteCount()
}

func validateRouteShape(route Route) (sandboxroute.Shape, error) {
	if err := route.Validate(); err != nil {
		return "", err
	}
	return route.Shape()
}

// DeleteAuthoritativeByObjectKey removes the current full route for a locally
// observed object absence. The separately resolved legacy ID is used only to
// drain an old ID-only peer record when no full record exists.
func (s *Server) DeleteAuthoritativeByObjectKey(key types.NamespacedName, legacyFallbackID string) sandboxroute.MutationResult {
	result := s.store.DeleteAuthoritativeByObjectKey(key, legacyFallbackID)
	s.enqueueMutation(result)
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
