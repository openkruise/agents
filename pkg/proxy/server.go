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
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/peers"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
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

// Peer is kept for backward compatibility, but now uses peers.Peer from pkg/peers
type Peer = peers.Peer

// Server implements the Envoy external processing server.
// https://www.envoyproxy.io/docs/envoy/latest/api-v3/service/ext_proc/v3/external_processor.proto
type Server struct {
	// grpc
	grpcSrv                     *grpc.Server
	extProcMaxConcurrentStreams uint32
	// http
	httpSrv *http.Server
	// internal
	routes  sync.Map
	adapter RequestAdapter
	LBEntry string // entry of load balancer, usually a service
	// peers - now managed by Peers
	peersManager peers.Peers
	// lifecycle
	mu sync.Mutex
}

func NewServer(adapter RequestAdapter, peersManager peers.Peers, opts config.SandboxManagerOptions) *Server {
	s := &Server{
		adapter:                     adapter,
		peersManager:                peersManager,
		extProcMaxConcurrentStreams: opts.ExtProcMaxConcurrency,
	}
	if adapter != nil {
		s.LBEntry = adapter.Entry()
	}
	return s
}

func (s *Server) Run() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// HTTP
	mux := http.NewServeMux()
	web.RegisterRoute(mux, http.MethodPost, RefreshAPI, s.handleRefresh)
	s.httpSrv = &http.Server{
		Addr:              fmt.Sprintf(":%d", SystemPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// GRPC
	lis, err := net.Listen("tcp", fmt.Sprintf(":%d", consts.ExtProcPort))
	if err != nil {
		return err
	}
	s.grpcSrv = grpc.NewServer(grpc.MaxConcurrentStreams(s.extProcMaxConcurrentStreams))
	extProcPb.RegisterExternalProcessorServer(s.grpcSrv, s)
	grpc_health_v1.RegisterHealthServer(s.grpcSrv, &healthServer{})
	klog.InfoS("Starting envoy ext-proc gRPC server", "address", lis.Addr())

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

	return nil
}

func (s *Server) Stop(ctx context.Context) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.grpcSrv != nil {
		s.grpcSrv.Stop()
	}
	if s.httpSrv != nil {
		_ = s.httpSrv.Shutdown(ctx)
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
	if route.State == v1alpha1.SandboxStateDead {
		s.DeleteRoute(route.ID)
		log.Info("route deleted")
	} else {
		s.SetRoute(ctx, route)
		log.Info("route refreshed", "route", route)
	}
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
