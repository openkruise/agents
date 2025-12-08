package proxy

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

// Route represents an internal sandbox routing rule
type Route struct {
	IP           string            `json:"ip"`
	ID           string            `json:"id"`
	Owner        string            `json:"owner"`
	State        string            `json:"state"`
	ExtraHeaders map[string]string `json:"extra_headers"`
}

func (s *Server) SetRoute(route Route) {
	s.routes.Store(route.ID, route)
}

func (s *Server) SyncRouteWithPeers(route Route) error {
	body, err := json.Marshal(route)
	if err != nil {
		return err
	}
	var errStrings []string
	s.peerMu.RLock()
	for ip := range s.peers {
		if err = requestPeer(http.MethodPost, ip, RefreshAPI, body); err != nil {
			errStrings = append(errStrings, err.Error())
		}
	}
	s.peerMu.RUnlock()
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

func (s *Server) ListPeers() []Peer {
	peers := make([]Peer, 0)
	s.peerMu.RLock()
	defer s.peerMu.RUnlock()
	for _, peer := range s.peers {
		peers = append(peers, peer)
	}
	return peers
}

func (s *Server) DeleteRoute(id string) {
	s.routes.Delete(id)
}

// RequestAdapter is used to register mapping from business-side sandbox requests to internal logic
type RequestAdapter interface {
	// Map extracts sandbox id and port information from the request
	Map(scheme, authority, path string, port int, headers map[string]string) (
		sandboxID string, sandboxPort int, extraHeaders map[string]string, user string, err error)
	// Authorize determines if the user has permission to access the sandbox
	Authorize(user, owner string) bool
	// IsSandboxRequest determines whether the request is a sandbox request. If it returns true, it means it's requesting a sandbox, otherwise it's requesting the API Server. Only sandbox requests will be processed by the Adapter.
	IsSandboxRequest(authority, path string, port int) bool
	// Entry gets the entry address of the service process, such as "127.0.0.1:8080"
	Entry() string
}
