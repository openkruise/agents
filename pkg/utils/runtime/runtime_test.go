package runtime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

func TestGetRuntimeURL(t *testing.T) {
	tests := []struct {
		name        string
		sandbox     *v1alpha1.Sandbox
		expectedURL string
	}{
		{
			name: "runtime URL from AnnotationRuntimeURL",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeURL: "http://10.0.0.1:49983",
					},
				},
			},
			expectedURL: "http://10.0.0.1:49983",
		},
		{
			name: "runtime URL from legacy AnnotationEnvdURL",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationEnvdURL: "http://10.0.0.2:49983",
					},
				},
			},
			expectedURL: "http://10.0.0.2:49983",
		},
		{
			name: "AnnotationRuntimeURL takes precedence over legacy AnnotationEnvdURL",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeURL: "http://primary:49983",
						v1alpha1.AnnotationEnvdURL:    "http://legacy:49983",
					},
				},
			},
			expectedURL: "http://primary:49983",
		},
		{
			name: "fallback to route IP when no annotation",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
				Status: v1alpha1.SandboxStatus{
					PodInfo: v1alpha1.PodInfo{
						PodIP: "192.168.1.100",
					},
				},
			},
			expectedURL: fmt.Sprintf("http://192.168.1.100:%d", consts.RuntimePort),
		},
		{
			name: "empty string when no annotation and no route IP",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			expectedURL: "",
		},
		{
			name: "nil annotations - empty string",
			sandbox: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expectedURL: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			url := GetRuntimeURL(tt.sandbox)
			assert.Equal(t, tt.expectedURL, url)
		})
	}
}

func TestGetAccessToken(t *testing.T) {
	tests := []struct {
		name          string
		obj           metav1.Object
		expectedToken string
	}{
		{
			name: "token from AnnotationRuntimeAccessToken",
			obj: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeAccessToken: "my-token-123",
					},
				},
			},
			expectedToken: "my-token-123",
		},
		{
			name: "token from legacy AnnotationEnvdAccessToken",
			obj: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationEnvdAccessToken: "legacy-token",
					},
				},
			},
			expectedToken: "legacy-token",
		},
		{
			name: "AnnotationRuntimeAccessToken takes precedence over legacy",
			obj: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeAccessToken: "primary-token",
						v1alpha1.AnnotationEnvdAccessToken:    "legacy-token",
					},
				},
			},
			expectedToken: "primary-token",
		},
		{
			name: "empty string when no token annotations",
			obj: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			expectedToken: "",
		},
		{
			name: "nil annotations - empty string",
			obj: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expectedToken: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			token := GetAccessToken(tt.obj)
			assert.Equal(t, tt.expectedToken, token)
		})
	}
}

func TestResolveCSIMountFromAnnotation_NoAnnotation(t *testing.T) {
	// When sandbox has no CSI mount annotation, should return nil, nil.
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-sandbox",
			Namespace:   "default",
			Annotations: map[string]string{},
		},
	}

	result, err := ResolveCSIMountFromAnnotation(context.Background(), sbx, nil, nil, nil)
	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestResolveCSIMountFromAnnotation_InvalidJSON(t *testing.T) {
	// When sandbox has invalid CSI mount annotation, should return error.
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: "not-valid-json",
			},
		},
	}

	result, err := ResolveCSIMountFromAnnotation(context.Background(), sbx, nil, nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse csi mount config from annotation")
	assert.Nil(t, result)
}

func TestGetRuntimeURL_WithUID(t *testing.T) {
	// Verify route-based URL generation includes proper IP from PodInfo
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sbx",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Status: v1alpha1.SandboxStatus{
			PodInfo: v1alpha1.PodInfo{
				PodIP: "10.244.0.5",
			},
		},
	}
	url := GetRuntimeURL(sbx)
	assert.Equal(t, fmt.Sprintf("http://10.244.0.5:%d", consts.RuntimePort), url)
}

