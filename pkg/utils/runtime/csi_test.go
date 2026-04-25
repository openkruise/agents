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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

// mockProcessHandler implements ProcessHandler for testing.
type mockProcessHandler struct {
	processconnect.UnimplementedProcessHandler
	startFn func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error
}

func (m *mockProcessHandler) Start(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	if m.startFn != nil {
		return m.startFn(ctx, req, stream)
	}
	return m.UnimplementedProcessHandler.Start(ctx, req, stream)
}

// newMockRuntimeServer creates a test server with a mock ProcessHandler and returns the server and sandbox.
func newMockRuntimeServer(t *testing.T, handler *mockProcessHandler) (*httptest.Server, *agentsv1alpha1.Sandbox) {
	t.Helper()
	_, h := processconnect.NewProcessHandler(handler)
	mux := http.NewServeMux()
	mux.Handle("/", h)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL: server.URL,
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			PodInfo: agentsv1alpha1.PodInfo{
				PodUID: types.UID("test-pod-uid"),
			},
		},
	}
	return server, sbx
}

func TestCSIMount(t *testing.T) {
	tests := []struct {
		name        string
		driver      string
		request     string
		startFn     func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error
		wantErr     bool
		errContains string
	}{
		{
			name:    "successful mount with exit code 0",
			driver:  "nfs",
			request: `{"path":"/mnt/data"}`,
			startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				// Verify the process config
				assert.Equal(t, MountCommand, req.Msg.Process.Cmd)
				assert.Equal(t, []string{"mount", "--driver", "nfs", "--config", `{"path":"/mnt/data"}`}, req.Msg.Process.Args)
				assert.Equal(t, "test-pod-uid", req.Msg.Process.Envs["POD_UID"])

				// Send start event
				if err := stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_Start{
							Start: &process.ProcessEvent_StartEvent{Pid: 42},
						},
					},
				}); err != nil {
					return err
				}
				// Send end event with exit code 0
				return stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_End{
							End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true},
						},
					},
				})
			},
			wantErr: false,
		},
		{
			name:    "command failed with non-zero exit code",
			driver:  "oss",
			request: `{"bucket":"test"}`,
			startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_Start{
							Start: &process.ProcessEvent_StartEvent{Pid: 43},
						},
					},
				}); err != nil {
					return err
				}
				// Do NOT set Error field — otherwise RunCommandWithRuntime returns result.Error
				// directly, and CSIMount hits the first err != nil branch (L45) instead of
				// reaching the ExitCode != 0 check (L49).
				return stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_End{
							End: &process.ProcessEvent_EndEvent{ExitCode: 1, Exited: true},
						},
					},
				})
			},
			wantErr:     true,
			errContains: "command failed",
		},
		{
			name:    "runtime connection error (no runtime URL)",
			driver:  "nfs",
			request: `{}`,
			startFn: nil, // won't be called since we override sbx
			wantErr: true,
		},
		{
			name:    "gRPC Start returns error",
			driver:  "nfs",
			request: `{}`,
			startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("internal server error"))
			},
			wantErr: true,
		},
		{
			name:    "command produces stderr output with non-zero exit",
			driver:  "cephfs",
			request: `{"pool":"rbd"}`,
			startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_Start{
							Start: &process.ProcessEvent_StartEvent{Pid: 44},
						},
					},
				}); err != nil {
					return err
				}
				if err := stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_Data{
							Data: &process.ProcessEvent_DataEvent{
								Output: &process.ProcessEvent_DataEvent_Stderr{
									Stderr: []byte("permission denied"),
								},
							},
						},
					},
				}); err != nil {
					return err
				}
				return stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_End{
							End: &process.ProcessEvent_EndEvent{ExitCode: 2, Exited: true},
						},
					},
				})
			},
			wantErr:     true,
			errContains: "permission denied",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()

			if tt.name == "runtime connection error (no runtime URL)" {
				// Sandbox without runtime URL
				sbx := &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-sandbox",
						Namespace: "default",
					},
					Status: agentsv1alpha1.SandboxStatus{
						PodInfo: agentsv1alpha1.PodInfo{
							PodUID: types.UID("test-pod-uid"),
						},
					},
				}
				err := CSIMount(ctx, sbx, tt.driver, tt.request)
				require.Error(t, err)
				assert.Contains(t, err.Error(), "runtime url not found")
				return
			}

			handler := &mockProcessHandler{startFn: tt.startFn}
			_, sbx := newMockRuntimeServer(t, handler)

			err := CSIMount(ctx, sbx, tt.driver, tt.request)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestProcessCSIMounts(t *testing.T) {
	successStartFn := func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if err := stream.Send(&process.StartResponse{
			Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Start{
					Start: &process.ProcessEvent_StartEvent{Pid: 1},
				},
			},
		}); err != nil {
			return err
		}
		return stream.Send(&process.StartResponse{
			Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_End{
					End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true},
				},
			},
		})
	}

	tests := []struct {
		name        string
		opts        config.CSIMountOptions
		startFn     func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error
		wantErr     bool
		errContains string
	}{
		{
			name: "empty mount list returns immediately",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{},
			},
			startFn: successStartFn,
			wantErr: false,
		},
		{
			name: "single mount success",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "nfs", RequestRaw: `{"path":"/mnt"}`},
				},
			},
			startFn: successStartFn,
			wantErr: false,
		},
		{
			name: "multiple mounts all succeed",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "nfs", RequestRaw: `{"path":"/mnt/a"}`},
					{Driver: "oss", RequestRaw: `{"bucket":"test"}`},
					{Driver: "cephfs", RequestRaw: `{"pool":"rbd"}`},
				},
				Concurrency: 2,
			},
			startFn: successStartFn,
			wantErr: false,
		},
		{
			name: "one mount fails among multiple",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "nfs", RequestRaw: `{"path":"/mnt/a"}`},
					{Driver: "bad-driver", RequestRaw: `{}`},
					{Driver: "oss", RequestRaw: `{"bucket":"test"}`},
				},
				Concurrency: 1,
			},
			startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				driver := ""
				for _, arg := range req.Msg.Process.Args {
					if arg == "bad-driver" {
						driver = "bad-driver"
						break
					}
				}
				if err := stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_Start{
							Start: &process.ProcessEvent_StartEvent{Pid: 1},
						},
					},
				}); err != nil {
					return err
				}
				exitCode := int32(0)
				if driver == "bad-driver" {
					exitCode = 1
				}
				return stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_End{
							End: &process.ProcessEvent_EndEvent{ExitCode: exitCode, Exited: true},
						},
					},
				})
			},
			wantErr:     true,
			errContains: "command failed",
		},
		{
			name: "default concurrency when value is 0",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "nfs", RequestRaw: `{"path":"/mnt"}`},
				},
				Concurrency: 0,
			},
			startFn: successStartFn,
			wantErr: false,
		},
		{
			name: "default concurrency when value is negative",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "nfs", RequestRaw: `{"path":"/mnt"}`},
				},
				Concurrency: -1,
			},
			startFn: successStartFn,
			wantErr: false,
		},
		{
			name: "custom concurrency with multiple mounts",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "d1", RequestRaw: `{}`},
					{Driver: "d2", RequestRaw: `{}`},
					{Driver: "d3", RequestRaw: `{}`},
					{Driver: "d4", RequestRaw: `{}`},
				},
				Concurrency: 2,
			},
			startFn: successStartFn,
			wantErr: false,
		},
		{
			name: "multiple mounts all fail",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "d1", RequestRaw: `{}`},
					{Driver: "d2", RequestRaw: `{}`},
					{Driver: "d3", RequestRaw: `{}`},
				},
			},
			startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 1}},
				}}); err != nil {
					return err
				}
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 1, Exited: true}},
				}})
			},
			wantErr:     true,
			errContains: "command failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &mockProcessHandler{startFn: tt.startFn}
			_, sbx := newMockRuntimeServer(t, handler)

			duration, err := ProcessCSIMounts(context.Background(), sbx, tt.opts)
			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}
			require.NoError(t, err)
			assert.True(t, duration > 0, "duration should be positive, got %v", duration)
		})
	}
}

