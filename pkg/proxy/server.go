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
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/sandboxroute/refresh"
	"github.com/openkruise/agents/pkg/utils/network"
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

// Peer is kept for backward compatibility, but now uses peers.Peer from pkg/peers
type Peer = peers.Peer

// Server implements the Envoy external processing server.
// https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto
type Server struct {
	// grpc
	grpcSrv                     *grpc.Server
	extProcMaxConcurrentStreams uint32
	disableEnvoyExtProc         bool
	// http
	httpSrv *http.Server
	// internal
	store   *sandboxroute.Store
	adapter RequestAdapter
	LBEntry string // entry of load balancer, usually a service
	// peers - now managed by Peers
	peersManager peers.Peers
	bindAddress  string
	// lifecycle: Run is called once and Stop at most once, after Run.
	mu sync.Mutex
}

func NewServer(opts config.SandboxManagerOptions) *Server {
	store := sandboxroute.NewStore()
	return &Server{
		extProcMaxConcurrentStreams: opts.ExtProcMaxConcurrency,
		disableEnvoyExtProc:         opts.DisableEnvoyExtProc,
		store:                       store,
		bindAddress:                 opts.BindAddress,
	}
}

func (s *Server) SetRequestAdapter(adapter RequestAdapter) {
	s.adapter = adapter
	s.LBEntry = adapter.Entry()
}

func (s *Server) SetPeersManager(p peers.Peers) {
	s.peersManager = p
}

// Run binds the route-refresh HTTP listener and, unless disabled, the Envoy
// ext-proc gRPC listener, then serves both in the background. Bind failures
// are returned synchronously. Listeners are owned by their servers: Shutdown
// and Stop close them, as does Serve when it observes a stopped server.
func (s *Server) Run() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	httpSrv := &http.Server{
		Addr:              network.ListenAddress(s.bindAddress, refresh.DefaultPort),
		Handler:           s.newServeMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	httpLis, err := net.Listen("tcp", httpSrv.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen for proxy route updates on %s: %w", httpSrv.Addr, err)
	}

	var grpcSrv *grpc.Server
	var grpcLis net.Listener
	if s.disableEnvoyExtProc {
		klog.InfoS("Envoy ext-proc gRPC listener disabled")
	} else {
		extProcAddr := network.ListenAddress("", consts.ExtProcPort)
		grpcLis, err = net.Listen("tcp", extProcAddr)
		if err != nil {
			if closeErr := httpLis.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				err = errors.Join(err, fmt.Errorf("close proxy route listener: %w", closeErr))
			}
			return fmt.Errorf("failed to listen for envoy ext-proc on %s: %w", extProcAddr, err)
		}
		grpcSrv = grpc.NewServer(grpc.MaxConcurrentStreams(s.extProcMaxConcurrentStreams))
		extProcPb.RegisterExternalProcessorServer(grpcSrv, s)
		grpc_health_v1.RegisterHealthServer(grpcSrv, &healthServer{})
	}

	s.httpSrv = httpSrv
	s.grpcSrv = grpcSrv

	go func(srv *http.Server, lis net.Listener) {
		klog.InfoS("Starting proxy system server", "address", lis.Addr())
		if err := srv.Serve(lis); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			klog.Fatalf("HTTP server failed to start: %v", err)
		}
	}(httpSrv, httpLis)

	if grpcSrv != nil {
		go func(srv *grpc.Server, lis net.Listener) {
			klog.InfoS("Starting proxy gRPC server", "address", lis.Addr())
			if err := srv.Serve(lis); err != nil && !errors.Is(err, grpc.ErrServerStopped) && !errors.Is(err, net.ErrClosed) {
				klog.Fatalf("gRPC server failed to start: %v", err)
			}
		}(grpcSrv, grpcLis)
	}

	return nil
}

func (s *Server) newServeMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle(
		http.MethodPost+" "+refresh.Path,
		refresh.NewHandler(s.store, func(result sandboxroute.MutationResult) {
			switch result.Result {
			case sandboxroute.EventResultApplied, sandboxroute.EventResultIgnored:
				s.updateRouteCount()
			}
		}),
	)
	return mux
}

func (s *Server) Stop(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grpcSrv != nil {
		s.grpcSrv.Stop()
	}
	if s.httpSrv != nil {
		if err := s.httpSrv.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			klog.ErrorS(err, "Failed to shut down proxy system server")
		}
	}
}

func (s *Server) updateRouteCount() {
	routeCount.Set(float64(s.store.Len()))
}
