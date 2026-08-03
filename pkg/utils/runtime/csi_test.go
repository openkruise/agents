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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/protoadapt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
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

// testPublishRequest builds a CSI publish request identified by its target path.
// The legacy CLI transport treats the request as an opaque blob and the storage
// API compares it as a message, so the target path alone is enough to tell the
// mounts of a test apart.
func testPublishRequest(targetPath string) *csi.NodePublishVolumeRequest {
	return &csi.NodePublishVolumeRequest{VolumeId: "pv" + targetPath, TargetPath: targetPath}
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
					{Driver: "nfs", PublishRequest: testPublishRequest("/mnt")},
				},
			},
			startFn: successStartFn,
			wantErr: false,
		},
		{
			name: "multiple mounts all succeed",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "nfs", PublishRequest: testPublishRequest("/mnt/a")},
					{Driver: "oss", PublishRequest: testPublishRequest("/mnt/b")},
					{Driver: "cephfs", PublishRequest: testPublishRequest("/mnt/c")},
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
					{Driver: "nfs", PublishRequest: testPublishRequest("/mnt/a")},
					{Driver: "bad-driver", PublishRequest: testPublishRequest("/mnt/bad")},
					{Driver: "oss", PublishRequest: testPublishRequest("/mnt/b")},
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
					{Driver: "nfs", PublishRequest: testPublishRequest("/mnt")},
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
					{Driver: "nfs", PublishRequest: testPublishRequest("/mnt")},
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
					{Driver: "d1", PublishRequest: testPublishRequest("/mnt/1")},
					{Driver: "d2", PublishRequest: testPublishRequest("/mnt/2")},
					{Driver: "d3", PublishRequest: testPublishRequest("/mnt/3")},
					{Driver: "d4", PublishRequest: testPublishRequest("/mnt/4")},
				},
				Concurrency: 2,
			},
			startFn: successStartFn,
			wantErr: false,
		},
		{
			name: "mount without publish request fails before the cli runs",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "nfs"},
				},
			},
			startFn:     successStartFn,
			wantErr:     true,
			errContains: "csi publish request is required",
		},
		{
			name: "multiple mounts all fail",
			opts: config.CSIMountOptions{
				MountOptionList: []config.MountConfig{
					{Driver: "d1", PublishRequest: testPublishRequest("/mnt/1")},
					{Driver: "d2", PublishRequest: testPublishRequest("/mnt/2")},
					{Driver: "d3", PublishRequest: testPublishRequest("/mnt/3")},
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
			{Driver: "d1", PublishRequest: testPublishRequest("/mnt/1")},
			{Driver: "d2", PublishRequest: testPublishRequest("/mnt/2")},
			{Driver: "d3", PublishRequest: testPublishRequest("/mnt/3")},
			{Driver: "d4", PublishRequest: testPublishRequest("/mnt/4")},
			{Driver: "d5", PublishRequest: testPublishRequest("/mnt/5")},
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
			{Driver: "nfs", PublishRequest: testPublishRequest("/mnt")},
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
			{Driver: "d1", PublishRequest: testPublishRequest("/mnt/1")},
			{Driver: "d2", PublishRequest: testPublishRequest("/mnt/2")},
			{Driver: "d3", PublishRequest: testPublishRequest("/mnt/3")},
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
			{Driver: "d1", PublishRequest: testPublishRequest("/mnt/1")},
			{Driver: "d2", PublishRequest: testPublishRequest("/mnt/2")},
			{Driver: "d3", PublishRequest: testPublishRequest("/mnt/3")},
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
			{Driver: "d1", PublishRequest: testPublishRequest("/mnt/1")},
			{Driver: "d2", PublishRequest: testPublishRequest("/mnt/2")},
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
			opts:    config.MountConfig{Driver: "nfs", PublishRequest: testPublishRequest("/mnt")},
			wantErr: false,
		},
		{
			name: "error propagated from CSIMount",
			startFn: func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				return connect.NewError(connect.CodeInternal, fmt.Errorf("server failure"))
			},
			opts:    config.MountConfig{Driver: "nfs", PublishRequest: testPublishRequest("/mnt")},
			wantErr: true,
		},
		{
			// The CLI would only fail inside the sandbox on an empty --config, so
			// the missing request must be rejected here instead.
			name:    "missing publish request rejected before the cli runs",
			startFn: nil,
			opts:    config.MountConfig{Driver: "nfs"},
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

// TestDoCSIMount_LegacyTransportEncodesRequest pins the legacy CLI contract now
// that MountConfig carries the typed message: the encoding happens as the last
// step of the mount, and the blob on the command line must decode back into the
// exact request the caller built.
func TestDoCSIMount_LegacyTransportEncodesRequest(t *testing.T) {
	publishRequest := testPublishRequest("/data/workspace")

	gotArgs := make(chan []string, 1)
	handler := &mockProcessHandler{
		startFn: func(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			gotArgs <- req.Msg.Process.Args
			if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 1}},
			}}); err != nil {
				return err
			}
			return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}},
			}})
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	_, err := doCSIMount(context.Background(), sbx, config.MountConfig{Driver: "nfs", PublishRequest: publishRequest})
	require.NoError(t, err)

	args := <-gotArgs
	require.Len(t, args, 5)
	assert.Equal(t, []string{"mount", "--driver", "nfs", "--config"}, args[:4])

	raw, err := base64.StdEncoding.DecodeString(args[4])
	require.NoError(t, err, "--config must be base64")
	decoded := &csi.NodePublishVolumeRequest{}
	require.NoError(t, proto.Unmarshal(raw, protoadapt.MessageV2Of(decoded)))
	assert.True(t, protoEqual(publishRequest, decoded), "the CLI must receive the caller's request unchanged")
}

