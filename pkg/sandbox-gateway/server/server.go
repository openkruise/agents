package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils"
)

// Environment variable names for peer discovery
const (
	EnvNamespace     = "PEER_NAMESPACE"
	EnvLabelSelector = "PEER_LABEL_SELECTOR"
)

const (
	// MemberlistBindPort is the default port for memberlist gossip
	MemberlistBindPort = 7946
)

// Server handles peer-to-peer communication for route synchronization
type Server struct {
	httpServer  *http.Server
	peerManager *peers.MemberlistPeers
	port        int
	client      kubernetes.Interface
}

// NewServer creates a new peer server
func NewServer(client kubernetes.Interface, port int) *Server {
	if port == 0 {
		port = proxy.SystemPort
	}
	return &Server{
		port:   port,
		client: client,
	}
}

// Start starts the HTTP server for handling refresh requests from peers
func (s *Server) Start(ctx context.Context) error {
	log := klog.FromContext(ctx)

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
	localIP := utils.GetFirstNonLoopbackIP()
	if localIP == "" {
		return fmt.Errorf("failed to determine local IP")
	}

	// Get namespace and label selector from environment variables
	namespace := os.Getenv(EnvNamespace)
	labelSelector := os.Getenv(EnvLabelSelector)

	// Discover existing peers from Kubernetes API
	var existingPeers []string
	if s.client != nil && namespace != "" && labelSelector != "" {
		log.Info("discovering existing peers for memberlist join", "namespace", namespace, "selector", labelSelector)
		peerList, err := s.client.CoreV1().Pods(namespace).List(ctx, v1.ListOptions{
			LabelSelector: labelSelector,
		})
		if err != nil {
			log.Error(err, "failed to list peer pods, continuing without existing peers")
		} else {
			for _, peer := range peerList.Items {
				ip := peer.Status.PodIP
				if ip == "" || ip == localIP {
					continue
				}
				existingPeers = append(existingPeers, fmt.Sprintf("%s:%d", ip, MemberlistBindPort))
			}
			log.Info("found existing peers for memberlist join", "count", len(existingPeers))
		}
	} else {
		log.Info("skipping peer discovery: client not available or namespace/labelSelector not set")
	}

	s.peerManager = peers.NewMemberlistPeers(nodeName)

	if err := s.peerManager.Start(ctx, localIP, MemberlistBindPort, existingPeers); err != nil {
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
