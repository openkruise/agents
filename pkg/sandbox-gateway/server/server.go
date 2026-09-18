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
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/peersecurity"
	"github.com/openkruise/agents/pkg/proxy"
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

// Environment variable names for peer security. They mirror the sandbox-manager
// --peer-* flags.
const (
	envPeerKeySecret        = "PEER_KEY_SECRET"        // #nosec G101 -- env-var name, not a credential
	envPeerKeySecretKey     = "PEER_KEY_SECRET_KEY"    // #nosec G101 -- env-var name, not a credential
	envPeerTLSServerSecret  = "PEER_TLS_SERVER_SECRET" // #nosec G101 -- env-var name, not a credential
	envPeerTLSClientSecret  = "PEER_TLS_CLIENT_SECRET" // #nosec G101 -- env-var name, not a credential
	envPeerTLSServerCAKey   = "PEER_TLS_SERVER_CA_KEY"
	envPeerTLSServerCertKey = "PEER_TLS_SERVER_CERT_KEY"
	envPeerTLSServerKeyKey  = "PEER_TLS_SERVER_KEY_KEY"
	envPeerTLSClientCAKey   = "PEER_TLS_CLIENT_CA_KEY"
	envPeerTLSClientCertKey = "PEER_TLS_CLIENT_CERT_KEY"
	envPeerTLSClientKeyKey  = "PEER_TLS_CLIENT_KEY_KEY"
)

// ReadinessCheck reports whether the gateway is ready to receive traffic.
type ReadinessCheck func() error

// globalPeerRuntime is set when server.Start() creates the peer manager and
// outbound client. It allows other packages (e.g. wake) to use this process's
// peer owner without creating a full Server instance.
// Protected by globalPeerRuntimeMu for concurrent read/write safety.
var (
	globalPeerRuntimeMu sync.RWMutex
	globalPeerManager   peers.Peers
	globalPeerOutbound  *proxy.PeerOutbound
)

func setPeerRuntime(pm peers.Peers, outbound *proxy.PeerOutbound) {
	globalPeerRuntimeMu.Lock()
	defer globalPeerRuntimeMu.Unlock()
	globalPeerManager = pm
	globalPeerOutbound = outbound
}

// GetPeerManager returns the peer manager for use by the wake package.
// Returns nil if the server has not been started yet.
func GetPeerManager() peers.Peers {
	globalPeerRuntimeMu.RLock()
	defer globalPeerRuntimeMu.RUnlock()
	return globalPeerManager
}

// GetPeerOutbound returns this process's outbound peer client for use by the
// wake package. Returns nil if the server has not been started yet.
func GetPeerOutbound() *proxy.PeerOutbound {
	globalPeerRuntimeMu.RLock()
	defer globalPeerRuntimeMu.RUnlock()
	return globalPeerOutbound
}

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

