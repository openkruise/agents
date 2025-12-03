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
	"github.com/openkruise/agents/pkg/servers/web"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
)

const (
	RefreshAPI = "/refresh"
	SystemPort = 7789
)

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

// Server implements the Envoy external processing server.
// https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto
type Server struct {
	grpcSrv *grpc.Server
	httpSrv *http.Server
	routes  sync.Map
	adapter RequestAdapter
	LBEntry string // entry of load balancer, usually a service
	Peers   []string
}

func NewServer(adapter RequestAdapter) *Server {
	s := &Server{
		adapter: adapter,
	}
	if adapter != nil {
		s.LBEntry = adapter.Entry()
	}
	return s
}

func (s *Server) SetPeers(peers []string) {
	s.Peers = peers
}

func (s *Server) Run() error {
	if s.grpcSrv != nil || s.httpSrv != nil {
		return errors.New("proxy server already started")
	}

	// HTTP
	mux := http.NewServeMux()
	web.RegisterRoute(mux, fmt.Sprintf("POST %s", RefreshAPI), s.handleRefresh)
	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf(":%d", SystemPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		klog.InfoS("Starting proxy system server", "address", s.httpSrv.Addr)
		if err := s.httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			klog.Fatalf("HTTP server failed to start: %v", err)
		}
	}()

	// GRPC
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", consts.ExtProcPort))
	if err != nil {
		return err
	}
	s.grpcSrv = grpc.NewServer(grpc.MaxConcurrentStreams(1000))
	extProcPb.RegisterExternalProcessorServer(s.grpcSrv, s)
	grpc_health_v1.RegisterHealthServer(s.grpcSrv, &healthServer{})
	klog.InfoS("Starting envoy ext-proc gRPC server", "address", lis.Addr())
	return s.grpcSrv.Serve(lis)
}

func (s *Server) Stop() {
	if s.grpcSrv != nil {
		s.grpcSrv.Stop()
	}
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(context.Background())
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
