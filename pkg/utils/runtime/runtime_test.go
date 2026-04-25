/*
Copyright 2025.

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
	"encoding/json"
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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/proto/envd/process"
)

func TestGetRuntimeURL(t *testing.T) {
	tests := []struct {
		name        string
		sandbox     *agentsv1alpha1.Sandbox
		expectedURL string
	}{
		{
			name: "runtime URL from AnnotationRuntimeURL",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeURL: "http://10.0.0.1:49983",
					},
				},
			},
			expectedURL: "http://10.0.0.1:49983",
		},
		{
			name: "runtime URL from legacy AnnotationEnvdURL",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationEnvdURL: "http://10.0.0.2:49983",
					},
				},
			},
			expectedURL: "http://10.0.0.2:49983",
		},
		{
			name: "AnnotationRuntimeURL takes precedence over legacy AnnotationEnvdURL",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeURL: "http://primary:49983",
						agentsv1alpha1.AnnotationEnvdURL:    "http://legacy:49983",
					},
				},
			},
			expectedURL: "http://primary:49983",
		},
		{
			name: "fallback to route IP when no annotation",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "192.168.1.100",
					},
				},
			},
			expectedURL: fmt.Sprintf("http://192.168.1.100:%d", consts.RuntimePort),
		},
		{
			name: "empty string when no annotation and no route IP",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			expectedURL: "",
		},
		{
			name: "nil annotations - empty string",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expectedURL: "",
		},
		{
			name: "route-based URL with UID",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-sbx",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "10.244.0.5",
					},
				},
			},
			expectedURL: fmt.Sprintf("http://10.244.0.5:%d", consts.RuntimePort),
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
			obj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeAccessToken: "my-token-123",
					},
				},
			},
			expectedToken: "my-token-123",
		},
		{
			name: "token from legacy AnnotationEnvdAccessToken",
			obj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationEnvdAccessToken: "legacy-token",
					},
				},
			},
			expectedToken: "legacy-token",
		},
		{
			name: "AnnotationRuntimeAccessToken takes precedence over legacy",
			obj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeAccessToken: "primary-token",
						agentsv1alpha1.AnnotationEnvdAccessToken:    "legacy-token",
					},
				},
			},
			expectedToken: "primary-token",
		},
		{
			name: "empty string when no token annotations",
			obj: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			expectedToken: "",
		},
		{
			name: "nil annotations - empty string",
			obj: &agentsv1alpha1.Sandbox{
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
		},
		{
			name: "empty csi mount annotation",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: "",
			},
			expectNil: true,
		},
		{
			name: "valid csi mount config with multiple entries",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"","pvName":"oss-pv","mountPath":"/dir1","subPath":"sp1","readOnly":true},{"mountID":"","pvName":"oss-pv","mountPath":"/dir2","subPath":"sp2","readOnly":false}]`,
			},
			expectCount: 2,
		},
		{
			name: "valid csi mount config with single entry",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"m1","pvName":"pv-1","mountPath":"/mnt/data","subPath":"sub","readOnly":false}]`,
			},
			expectCount: 1,
		},
		{
			name: "invalid json format",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `invalid-json`,
			},
			expectError: true,
			errorMsg:    "failed to unmarshal csi mount options",
		},
		{
			name: "empty array",
			annotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[]`,
			},
			expectNil: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{Annotations: tt.annotations},
			}
			result, err := GetCsiMountExtensionRequest(sandbox)

			if tt.expectError {
				require.Error(t, err)
				if tt.errorMsg != "" {
					assert.Contains(t, err.Error(), tt.errorMsg)
				}
				assert.Nil(t, result)
				return
			}

			require.NoError(t, err)
			if tt.expectNil {
				assert.Empty(t, result)
			} else {
				assert.Len(t, result, tt.expectCount)
				for i, cfg := range result {
					assert.NotEmpty(t, cfg.PvName, "pvName should not be empty at index %d", i)
					assert.NotEmpty(t, cfg.MountPath, "mountPath should not be empty at index %d", i)
				}
			}
		})
	}
}

func TestGetInitRuntimeRequest(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		wantNil     bool
		wantReInit  bool
		wantErr     bool
		wantEnvVars map[string]string
	}{
		{
			name:        "no annotation returns nil",
			annotations: nil,
			wantNil:     true,
		},
		{
			name:        "empty annotation map returns nil",
			annotations: map[string]string{},
			wantNil:     true,
		},
		{
			name: "valid annotation with envVars",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationInitRuntimeRequest: `{"envVars":{"FOO":"bar"},"accessToken":"tok123"}`,
			},
			wantReInit:  true,
			wantEnvVars: map[string]string{"FOO": "bar"},
		},
		{
			name: "valid annotation with empty object",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationInitRuntimeRequest: `{}`,
			},
			wantReInit: true,
		},
		{
			name: "invalid JSON returns error",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationInitRuntimeRequest: `{invalid-json}`,
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "test-sandbox",
					Namespace:   "default",
					Annotations: tt.annotations,
				},
			}

			result, err := GetInitRuntimeRequest(obj)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "failed to unmarshal init runtime request")
				return
			}
			require.NoError(t, err)

			if tt.wantNil {
				assert.Nil(t, result)
				return
			}

			require.NotNil(t, result)
			assert.Equal(t, tt.wantReInit, result.ReInit)
			if tt.wantEnvVars != nil {
				assert.Equal(t, tt.wantEnvVars, result.EnvVars)
			}
		})
	}
}

func newTestSandboxWithURL(url string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sandbox",
			Namespace: "default",
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL: url,
			},
		},
	}
}

func TestInitRuntime(t *testing.T) {
	tests := []struct {
		name        string
		handler     http.HandlerFunc
		opts        config.InitRuntimeOptions
		refreshFn   RefreshFunc
		sbxSetup    func(url string) *agentsv1alpha1.Sandbox
		wantErr     bool
		errContains string
	}{
		{
			name: "successful init with 200 response",
			handler: func(w http.ResponseWriter, r *http.Request) {
				assert.Equal(t, "/init", r.URL.Path)
				assert.Equal(t, http.MethodPost, r.Method)
				w.WriteHeader(http.StatusOK)
			},
			opts: config.InitRuntimeOptions{SkipRefresh: true},
			sbxSetup: func(url string) *agentsv1alpha1.Sandbox {
				return newTestSandboxWithURL(url)
			},
		},
		{
			name: "ReInit true with 401 treated as success",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			opts: config.InitRuntimeOptions{SkipRefresh: true, ReInit: true},
			sbxSetup: func(url string) *agentsv1alpha1.Sandbox {
				return newTestSandboxWithURL(url)
			},
		},
		{
			name: "ReInit false with 401 returns error",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("unauthorized"))
			},
			opts: config.InitRuntimeOptions{SkipRefresh: true, ReInit: false},
			sbxSetup: func(url string) *agentsv1alpha1.Sandbox {
				return newTestSandboxWithURL(url)
			},
			wantErr:     true,
			errContains: "not 2xx",
		},
		{
			name:    "empty runtime URL returns error",
			handler: nil,
			opts:    config.InitRuntimeOptions{SkipRefresh: true},
			sbxSetup: func(_ string) *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				}
			},
			wantErr:     true,
			errContains: "runtimeURL is empty",
		},
		{
			name: "SkipRefresh false with refreshFn updates sandbox",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
			opts: config.InitRuntimeOptions{SkipRefresh: false},
			sbxSetup: func(_ string) *agentsv1alpha1.Sandbox {
				// initial sandbox has no runtime URL
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				}
			},
			refreshFn: nil, // set dynamically in test body
		},
		{
			name:    "SkipRefresh false with refreshFn error",
			handler: nil,
			opts:    config.InitRuntimeOptions{SkipRefresh: false},
			sbxSetup: func(_ string) *agentsv1alpha1.Sandbox {
				return &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				}
			},
			refreshFn: func(_ context.Context) (*agentsv1alpha1.Sandbox, error) {
				return nil, fmt.Errorf("refresh failed")
			},
			wantErr:     true,
			errContains: "refresh failed",
		},
		{
			name: "SkipRefresh true does not call refreshFn",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			},
			opts: config.InitRuntimeOptions{SkipRefresh: true},
			sbxSetup: func(url string) *agentsv1alpha1.Sandbox {
				return newTestSandboxWithURL(url)
			},
			refreshFn: func(_ context.Context) (*agentsv1alpha1.Sandbox, error) {
				t.Fatal("refreshFn should not be called when SkipRefresh is true")
				return nil, nil
			},
		},
		{
			name: "server returns 500 retries and eventually fails",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal error"))
			},
			opts: config.InitRuntimeOptions{SkipRefresh: true},
			sbxSetup: func(url string) *agentsv1alpha1.Sandbox {
				return newTestSandboxWithURL(url)
			},
			wantErr:     true,
			errContains: "not 2xx",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var server *httptest.Server
			if tt.handler != nil {
				server = httptest.NewServer(tt.handler)
				defer server.Close()
			}

			var serverURL string
			if server != nil {
				serverURL = server.URL
			}
			sbx := tt.sbxSetup(serverURL)

			refreshFn := tt.refreshFn
			// Special case: dynamically set refreshFn to return sandbox with server URL
			if tt.name == "SkipRefresh false with refreshFn updates sandbox" && refreshFn == nil {
				refreshFn = func(_ context.Context) (*agentsv1alpha1.Sandbox, error) {
					return newTestSandboxWithURL(serverURL), nil
				}
			}

			duration, err := InitRuntime(context.Background(), sbx, tt.opts, refreshFn)
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

func TestInitRuntime_RequestBodyContainsOpts(t *testing.T) {
	opts := config.InitRuntimeOptions{
		EnvVars:     map[string]string{"KEY": "VALUE"},
		AccessToken: "test-token",
		SkipRefresh: true,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var received config.InitRuntimeOptions
		err := json.NewDecoder(r.Body).Decode(&received)
		require.NoError(t, err)
		assert.Equal(t, opts.EnvVars, received.EnvVars)
		assert.Equal(t, opts.AccessToken, received.AccessToken)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	sbx := newTestSandboxWithURL(server.URL)
	_, err := InitRuntime(context.Background(), sbx, opts, nil)
	require.NoError(t, err)
}

func TestResolveCSIMountFromAnnotation_NoAnnotation(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
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
	sbx := &agentsv1alpha1.Sandbox{
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

func TestRunCommandWithRuntime_NoRuntimeURL(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
	}
	args := RunCmdFuncArgs{
		Sbx:           sbx,
		ProcessConfig: &process.ProcessConfig{Cmd: "echo"},
		Timeout:       5 * time.Second,
	}

	result, err := RunCommandWithRuntime(context.Background(), args)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "runtime url not found on sandbox")
	assert.Equal(t, RunCommandResult{}, result)
}

func TestRunCommandWithRuntime_SuccessfulExecution(t *testing.T) {
	handler := &mockProcessHandler{
		startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 42}},
			}}); err != nil {
				return err
			}
			if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
					Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte("hello world")},
				}},
			}}); err != nil {
				return err
			}
			if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
					Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte("some warning")},
				}},
			}}); err != nil {
				return err
			}
			return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}},
			}})
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	result, err := RunCommandWithRuntime(context.Background(), RunCmdFuncArgs{
		Sbx:           sbx,
		ProcessConfig: &process.ProcessConfig{Cmd: "echo", Args: []string{"hello"}},
		Timeout:       5 * time.Second,
	})
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
		startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
			if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 99}},
			}}); err != nil {
				return err
			}
			return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{
					ExitCode: 139, Exited: true, Error: ptr.To(errMsg),
				}},
			}})
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	result, err := RunCommandWithRuntime(context.Background(), RunCmdFuncArgs{
		Sbx: sbx, ProcessConfig: &process.ProcessConfig{Cmd: "crash"}, Timeout: 5 * time.Second,
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "segmentation fault")
	assert.Equal(t, uint32(99), result.PID)
	assert.Equal(t, int32(139), result.ExitCode)
	assert.True(t, result.Exited)
}

func TestRunCommandWithRuntime_ServerError(t *testing.T) {
	handler := &mockProcessHandler{
		startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
			return connect.NewError(connect.CodeInternal, fmt.Errorf("internal server error"))
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	_, err := RunCommandWithRuntime(context.Background(), RunCmdFuncArgs{
		Sbx: sbx, ProcessConfig: &process.ProcessConfig{Cmd: "test"}, Timeout: 5 * time.Second,
	})
	assert.Error(t, err)
}

func TestRunCommandWithRuntime_EmptyStream(t *testing.T) {
	handler := &mockProcessHandler{
		startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
			return nil // close stream without sending anything
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	result, err := RunCommandWithRuntime(context.Background(), RunCmdFuncArgs{
		Sbx: sbx, ProcessConfig: &process.ProcessConfig{Cmd: "noop"}, Timeout: 5 * time.Second,
	})
	assert.NoError(t, err)
	assert.Equal(t, uint32(0), result.PID)
	assert.Nil(t, result.Stdout)
	assert.Nil(t, result.Stderr)
}

func TestRunCommandWithRuntime_ContextTimeout(t *testing.T) {
	handler := &mockProcessHandler{
		startFn: func(ctx context.Context, _ *connect.Request[process.StartRequest], _ *connect.ServerStream[process.StartResponse]) error {
			<-ctx.Done()
			return ctx.Err()
		},
	}
	_, sbx := newMockRuntimeServer(t, handler)

	_, err := RunCommandWithRuntime(context.Background(), RunCmdFuncArgs{
		Sbx: sbx, ProcessConfig: &process.ProcessConfig{Cmd: "sleep"}, Timeout: 100 * time.Millisecond,
	})
	assert.Error(t, err)
}
