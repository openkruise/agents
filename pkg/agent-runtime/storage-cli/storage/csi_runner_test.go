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

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
)

// nopCloser is an io.Closer that always succeeds. It is used to wrap fake
// CSI clients that do not own a real network connection.
type nopCloser struct {
	closed atomic.Bool
}

func (c *nopCloser) Close() error {
	c.closed.Store(true)
	return nil
}

// fakeNodeClient is a minimal csi.NodeClient stub. Only NodePublishVolume is
// exercised; the remaining methods are unimplemented and will panic if a
// test accidentally calls them.
type fakeNodeClient struct {
	csi.NodeClient

	gotReq  *csi.NodePublishVolumeRequest
	gotCtx  context.Context
	resp    *csi.NodePublishVolumeResponse
	err     error
	calls   atomic.Int32
	onCall  func(ctx context.Context)
}

func (f *fakeNodeClient) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest, _ ...grpc.CallOption) (*csi.NodePublishVolumeResponse, error) {
	f.calls.Add(1)
	f.gotReq = req
	f.gotCtx = ctx
	if f.onCall != nil {
		f.onCall(ctx)
	}
	return f.resp, f.err
}

// withClientFactory swaps newClientFn for the duration of fn.
func withClientFactory(t *testing.T, factory func(socketPath string) (csi.NodeClient, io.Closer, error)) {
	t.Helper()
	orig := newClientFn
	newClientFn = factory
	t.Cleanup(func() { newClientFn = orig })
}

func TestRunNodePublishVolume(t *testing.T) {
	const driver = "fake.csi.example.com"
	expectedSocketPath := path.Join(CsiSocketDir, driver, CsiSocketFile)

	tests := []struct {
		name        string
		ctx         func() (context.Context, context.CancelFunc)
		factoryErr  error
		rpcResp     *csi.NodePublishVolumeResponse
		rpcErr      error
		onCall      func(ctx context.Context)
		expectError string
		assertCalls func(t *testing.T, fake *fakeNodeClient, closer *nopCloser)
	}{
		{
			name:    "success closes connection and forwards request",
			ctx:     func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			rpcResp: &csi.NodePublishVolumeResponse{},
			assertCalls: func(t *testing.T, fake *fakeNodeClient, closer *nopCloser) {
				assert.Equal(t, int32(1), fake.calls.Load())
				assert.NotNil(t, fake.gotReq)
				assert.Equal(t, "vol-1", fake.gotReq.VolumeId)
				assert.True(t, closer.closed.Load(), "closer must be invoked")
			},
		},
		{
			name:        "factory error wraps driver name",
			ctx:         func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			factoryErr:  errors.New("dial refused"),
			expectError: `create CSI client for driver "fake.csi.example.com"`,
		},
		{
			name:        "rpc error wraps driver name",
			ctx:         func() (context.Context, context.CancelFunc) { return context.Background(), func() {} },
			rpcErr:      errors.New("permission denied"),
			expectError: `NodePublishVolume failed for driver "fake.csi.example.com"`,
			assertCalls: func(t *testing.T, fake *fakeNodeClient, closer *nopCloser) {
				assert.Equal(t, int32(1), fake.calls.Load())
				assert.True(t, closer.closed.Load(), "closer must run on error")
			},
		},
		{
			name: "rpc respects parent context cancellation",
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel() // already cancelled when the call begins
				return ctx, func() {}
			},
			onCall: func(ctx context.Context) {
				// echo the parent ctx error so the test can assert it propagated.
			},
			rpcErr:      context.Canceled,
			expectError: "NodePublishVolume failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeNodeClient{
				resp:   tt.rpcResp,
				err:    tt.rpcErr,
				onCall: tt.onCall,
			}
			closer := &nopCloser{}
			factoryErr := tt.factoryErr
			withClientFactory(t, func(socketPath string) (csi.NodeClient, io.Closer, error) {
				assert.Equal(t, expectedSocketPath, socketPath, "socket path must follow CsiSocketDir/<driver>/CsiSocketFile")
				if factoryErr != nil {
					return nil, nil, factoryErr
				}
				return fake, closer, nil
			})

			ctx, cancel := tt.ctx()
			defer cancel()

			err := RunNodePublishVolume(ctx, driver, csi.NodePublishVolumeRequest{VolumeId: "vol-1"}, false)

			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			if tt.assertCalls != nil {
				tt.assertCalls(t, fake, closer)
			}
		})
	}
}

// TestRunNodePublishVolumeAppliesTimeout verifies that the per-call timeout
// derived from nodePublishVolumeTimeout is layered on top of the parent ctx.
func TestRunNodePublishVolumeAppliesTimeout(t *testing.T) {
	fake := &fakeNodeClient{
		resp: &csi.NodePublishVolumeResponse{},
		onCall: func(ctx context.Context) {
			deadline, ok := ctx.Deadline()
			assert.True(t, ok, "RunNodePublishVolume must set a deadline on the call ctx")
			assert.False(t, deadline.IsZero())
		},
	}
	withClientFactory(t, func(_ string) (csi.NodeClient, io.Closer, error) {
		return fake, &nopCloser{}, nil
	})

	err := RunNodePublishVolume(context.Background(), "fake.csi.example.com", csi.NodePublishVolumeRequest{}, false)
	assert.NoError(t, err)
	assert.Equal(t, int32(1), fake.calls.Load())
}

