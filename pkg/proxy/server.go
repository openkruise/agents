package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/logs"
	"github.com/openkruise/agents/pkg/servers/web"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	RefreshAPI = "/refresh"
	HelloAPI   = "/hello"
	SystemPort = 7789
)

var HeartBeatInterval = 5 * time.Second

type healthServer struct{}

func (s *healthServer) List(context.Context, *grpc_health_v1.HealthListRequest) (*grpc_health_v1.HealthListResponse, error) {
	return &grpc_health_v1.HealthListResponse{
		Statuses: map[string]*grpc_health_v1.HealthCheckResponse{
			"envoy-ext-proc": {Status: grpc_health_v1.HealthCheckResponse_SERVING},
		},
	}, nil
}

func (s *healthServer) Check(context.Context, *grpc_health_v1.HealthCheckRequest) (
	*grpc_health_v1.HealthCheckResponse, error) {
	return &grpc_health_v1.HealthCheckResponse{Status: grpc_health_v1.HealthCheckResponse_SERVING}, nil
}

func (s *healthServer) Watch(*grpc_health_v1.HealthCheckRequest, grpc_health_v1.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}

type Peer struct {
	IP            string
	LastHeartBeat time.Time
}

// Server implements the Envoy external processing server.
// https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto
type Server struct {
	grpcSrv *grpc.Server
	httpSrv *http.Server
	routes  sync.Map
	adapter RequestAdapter
	LBEntry string // entry of load balancer, usually a service
	// peers
	peers           map[string]Peer
	peerMu          sync.RWMutex
	heartBeatTicker *time.Ticker
	heartBeatStopCh chan struct{}
}

func NewServer(adapter RequestAdapter) *Server {
	s := &Server{
		adapter:         adapter,
		peers:           make(map[string]Peer),
		heartBeatStopCh: make(chan struct{}),
	}
	if adapter != nil {
		s.LBEntry = adapter.Entry()
	}
	return s
}

func (s *Server) SetPeer(ip string) {
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	s.peers[ip] = Peer{
		IP:            ip,
		LastHeartBeat: time.Now(),
	}
}

func (s *Server) Run() error {
	if s.grpcSrv != nil || s.httpSrv != nil {
		return errors.New("proxy server already started")
	}

	s.peerMu.Lock()

	// HTTP
	mux := http.NewServeMux()
	web.RegisterRoute(mux, http.MethodPost, RefreshAPI, s.handleRefresh)
	web.RegisterRoute(mux, http.MethodGet, HelloAPI, s.handleHello)
	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf(":%d", SystemPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	s.heartBeatTicker = time.NewTicker(HeartBeatInterval)

	// GRPC
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", consts.ExtProcPort))
	if err != nil {
		return err
	}
	s.grpcSrv = grpc.NewServer(grpc.MaxConcurrentStreams(1000))
	extProcPb.RegisterExternalProcessorServer(s.grpcSrv, s)
	grpc_health_v1.RegisterHealthServer(s.grpcSrv, &healthServer{})
	klog.InfoS("Starting envoy ext-proc gRPC server", "address", lis.Addr())

	s.peerMu.Unlock()

	// Start servers
	go func() {
		klog.InfoS("Starting proxy system server", "address", s.httpSrv.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Fatalf("HTTP server failed to start: %v", err)
		}
	}()

	go func() {
		klog.InfoS("Starting proxy gRPC server", "address", lis.Addr())
		if err := s.grpcSrv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			klog.Fatalf("gRPC server failed to start: %v", err)
		}
	}()

	go func(ctx context.Context) {
		log := klog.FromContext(ctx).V(consts.DebugLogLevel)
		for {
			select {
			case <-s.heartBeatTicker.C:
				s.peerMu.Lock()
				peersToCheck := make([]Peer, 0, len(s.peers))
				peersToDelete := make([]string, 0)

				for ip, peer := range s.peers {
					if time.Since(peer.LastHeartBeat) > 5*HeartBeatInterval {
						peersToDelete = append(peersToDelete, ip)
					} else {
						peersToCheck = append(peersToCheck, peer)
					}
				}
				s.peerMu.Unlock()

				if len(peersToDelete) > 0 {
					s.peerMu.Lock()
					for _, ip := range peersToDelete {
						delete(s.peers, ip)
						log.Info("peer deleted for heartbeat timeout", "ip", ip)
					}
					s.peerMu.Unlock()
				}

				for _, peer := range peersToCheck {
					if err := s.HelloPeer(peer.IP); err != nil {
						log.Error(err, "failed to send heartbeat to peer", "ip", peer.IP)
					}
				}
			case <-s.heartBeatStopCh:
				return
			}

		}
	}(logs.NewContext("component", "PeerHeartBeat"))

	return nil
}

func (s *Server) HelloPeer(ip string) error {
	return requestPeer(http.MethodGet, ip, HelloAPI, nil)
}

func (s *Server) Stop() {
	s.peerMu.Lock()
	defer s.peerMu.Unlock()
	close(s.heartBeatStopCh)
	s.heartBeatTicker.Stop()
	if s.grpcSrv != nil {
		s.grpcSrv.Stop()
	}
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(context.Background())
	}
}

func (s *Server) handleHello(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	log := klog.FromContext(r.Context()).V(LogLevel + 1)
	ip := getRealIP(r)
	if ip != "" {
		log.Info("hello", "ip", ip)
		s.SetPeer(ip)
		return web.ApiResponse[struct{}]{
			Code: http.StatusNoContent,
		}, nil
	} else {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "failed to get your ip",
		}
	}
}

func (s *Server) handleRefresh(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	var route Route
	if err := json.NewDecoder(r.Body).Decode(&route); err != nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("failed to unmarshal body: %s", err.Error()),
		}
	}
	s.SetRoute(route)
	log.Info("route refreshed", "route", route)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