// peerSecurityFromEnv parses the Gateway peer-security environment variables.
// They mirror the sandbox-manager flags and keep the same defaults and
// enablement rules: a feature is enabled only by its own Secret reference, data
// keys are read only while that reference is set, and both TLS references empty
// keeps plaintext peer HTTP. The server variables are the credentials this
// process presents on inbound peer HTTPS, and PEER_TLS_SERVER_CA_KEY is the
// trust anchor verifying inbound peer client certificates. The client variables
// are this process's own runtime client bundle used for outbound peer HTTPS
// (never the sandbox manager's bundle), and PEER_TLS_CLIENT_CA_KEY is the trust
// anchor verifying outbound peer server certificates.
func peerSecurityFromEnv() (peersecurity.Inputs, error) {
	keySecret, err := utils.ParseSecretRef(os.Getenv(envPeerKeySecret))
	if err != nil {
		return peersecurity.Inputs{}, fmt.Errorf("invalid %s: %w", envPeerKeySecret, err)
	}
	serverSecret, err := utils.ParseSecretRef(os.Getenv(envPeerTLSServerSecret))
	if err != nil {
		return peersecurity.Inputs{}, fmt.Errorf("invalid %s: %w", envPeerTLSServerSecret, err)
	}
	clientSecret, err := utils.ParseSecretRef(os.Getenv(envPeerTLSClientSecret))
	if err != nil {
		return peersecurity.Inputs{}, fmt.Errorf("invalid %s: %w", envPeerTLSClientSecret, err)
	}
	in := peersecurity.Inputs{
		PeerKeySecret:     keySecret,
		PeerKeyDataKey:    os.Getenv(envPeerKeySecretKey),
		TLSServerSecret:   serverSecret,
		ServerCADataKey:   os.Getenv(envPeerTLSServerCAKey),
		ServerCertDataKey: os.Getenv(envPeerTLSServerCertKey),
		ServerKeyDataKey:  os.Getenv(envPeerTLSServerKeyKey),
		TLSClientSecret:   clientSecret,
		ClientCADataKey:   os.Getenv(envPeerTLSClientCAKey),
		ClientCertDataKey: os.Getenv(envPeerTLSClientCertKey),
		ClientKeyDataKey:  os.Getenv(envPeerTLSClientKeyKey),
	}
	in.ApplyDefaults()
	if err := in.Validate(); err != nil {
		return peersecurity.Inputs{}, fmt.Errorf("invalid peer security environment variables: %w", err)
	}
	return in, nil
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
	peerServerTLS      *tls.Config
	peerOutbound       *proxy.PeerOutbound
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
	s.httpServer = &http.Server{
		Addr:              fmt.Sprintf(":%d", normalizePort(s.port, refresh.DefaultPort)),
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

	inputs, err := peerSecurityFromEnv()
	if err != nil {
		return err
	}
	// Load owns the enablement decision: inputs without Secret references
	// return plaintext materials without reading any Secret.
	loadCtx, cancel := context.WithTimeout(ctx, peersecurity.LoadTimeout)
	secretKey, serverTLS, clientTLS, err := peersecurity.Load(loadCtx, s.client, inputs)
	cancel()
	if err != nil {
		return fmt.Errorf("load peer security: %w", err)
	}
	if serverTLS != nil {
		// Client certificates stay optional at the handshake because this
		// listener also serves the health probes, which cannot present one.
		// A certificate that is sent must still verify, and newServeMux
		// enforces a verified certificate on the refresh route.
		cfg := serverTLS.Clone()
		cfg.ClientAuth = tls.VerifyClientCertIfGiven
		s.peerServerTLS = cfg
	}
	s.peerOutbound = proxy.NewPeerOutbound(clientTLS)
	s.httpServer.Handler = s.newServeMux(s.peerServerTLS != nil)

	lis, err := net.Listen("tcp", s.httpServer.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen for peer route updates on %s: %w", s.httpServer.Addr, err)
	}
	if s.peerServerTLS != nil {
		lis = tls.NewListener(lis, s.peerServerTLS)
	}

	// Get namespace and label selector from environment variables
	namespace := os.Getenv(EnvNamespace)
	labelSelector := os.Getenv(EnvLabelSelector)

	s.peerManager = peers.NewMemberlistPeers(s.client, peers.NodePrefixSandboxGateway+nodeName, namespace, labelSelector)
	s.peerManager.SetSecretKey(secretKey)
	setPeerRuntime(s.peerManager, s.peerOutbound)

	go func() {
		klog.InfoS("Starting sandbox-gateway peer server", "address", lis.Addr().String())
		if err := s.httpServer.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.ErrorS(err, "Peer server failed to start")
		}
	}()

	if err := s.peerManager.Start(ctx, "", s.memberlistBindPort); err != nil {
		_ = s.httpServer.Shutdown(ctx)
		return err
	}

	return nil
}

// newServeMux registers the peer routes served over the TLS-terminated
// listener. Peer TLS asks for a client certificate but does not require one, so
// an unauthenticated request would otherwise reach every route;
// requireClientCert (set whenever peer TLS is enabled) moves the certificate
// requirement into the mux. The refresh route is wrapped in
// requireVerifiedClientCert and rejects a request without a verified
// certificate before the body is read or a route is mutated, while the GET
// health paths stay reachable without a certificate so probes work. Without
// peer TLS every route is plaintext.
func (s *Server) newServeMux(requireClientCert bool) *http.ServeMux {
	mux := http.NewServeMux()
	refreshHandler := refresh.NewHandler(s.registry, nil)
	if requireClientCert {
		refreshHandler = requireVerifiedClientCert(refreshHandler)
	}
	mux.Handle(
		http.MethodPost+" "+refresh.Path,
		refreshHandler,
	)
	mux.HandleFunc(HealthAPI, s.handleHealth)
	mux.HandleFunc(ReadyAPI, s.handleReady)
	return mux
}

func requireVerifiedClientCert(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 {
			http.Error(w, "client certificate required", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
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
		if err := s.peerManager.Stop(ctx); err != nil {
			errs = append(errs, err)
		}
		setPeerRuntime(nil, nil)
	}
	s.peerOutbound.CloseIdleConnections()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
