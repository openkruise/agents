package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"
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
	srv     *grpc.Server
	routes  sync.Map
	adapter RequestAdapter
	LBEntry string // entry of load balancer, usually a service
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

func (s *Server) Run() error {
	if s.srv != nil {
		return errors.New("proxy server already started")
	}

	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", consts.ExtProcPort))
	if err != nil {
		return err
	}
	s.srv = grpc.NewServer(grpc.MaxConcurrentStreams(1000))
	extProcPb.RegisterExternalProcessorServer(s.srv, s)
	grpc_health_v1.RegisterHealthServer(s.srv, &healthServer{})
	klog.InfoS("Starting envoy ext-proc gRPC server", "address", lis.Addr())
	return s.srv.Serve(lis)
}

func (s *Server) Stop() {
	if s.srv != nil {
		s.srv.Stop()
	}
}
