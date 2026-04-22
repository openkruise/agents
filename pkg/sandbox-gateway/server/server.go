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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
)

// Environment variable names for peer discovery
const (
	EnvNamespace          = "PEER_NAMESPACE"
	EnvLabelSelector      = "PEER_LABEL_SELECTOR"
	EnvMemberlistBindPort = "MEMBERLIST_BIND_PORT"
)

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

// Server handles peer-to-peer communication for route synchronization
type Server struct {
	httpServer         *http.Server
	peerManager        *peers.MemberlistPeers
	port               int
	memberlistBindPort int
	client             client.Client
}

// NewServer creates a new peer server
func NewServer(client client.Client, port int) *Server {
	if port == 0 {
		port = proxy.SystemPort
	}
	return &Server{
		port:               port,
		client:             client,
		memberlistBindPort: getMemberlistBindPort(),
	}
}

// Start starts the HTTP server for handling refresh requests from peers
func (s *Server) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc(proxy.RefreshAPI, s.handleRefresh)

	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.port),
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

	s.peerManager = peers.NewMemberlistPeers(s.client, consts.NodePrefixSandboxGateway+nodeName, namespace, labelSelector)

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

// handleRefresh handles the /refresh endpoint for route synchronization
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	log := klog.FromContext(ctx)

	var route proxy.Route
	if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
		log.Error(err, "Failed to decode refresh request")
		http.Error(w, fmt.Sprintf("Failed to decode request: %v", err), http.StatusBadRequest)
		return
	}

	log.V(consts.DebugLogLevel).Info("Received route refresh", "route", route)

	// Handle based on state
	if route.State == v1alpha1.SandboxStateRunning {
		// Update the route
		if registry.GetRegistry().Update(route.ID, route) {
			log.Info("Route updated via refresh", "id", route.ID, "ip", route.IP)
		} else {
			log.V(consts.DebugLogLevel).Info("Route update skipped due to older resourceVersion", "id", route.ID)
		}
	} else {
		// Delete the route if the sandbox is dead
		registry.GetRegistry().Delete(route.ID)
	}

	w.WriteHeader(http.StatusNoContent)
}