// TestDoCSIMount_TransportDispatch verifies the storage-API side of the two
// coexisting mount transports: non-empty rtOpts route the mount through the
// runtime storage API (POST /v1/storage/mounts) instead of the legacy CLI, and
// the typed MountConfig.PublishRequest reaches the runtime untouched. The legacy
// path (empty rtOpts) is covered by TestDoCSIMount above.
func TestDoCSIMount_TransportDispatch(t *testing.T) {
	publishRequest := testMountRequest("ossplugin.csi.alibabacloud.com").PublishRequest

	tests := []struct {
		name           string
		publishRequest *csi.NodePublishVolumeRequest
		status         int
		resp           CreateMountResponse
		// wantNoDispatch asserts the mount is rejected locally, before any
		// request reaches the runtime.
		wantNoDispatch bool
		wantErr        bool
	}{
		{
			name:           "storage API success",
			publishRequest: publishRequest,
			status:         http.StatusOK,
			resp:           CreateMountResponse{Success: true, MountPath: "/m"},
		},
		{
			name:           "storage API reports mount failure",
			publishRequest: publishRequest,
			status:         http.StatusOK,
			resp:           CreateMountResponse{Success: false, Message: "denied"},
			wantErr:        true,
		},
		{
			name:           "storage API server error",
			publishRequest: publishRequest,
			status:         http.StatusInternalServerError,
			resp:           CreateMountResponse{},
			wantErr:        true,
		},
		{
			name:           "missing request is rejected before dispatch",
			publishRequest: nil,
			status:         http.StatusOK,
			resp:           CreateMountResponse{Success: true, MountPath: "/m"},
			wantNoDispatch: true,
			wantErr:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var gotReq CreateMountRequest
			var hits atomic.Int32
			_, sbx := newMountTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				// The handler runs on the server goroutine, where t.FailNow (and
				// therefore require) is not allowed: assert and let the test body
				// report the mismatch.
				assert.Equal(t, http.MethodPost, r.Method)
				assert.NoError(t, json.NewDecoder(r.Body).Decode(&gotReq))
				writeMountResponse(t, w, tt.status, tt.resp)
			})

			opt := config.MountConfig{Driver: "ossplugin.csi.alibabacloud.com", PublishRequest: tt.publishRequest}

			// Non-empty rtOpts select the storage API path; the sandbox carries the
			// plain-HTTP test server URL, so the exercised branch is exactly the one
			// the TLS options would take in production.
			_, err := doCSIMount(context.Background(), sbx, opt, WithRetry(fastBackoff))
			if tt.wantErr {
				require.Error(t, err)
				if tt.wantNoDispatch {
					assert.Zero(t, hits.Load(), "a request without a CSI payload must not reach the runtime")
				}
				return
			}
			require.NoError(t, err)
			assert.GreaterOrEqual(t, hits.Load(), int32(1))
			assert.Equal(t, opt.Driver, gotReq.Driver)
			// The runtime must observe the exact CSI message the caller built,
			// proving the transport switch stays lossless.
			assert.True(t, protoEqual(publishRequest, gotReq.PublishRequest))
		})
	}
}
