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

package main

import (
	"context"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"
	"google.golang.org/grpc/codes"
	healthPb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/status"

	logutil "github.com/openkruise/agents/pkg/traffic-extension/util/logging"
)

type healthServer struct {
	logger logr.Logger
}

func (s *healthServer) Check(ctx context.Context, in *healthPb.HealthCheckRequest) (*healthPb.HealthCheckResponse, error) {
	s.logger.V(logutil.TRACE).Info("gRPC health check serving", "service", in.Service)
	return &healthPb.HealthCheckResponse{Status: healthPb.HealthCheckResponse_SERVING}, nil
}

func (s *healthServer) List(ctx context.Context, _ *healthPb.HealthListRequest) (*healthPb.HealthListResponse, error) {
	serviceHealthResponse, err := s.Check(ctx, &healthPb.HealthCheckRequest{Service: extProcPb.ExternalProcessor_ServiceDesc.ServiceName})
	if err != nil {
		return nil, err
	}

	return &healthPb.HealthListResponse{
		Statuses: map[string]*healthPb.HealthCheckResponse{
			extProcPb.ExternalProcessor_ServiceDesc.ServiceName: serviceHealthResponse,
		},
	}, nil
}

func (s *healthServer) Watch(in *healthPb.HealthCheckRequest, srv healthPb.Health_WatchServer) error {
	return status.Error(codes.Unimplemented, "Watch is not implemented")
}
