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

package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/openkruise/agents/pkg/utils"
)

// Environment variable names for peer discovery
const (
	EnvNamespace          = "PEER_NAMESPACE"
	EnvLabelSelector      = "PEER_LABEL_SELECTOR"
	EnvMemberlistBindPort = "MEMBERLIST_BIND_PORT"
	HealthAPI             = "/healthz"
	ReadyAPI              = "/readyz"
)

// ReadinessCheck reports whether the gateway is ready to receive traffic.
type ReadinessCheck func() error

// getMemberlistBindPort reads the memberlist bind port from environment variable
// Returns the default port if not set or invalid
func getMemberlistBindPort() int {
	if val := os.Getenv(EnvMemberlistBindPort); val != "" {
		if port, err := strconv.Atoi(val); err == nil && port > 0 {
			return port
		}
	}
	return config.DefaultMemberlistBindPort
}

func normalizePort(port int, defaultPort int) int {
	if port <= 0 {
		return defaultPort
	}
	return port
}

// Server handles peer-to-peer communication for route synchronization
type Server struct {
	httpServer         *http.Server
	peerManager        *peers.MemberlistPeers
	port               int
	memberlistBindPort int
	client             client.Client
	registry           *registry.Registry
	readinessCheck     ReadinessCheck
}

// NewServer creates a new peer server
func NewServer(
	client client.Client,
	routeRegistry *registry.Registry,
	port int,
	readinessChecks ...ReadinessCheck,
) *Server {
	server := &Server{
		port:               normalizePort(port, refresh.DefaultPort),
		client:             client,
		registry:           routeRegistry,
		memberlistBindPort: getMemberlistBindPort(),
	}
	checks := append([]ReadinessCheck(nil), readinessChecks...)
	server.readinessCheck = func() error {
		for _, check := range checks {
			if check == nil {
				continue
			}
			if err := check(); err != nil {
				return err
			}
		}
		if !server.registry.Ready() {
			return registry.ErrNotReady
		}
		return nil
	}
	return server
}

// Start starts the HTTP server for handling refresh requests from peers
func (s *Server) Start(ctx context.Context) error {
	mux := s.newServeMux()

	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", normalizePort(s.port, refresh.DefaultPort)),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Get node name from environment variables
	nodeName := os.Getenv("HOSTNAME")
	if nodeName == "" {
		nodeName = os.Getenv("POD_NAME")
	}
	if nodeName == "" {
		return fmt.Errorf("HOSTNAME or POD_NAME environment variable must be set")
	}

	// Get local IP
	localIP := os.Getenv("POD_IP")
	if localIP == "" {
		localIP = utils.GetFirstNonLoopbackIP()
	}
	if localIP == "" {
		return fmt.Errorf("failed to determine local IP")
	}

	// Get namespace and label selector from environment variables
	namespace := os.Getenv(EnvNamespace)
	labelSelector := os.Getenv(EnvLabelSelector)

	s.peerManager = peers.NewMemberlistPeers(s.client, peers.NodePrefixSandboxGateway+nodeName, namespace, labelSelector)

	if err := s.peerManager.Start(ctx, s.memberlistBindPort); err != nil {
		return err
	}

	go func() {
		klog.InfoS("Starting sandbox-gateway peer server", "address", s.httpServer.Addr)
		if err := s.httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.ErrorS(err, "Peer server failed to start")
		}
	}()

	return nil
}

func (s *Server) newServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(
		http.MethodPost+" "+refresh.Path,
		refresh.NewHandler(s.registry, nil),
	)
	mux.HandleFunc(HealthAPI, s.handleHealth)
	mux.HandleFunc(ReadyAPI, s.handleReady)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.readinessCheck != nil {
		if err := s.readinessCheck(); err != nil {
			http.Error(w, "gateway is not ready", http.StatusServiceUnavailable)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) Stop(ctx context.Context) error {
	var errs []error
	if s.httpServer != nil {
		if err := s.httpServer.Shutdown(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if s.peerManager != nil {
		if err := s.peerManager.Stop(); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