func TestGetCsiMountExtensionRequest(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expectNil   bool
		expectError bool
		errorMsg    string
		expectCount int
	}{
		{
			name:        "no csi mount annotation",
			annotations: map[string]string{},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "empty csi mount annotation",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: "",
			},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "valid csi mount config with multiple entries",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir1/u1/v1","subPath":"jicheng-1","readOnly":true},{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir2/u2","subPath":"jicheng-2","readOnly":false},{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir3","subPath":"jicheng-3","readOnly":true}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 3,
		},
		{
			name: "valid csi mount config with single entry",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-1","pvName":"pv-1","mountPath":"/mnt/data","subPath":"subpath-1","readOnly":false}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 1,
		},
		{
			name: "invalid json format",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `invalid-json-format`,
			},
			expectNil:   true,
			expectError: true,
			errorMsg:    "failed to unmarshal csi mount options",
		},
		{
			name: "empty array",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[]`,
			},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "valid csi mount with all fields populated",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-123","pvName":"nfs-pv-data","mountPath":"/var/lib/data","subPath":"user/project","readOnly":true}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}

			result, err := GetCsiMountExtensionRequest(sandbox)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
				assert.Nil(t, result)
				return
			}

			assert.NoError(t, err)

			if tt.expectNil {
				assert.Empty(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Len(t, result, tt.expectCount)
			}

			if len(result) > 0 {
				for i, config := range result {
					assert.NotEmpty(t, config.PvName, "pvName should not be empty at index %d", i)
					assert.NotEmpty(t, config.MountPath, "mountPath should not be empty at index %d", i)
				}
			}
		})
	}
}

func TestGetCsiMountExtensionRequest_v2(t *testing.T) {
	const csiVolumeConfigAnnotation = `[{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir1/u1/v1","subPath":"jicheng-1","readOnly":true},{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir2/u2","subPath":"jicheng-2","readOnly":false},{"mountID":"","pvName":"oss-pv-sandbox-system-hangzhou","mountPath":"/dir3","subPath":"jicheng-3","readOnly":true}]`

	tests := []struct {
		name        string
		annotations map[string]string
		expectNil   bool
		expectError bool
		errorMsg    string
		expectCount int
	}{
		{
			name:        "no csi mount annotation",
			annotations: map[string]string{},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "empty csi mount annotation",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: "",
			},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "valid csi mount config with multiple entries from real scenario",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: csiVolumeConfigAnnotation,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 3,
		},
		{
			name: "valid csi mount config with single entry",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-1","pvName":"pv-1","mountPath":"/mnt/data","subPath":"subpath-1","readOnly":false}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 1,
		},
		{
			name: "invalid json format",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `invalid-json-format`,
			},
			expectNil:   true,
			expectError: true,
			errorMsg:    "failed to unmarshal csi mount options",
		},
		{
			name: "empty array",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[]`,
			},
			expectNil:   true,
			expectError: false,
			expectCount: 0,
		},
		{
			name: "valid csi mount with all fields populated",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-123","pvName":"nfs-pv-data","mountPath":"/var/lib/data","subPath":"user/project","readOnly":true}]`,
			},
			expectNil:   false,
			expectError: false,
			expectCount: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}

			result, err := GetCsiMountExtensionRequest(sandbox)

			if tt.expectError {
				assert.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
				assert.Nil(t, result)
				return
			}

			assert.NoError(t, err)

			if tt.expectNil {
				assert.Empty(t, result)
			} else {
				assert.NotNil(t, result)
				assert.Len(t, result, tt.expectCount)
			}

			if len(result) > 0 {
				for i, config := range result {
					assert.NotEmpty(t, config.PvName, "pvName should not be empty at index %d", i)
					assert.NotEmpty(t, config.MountPath, "mountPath should not be empty at index %d", i)
				}
			}

			if tt.name == "valid csi mount config with multiple entries from real scenario" {
				assert.Equal(t, "oss-pv-sandbox-system-hangzhou", result[0].PvName)
				assert.Equal(t, "/dir1/u1/v1", result[0].MountPath)
				assert.Equal(t, "jicheng-1", result[0].SubPath)
				assert.True(t, result[0].ReadOnly)

				assert.Equal(t, "oss-pv-sandbox-system-hangzhou", result[1].PvName)
				assert.Equal(t, "/dir2/u2", result[1].MountPath)
				assert.Equal(t, "jicheng-2", result[1].SubPath)
				assert.False(t, result[1].ReadOnly)

				assert.Equal(t, "oss-pv-sandbox-system-hangzhou", result[2].PvName)
				assert.Equal(t, "/dir3", result[2].MountPath)
				assert.Equal(t, "jicheng-3", result[2].SubPath)
				assert.True(t, result[2].ReadOnly)
			}
		})
	}
}

// mockProcessHandler embeds UnimplementedProcessHandler and overrides Start for testing.
type mockProcessHandler struct {
	processconnect.UnimplementedProcessHandler
	startFunc func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error
}

func (m *mockProcessHandler) Start(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	if m.startFunc != nil {
		return m.startFunc(ctx, req, stream)
	}
	return nil
}

// newTestServer creates a httptest TLS server with the mock process handler.
// TLS server natively supports HTTP/2 which is required by gRPC.
// It also temporarily replaces http.DefaultClient's transport to trust the test TLS cert.
func newTestServer(t *testing.T, handler *mockProcessHandler) *httptest.Server {
	mux := http.NewServeMux()
	path, h := processconnect.NewProcessHandler(handler)
	mux.Handle(path, h)
	server := httptest.NewTLSServer(mux)

	// Save and restore original default client transport
	origTransport := http.DefaultClient.Transport
	http.DefaultClient.Transport = server.Client().Transport
	t.Cleanup(func() {
		http.DefaultClient.Transport = origTransport
		server.Close()
	})
	return server
}