// TestNewCSIClient exercises the real grpc.Dial path. Dial is lazy in gRPC,
// so it returns a usable client even when the underlying socket file does
// not exist; we only assert the happy-path invariants here.
func TestNewCSIClient(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "csi.sock")

	client, conn, err := newCSIClient(socketPath)
	assert.NoError(t, err)
	assert.NotNil(t, client)
	assert.NotNil(t, conn)
	t.Cleanup(func() {
		assert.NoError(t, conn.Close())
	})
}

// fakeCSINodeServer is a minimal NodeServer that records the last
// NodePublishVolume request and returns a configurable response. Methods
// other than NodePublishVolume are inherited from UnimplementedNodeServer.
type fakeCSINodeServer struct {
	csi.UnimplementedNodeServer

	gotReq atomic.Pointer[csi.NodePublishVolumeRequest]
}

func (s *fakeCSINodeServer) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	s.gotReq.Store(req)
	return &csi.NodePublishVolumeResponse{}, nil
}

// startTestCSIServer starts a real gRPC NodeServer on a unix socket and
// returns the socket path plus the fake server. The server is stopped via
// t.Cleanup. macOS imposes a ~104 byte limit on unix socket paths, so the
// socket is placed under /tmp rather than t.TempDir() to keep the path
// short and stable.
func startTestCSIServer(t *testing.T) (string, *fakeCSINodeServer) {
	t.Helper()

	socketPath := filepath.Join("/tmp", fmt.Sprintf("csi-runner-test-%d-%d.sock", os.Getpid(), time.Now().UnixNano()))
	// Pre-existing stale socket would make Listen fail with EADDRINUSE.
	_ = os.Remove(socketPath)

	lis, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix %q: %v", socketPath, err)
	}

	srv := grpc.NewServer()
	fake := &fakeCSINodeServer{}
	csi.RegisterNodeServer(srv, fake)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(lis)
	}()

	t.Cleanup(func() {
		srv.GracefulStop()
		<-done
		_ = os.Remove(socketPath)
	})

	return socketPath, fake
}

// TestNewCSIClientEndToEnd starts a real grpc NodeServer on a unix socket
// and issues a NodePublishVolume RPC through the client returned by
// newCSIClient. This exercises the WithContextDialer closure that is only
// invoked on the first RPC, which the lazy-dial happy path in
// TestNewCSIClient cannot reach.
func TestNewCSIClientEndToEnd(t *testing.T) {
	socketPath, fake := startTestCSIServer(t)

	client, conn, err := newCSIClient(socketPath)
	assert.NoError(t, err)
	assert.NotNil(t, client)
	assert.NotNil(t, conn)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{VolumeId: "vol-e2e"}, grpc.WaitForReady(true))
	assert.NoError(t, err)
	assert.NotNil(t, resp)

	got := fake.gotReq.Load()
	if assert.NotNil(t, got, "server must have received the request via the dialer closure") {
		assert.Equal(t, "vol-e2e", got.VolumeId)
	}
}

// TestDefaultNewClientFn verifies the package-level default newClientFn
// (which wraps newCSIClient) returns a usable client + closer pair against
// a real unix socket. This guards against accidental reassignment of
// newClientFn in production code paths.
func TestDefaultNewClientFn(t *testing.T) {
	socketPath, fake := startTestCSIServer(t)

	client, closer, err := newClientFn(socketPath)
	assert.NoError(t, err)
	assert.NotNil(t, client)
	assert.NotNil(t, closer)
	defer func() { assert.NoError(t, closer.Close()) }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = client.NodePublishVolume(ctx, &csi.NodePublishVolumeRequest{VolumeId: "vol-default"}, grpc.WaitForReady(true))
	assert.NoError(t, err)
	assert.NotNil(t, fake.gotReq.Load())
}

// TestRunNodePublishVolumeBuildsSocketPath ensures the generated socket path
// matches the kubelet CSI layout regardless of driver name shape.
func TestRunNodePublishVolumeBuildsSocketPath(t *testing.T) {
	tests := []struct {
		name   string
		driver string
		want   string
	}{
		{
			name:   "fqdn driver",
			driver: "fake.csi.example.com",
			want:   path.Join(CsiSocketDir, "fake.csi.example.com", CsiSocketFile),
		},
		{
			name:   "single segment driver",
			driver: "fake",
			want:   path.Join(CsiSocketDir, "fake", CsiSocketFile),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seenPath string
			withClientFactory(t, func(socketPath string) (csi.NodeClient, io.Closer, error) {
				seenPath = socketPath
				return &fakeNodeClient{resp: &csi.NodePublishVolumeResponse{}}, &nopCloser{}, nil
			})

			err := RunNodePublishVolume(context.Background(), tt.driver, csi.NodePublishVolumeRequest{}, false)
			assert.NoError(t, err)
			assert.Equal(t, tt.want, seenPath)
			// Sanity-check the layout: the socket must live under CsiSocketDir.
			assert.True(t, strings.HasPrefix(seenPath, CsiSocketDir+"/"))
			assert.True(t, strings.HasSuffix(seenPath, "/"+CsiSocketFile))
		})
	}
}