func TestProcessCSIMounts_ConcurrencyLimit(t *testing.T) {
	var maxConcurrent int32
	var currentConcurrent int32

	handler := &mockProcessHandler{
		startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			cur := atomic.AddInt32(&currentConcurrent, 1)
			defer atomic.AddInt32(&currentConcurrent, -1)

			// Track max concurrency observed
			for {
				old := atomic.LoadInt32(&maxConcurrent)
				if cur <= old || atomic.CompareAndSwapInt32(&maxConcurrent, old, cur) {
					break
				}
			}

			if err := stream.Send(&process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_Start{
						Start: &process.ProcessEvent_StartEvent{Pid: 1},
					},
				},
			}); err != nil {
				return err
			}
			return stream.Send(&process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_End{
						End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true},
					},
				},
			})
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	opts := config.CSIMountOptions{
		MountOptionList: []config.MountConfig{
			{Driver: "d1", RequestRaw: "{}"},
			{Driver: "d2", RequestRaw: "{}"},
			{Driver: "d3", RequestRaw: "{}"},
			{Driver: "d4", RequestRaw: "{}"},
			{Driver: "d5", RequestRaw: "{}"},
		},
		Concurrency: 2,
	}
	_, err := ProcessCSIMounts(context.Background(), sbx, opts)
	require.NoError(t, err)

	// maxConcurrent should not exceed the configured concurrency limit
	assert.LessOrEqual(t, atomic.LoadInt32(&maxConcurrent), int32(2),
		"max concurrent mounts should not exceed concurrency limit of 2")
}

