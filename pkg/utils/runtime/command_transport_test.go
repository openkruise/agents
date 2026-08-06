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

package runtime

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

func TestIsMissingGRPCStatusTrailer(t *testing.T) {
	missingTrailer := func(code connect.Code) error {
		return connect.NewError(code, errors.New("protocol error: no Grpc-Status trailer: unexpected EOF"))
	}

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "unrelated plain error",
			err:  errors.New("mount failed"),
			want: false,
		},
		{
			name: "no trailer at all is reported as internal",
			err:  missingTrailer(connect.CodeInternal),
			want: true,
		},
		{
			name: "trailers without grpc-status are reported as unknown",
			err:  missingTrailer(connect.CodeUnknown),
			want: true,
		},
		{
			name: "same message under an unrelated code is not a trailer problem",
			err:  missingTrailer(connect.CodeUnavailable),
			want: false,
		},
		{
			name: "wrapped with %w",
			err:  fmt.Errorf("failed to run command: %w", missingTrailer(connect.CodeInternal)),
			want: true,
		},
		{
			name: "joined the way RunCommandWithRuntime joins its errors",
			err:  errors.Join(nil, missingTrailer(connect.CodeInternal)),
			want: true,
		},
		{
			name: "real server error keeps failing the call",
			err:  connect.NewError(connect.CodeInternal, errors.New("internal server error")),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, IsMissingGRPCStatusTrailer(tt.err))
		})
	}
}

// newTrailerStrippingRuntimeServer serves the Process handler behind a middleware that
// drops the gRPC trailers connect-go staged for the response, reproducing a peer (or an
// intermediate hop) that terminates the response body cleanly but never delivers
// grpc-status. It returns a Sandbox pointing at that server.
func newTrailerStrippingRuntimeServer(t *testing.T, handler *mockProcessHandler) *agentsv1alpha1.Sandbox {
	t.Helper()
	_, processHandler := processconnect.NewProcessHandler(handler)
	stripped := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		processHandler.ServeHTTP(w, r)
		// connect stages gRPC trailers as http.TrailerPrefix-prefixed header keys, and
		// net/http only serializes them once the handler has returned. Deleting them here
		// is exactly what a trailer-unaware hop does to the wire.
		for key := range w.Header() {
			if strings.HasPrefix(key, http.TrailerPrefix) {
				w.Header().Del(key)
			}
		}
	})
	server := httptest.NewServer(stripped)
	t.Cleanup(server.Close)
	return newTestSandboxWithURL(server.URL)
}

func TestRunCommandWithRuntime_MissingGRPCStatusTrailer(t *testing.T) {
	sendStart := func(stream *connect.ServerStream[process.StartResponse], pid uint32) error {
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
			Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: pid}},
		}})
	}
	sendEnd := func(stream *connect.ServerStream[process.StartResponse], end *process.ProcessEvent_EndEvent) error {
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
			Event: &process.ProcessEvent_End{End: end},
		}})
	}

	tests := []struct {
		name            string
		startFn         func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error
		wantErr         bool
		errContains     string
		errNotContains  string
		wantEndReceived bool
		wantExitCode    int32
	}{
		{
			name: "end event received: the lost trailer is tolerated",
			startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := sendStart(stream, 42); err != nil {
					return err
				}
				return sendEnd(stream, &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true})
			},
			wantEndReceived: true,
		},
		{
			name: "end event with non-zero exit code is preserved for the caller to judge",
			startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := sendStart(stream, 43); err != nil {
					return err
				}
				return sendEnd(stream, &process.ProcessEvent_EndEvent{ExitCode: 2, Exited: true})
			},
			wantEndReceived: true,
			wantExitCode:    2,
		},
		{
			name: "process error from the end event still fails the call",
			startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := sendStart(stream, 44); err != nil {
					return err
				}
				return sendEnd(stream, &process.ProcessEvent_EndEvent{
					ExitCode: 1, Exited: true, Error: ptr.To("segmentation fault"),
				})
			},
			wantErr:         true,
			errContains:     "segmentation fault",
			errNotContains:  missingGRPCStatusTrailerMarker,
			wantEndReceived: true,
			wantExitCode:    1,
		},
		{
			name: "no end event: the protocol error must surface",
			startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				return sendStart(stream, 45)
			},
			wantErr:     true,
			errContains: missingGRPCStatusTrailerMarker,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sbx := newTrailerStrippingRuntimeServer(t, &mockProcessHandler{startFn: tt.startFn})

			result, err := RunCommandWithRuntime(context.Background(), RunCmdFuncArgs{
				Sbx:           sbx,
				ProcessConfig: &process.ProcessConfig{Cmd: "echo"},
				Timeout:       5 * time.Second,
			})

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				if tt.errNotContains != "" {
					assert.NotContains(t, err.Error(), tt.errNotContains)
				}
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.wantEndReceived, result.EndReceived)
			assert.Equal(t, tt.wantExitCode, result.ExitCode)
		})
	}
}

func TestRunCommandWithRuntime_DoesNotReuseConnections(t *testing.T) {
	handler := &mockProcessHandler{
		startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}},
			}})
		},
	}
	_, processHandler := processconnect.NewProcessHandler(handler)
	server := httptest.NewUnstartedServer(processHandler)
	var acceptedConns int32
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			atomic.AddInt32(&acceptedConns, 1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	sbx := newTestSandboxWithURL(server.URL)
	const calls = 2
	for i := 0; i < calls; i++ {
		result, err := RunCommandWithRuntime(context.Background(), RunCmdFuncArgs{
			Sbx:           sbx,
			ProcessConfig: &process.ProcessConfig{Cmd: "echo"},
			Timeout:       5 * time.Second,
		})
		require.NoError(t, err)
		require.True(t, result.EndReceived)
	}

	assert.Equal(t, int32(calls), atomic.LoadInt32(&acceptedConns),
		"each command RPC must run on its own connection so a pooled peer cannot truncate its trailers")
}

func TestRuntimeCommandHTTPClient_Configuration(t *testing.T) {
	assert.NotSame(t, http.DefaultClient, runtimeCommandHTTPClient,
		"command RPCs must not share the process-wide default connection pool")
	assert.Zero(t, runtimeCommandHTTPClient.Timeout,
		"deadlines come from the per-call context, not from the client")

	transport := newRuntimeCommandTransport()
	assert.True(t, transport.DisableKeepAlives)
	assert.True(t, transport.ForceAttemptHTTP2)
}