func TestRunCommandWithRuntime_NoRuntimeURL(t *testing.T) {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
		},
	}
	args := RunCmdFuncArgs{
		Sbx: sbx,
		ProcessConfig: &process.ProcessConfig{
			Cmd: "echo",
		},
		Timeout: 5 * time.Second,
	}

	result, err := RunCommandWithRuntime(context.Background(), args)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "runtime url not found on sandbox")
	assert.Equal(t, RunCommandResult{}, result)
}

func TestRunCommandWithRuntime_SuccessfulExecution(t *testing.T) {
	handler := &mockProcessHandler{
		startFunc: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
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
			// Send stdout data
			if err := stream.Send(&process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_Data{
						Data: &process.ProcessEvent_DataEvent{
							Output: &process.ProcessEvent_DataEvent_Stdout{
								Stdout: []byte("hello world"),
							},
						},
					},
				},
			}); err != nil {
				return err
			}
			// Send stderr data
			if err := stream.Send(&process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_Data{
						Data: &process.ProcessEvent_DataEvent{
							Output: &process.ProcessEvent_DataEvent_Stderr{
								Stderr: []byte("some warning"),
							},
						},
					},
				},
			}); err != nil {
				return err
			}
			// Send end event
			return stream.Send(&process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_End{
						End: &process.ProcessEvent_EndEvent{
							ExitCode: 0,
							Exited:   true,
						},
					},
				},
			})
		},
	}
	server := newTestServer(t, handler)

	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL: server.URL,
			},
		},
	}
	args := RunCmdFuncArgs{
		Sbx: sbx,
		ProcessConfig: &process.ProcessConfig{
			Cmd:  "echo",
			Args: []string{"hello"},
		},
		Timeout: 5 * time.Second,
	}

	result, err := RunCommandWithRuntime(context.Background(), args)
	require.NoError(t, err)
	assert.Equal(t, uint32(42), result.PID)
	assert.Equal(t, []string{"hello world"}, result.Stdout)
	assert.Equal(t, []string{"some warning"}, result.Stderr)
	assert.Equal(t, int32(0), result.ExitCode)
	assert.True(t, result.Exited)
	assert.Nil(t, result.Error)
}

func TestRunCommandWithRuntime_ProcessError(t *testing.T) {
	errMsg := "segmentation fault"
	handler := &mockProcessHandler{
		startFunc: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			if err := stream.Send(&process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_Start{
						Start: &process.ProcessEvent_StartEvent{Pid: 99},
					},
				},
			}); err != nil {
				return err
			}
			return stream.Send(&process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_End{
						End: &process.ProcessEvent_EndEvent{
							ExitCode: 139,
							Exited:   true,
							Error:    ptr.To(errMsg),
						},
					},
				},
			})
		},
	}
	server := newTestServer(t, handler)

	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL: server.URL,
			},
		},
	}
	args := RunCmdFuncArgs{
		Sbx:           sbx,
		ProcessConfig: &process.ProcessConfig{Cmd: "crash"},
		Timeout:       5 * time.Second,
	}

	result, err := RunCommandWithRuntime(context.Background(), args)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "segmentation fault")
	assert.Equal(t, uint32(99), result.PID)
	assert.Equal(t, int32(139), result.ExitCode)
	assert.True(t, result.Exited)
}

func TestRunCommandWithRuntime_ServerError(t *testing.T) {
	handler := &mockProcessHandler{
		startFunc: func(_ context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("internal server error"))
		},
	}
	server := newTestServer(t, handler)

	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL: server.URL,
			},
		},
	}
	args := RunCmdFuncArgs{
		Sbx:           sbx,
		ProcessConfig: &process.ProcessConfig{Cmd: "test"},
		Timeout:       5 * time.Second,
	}

	_, err := RunCommandWithRuntime(context.Background(), args)
	assert.Error(t, err)
}

func TestRunCommandWithRuntime_EmptyStream(t *testing.T) {
	// Server sends no events - stream closes immediately
	handler := &mockProcessHandler{
		startFunc: func(_ context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
			return nil // close stream without sending anything
		},
	}
	server := newTestServer(t, handler)

	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL: server.URL,
			},
		},
	}
	args := RunCmdFuncArgs{
		Sbx:           sbx,
		ProcessConfig: &process.ProcessConfig{Cmd: "noop"},
		Timeout:       5 * time.Second,
	}

	result, err := RunCommandWithRuntime(context.Background(), args)
	assert.NoError(t, err)
	assert.Equal(t, uint32(0), result.PID)
	assert.Nil(t, result.Stdout)
	assert.Nil(t, result.Stderr)
}

func TestRunCommandWithRuntime_ContextTimeout(t *testing.T) {
	// Server blocks indefinitely, context should timeout
	handler := &mockProcessHandler{
		startFunc: func(ctx context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	server := newTestServer(t, handler)

	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationRuntimeURL: server.URL,
			},
		},
	}
	args := RunCmdFuncArgs{
		Sbx:           sbx,
		ProcessConfig: &process.ProcessConfig{Cmd: "sleep"},
		Timeout:       100 * time.Millisecond,
	}

	_, err := RunCommandWithRuntime(context.Background(), args)
	assert.Error(t, err)
}