func TestProcessCSIMounts_NoRuntimeURL(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-sandbox",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
	}
	opts := config.CSIMountOptions{
		MountOptionList: []config.MountConfig{
			{Driver: "nfs", RequestRaw: `{"path":"/mnt"}`},
		},
	}

	_, err := ProcessCSIMounts(context.Background(), sbx, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "runtime url not found")
}

func TestProcessCSIMounts_ErrorDoesNotBlockOthers(t *testing.T) {
	// When all mounts fail, the function should still complete (not hang).
	failStartFn := func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
			Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 1}},
		}}); err != nil {
			return err
		}
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
			Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 1, Exited: true}},
		}})
	}

	handler := &mockProcessHandler{startFn: failStartFn}
	_, sbx := newMockRuntimeServer(t, handler)

	opts := config.CSIMountOptions{
		Concurrency: 1,
		MountOptionList: []config.MountConfig{
			{Driver: "d1", RequestRaw: "{}"},
			{Driver: "d2", RequestRaw: "{}"},
			{Driver: "d3", RequestRaw: "{}"},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := ProcessCSIMounts(context.Background(), sbx, opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "command failed")
	}()

	select {
	case <-done:
		// OK: function completed, no hang
	case <-time.After(10 * time.Second):
		t.Fatal("ProcessCSIMounts hung when mounts failed")
	}
}

func TestProcessCSIMounts_AllErrorsCollected(t *testing.T) {
	// When multiple mounts fail, errors.Join should aggregate all errors.
	failStartFn := func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
		if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
			Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 1}},
		}}); err != nil {
			return err
		}
		return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
			Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 1, Exited: true}},
		}})
	}

	handler := &mockProcessHandler{startFn: failStartFn}
	_, sbx := newMockRuntimeServer(t, handler)

	opts := config.CSIMountOptions{
		Concurrency: 3,
		MountOptionList: []config.MountConfig{
			{Driver: "d1", RequestRaw: "{}"},
			{Driver: "d2", RequestRaw: "{}"},
			{Driver: "d3", RequestRaw: "{}"},
		},
	}

	_, err := ProcessCSIMounts(context.Background(), sbx, opts)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command failed")
}

func TestProcessCSIMounts_ContextCanceled(t *testing.T) {
	// Use a blocking startFn to simulate slow mounts, then cancel context.
	blockCh := make(chan struct{})
	t.Cleanup(func() { close(blockCh) })

	handler := &mockProcessHandler{
		startFn: func(ctx context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-blockCh:
				return nil
			}
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	opts := config.CSIMountOptions{
		Concurrency: 1,
		MountOptionList: []config.MountConfig{
			{Driver: "d1", RequestRaw: "{}"},
			{Driver: "d2", RequestRaw: "{}"},
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = ProcessCSIMounts(ctx, sbx, opts)
	}()

	select {
	case <-done:
		// OK: function returned after context canceled
	case <-time.After(10 * time.Second):
		t.Fatal("ProcessCSIMounts did not respect context cancellation")
	}
}

func TestDoCSIMount(t *testing.T) {
	tests := []struct {
		name    string
		startFn func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error
		opts    config.MountConfig
		wantErr bool
	}{
		{
			name: "success delegates to CSIMount",
			startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_Start{
							Start: &process.ProcessEvent_StartEvent{Pid: 1},
						},
					},
				}); err != nil {
					return err
				}
				return stream.Send(&process.StartResponse{
					Event: &process.ProcessEvent{
						Event: &process.ProcessEvent_End{
							End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true},
						},
					},
				})
			},
			opts:    config.MountConfig{Driver: "nfs", RequestRaw: `{"path":"/mnt"}`},
			wantErr: false,
		},
		{
			name: "error propagated from CSIMount",
			startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("server failure"))
			},
			opts:    config.MountConfig{Driver: "nfs", RequestRaw: `{}`},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &mockProcessHandler{startFn: tt.startFn}
			_, sbx := newMockRuntimeServer(t, handler)

			duration, err := doCSIMount(context.Background(), sbx, tt.opts)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.True(t, duration > 0, "duration should be positive, got %v", duration)
		})
	}
}
