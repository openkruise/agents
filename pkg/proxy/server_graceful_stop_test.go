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
	"net"
	"testing"
	"time"

	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func TestStopWaitsForExtProcStreams(t *testing.T) {
	s := &Server{peerOutbound: NewPeerOutbound(nil)}
	s.grpcSrv = grpc.NewServer()
	extProcPb.RegisterExternalProcessorServer(s.grpcSrv, s)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func(srv *grpc.Server) { _ = srv.Serve(lis) }(s.grpcSrv)

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	stream, err := extProcPb.NewExternalProcessorClient(conn).Process(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Let the handler start before stopping the server.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stopped := make(chan struct{})
	go func() {
		s.Stop(ctx)
		close(stopped)
	}()

	select {
	case <-stopped:
		t.Fatal("Stop returned while an ext-proc stream was still open")
	case <-time.After(500 * time.Millisecond):
	}

	if err := stream.CloseSend(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return after the stream finished")
	}
}
