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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/pkg/utils"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/proto/envd/process"
)

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

// ------------------ WriteFileWithRuntime ------------------

// newWriteFileSandbox builds a Sandbox whose annotations point WriteFileWithRuntime at the
// supplied runtime URL and (optionally) carry an access token. Helper used by the
// WriteFileWithRuntime test family.
func newWriteFileSandbox(runtimeURL, accessToken string) *agentsv1alpha1.Sandbox {
	annos := map[string]string{}
	if runtimeURL != "" {
		annos[agentsv1alpha1.AnnotationRuntimeURL] = runtimeURL
	}
	if accessToken != "" {
		annos[agentsv1alpha1.AnnotationRuntimeAccessToken] = accessToken
	}
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "sbx-files",
			Namespace:   "default",
			UID:         types.UID("uid-files"),
			Annotations: annos,
		},
	}
}

// capturedFilesRequest records the relevant aspects of a request received by the fake
// runtime files server, so test assertions can inspect them after the call returns.
type capturedFilesRequest struct {
	method          string
	path            string
	query           url.Values
	accessToken     string
	authorization   string
	contentType     string
	multipartFields map[string][]byte // form field name -> raw file content
}

// newRuntimeFilesServer spins up a httptest server that captures every incoming files
// API request into *captured and replies with the given statusCode and body.
func newRuntimeFilesServer(t *testing.T, statusCode int, respBody string, captured *capturedFilesRequest) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.query = r.URL.Query()
		captured.accessToken = r.Header.Get("X-Access-Token")
		captured.authorization = r.Header.Get("Authorization")
		captured.contentType = r.Header.Get("Content-Type")

		if strings.HasPrefix(captured.contentType, "multipart/") {
			mediaParams := captured.contentType
			boundaryIdx := strings.Index(mediaParams, "boundary=")
			require.GreaterOrEqual(t, boundaryIdx, 0, "multipart Content-Type must include boundary")
			boundary := mediaParams[boundaryIdx+len("boundary="):]
			mr := multipart.NewReader(r.Body, boundary)
			captured.multipartFields = map[string][]byte{}
			for {
				part, err := mr.NextPart()
				if err == io.EOF {
					break
				}
				require.NoError(t, err)
				content, rerr := io.ReadAll(part)
				require.NoError(t, rerr)
				captured.multipartFields[part.FormName()] = content
				_ = part.Close()
			}
		}

		w.WriteHeader(statusCode)
		if respBody != "" {
			_, _ = w.Write([]byte(respBody))
		}
	}))
}

func TestWriteFileWithRuntime_InputValidation(t *testing.T) {
	tests := []struct {
		name        string
		args        WriteFileArgs
		expectError string
	}{
		{
			name:        "nil sandbox is rejected",
			args:        WriteFileArgs{Sbx: nil, FilePath: "/tmp/a", Content: []byte("x")},
			expectError: "sandbox is nil",
		},
		{
			name:        "empty filePath is rejected",
			args:        WriteFileArgs{Sbx: newWriteFileSandbox("http://ignored", ""), FilePath: "", Content: []byte("x")},
			expectError: "filePath is required",
		},
		{
			name: "missing runtime url is rejected",
			// No annotation, no PodIP — GetRuntimeURL returns "".
			args:        WriteFileArgs{Sbx: newWriteFileSandbox("", ""), FilePath: "/tmp/a", Content: []byte("x")},
			expectError: "runtime url not found on sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := WriteFileWithRuntime(context.Background(), tt.args)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.Equal(t, 0, res.StatusCode, "no HTTP call should be made on input validation failure")
		})
	}
}

func TestWriteFileWithRuntime_HTTPInteractions(t *testing.T) {
	type caseSpec struct {
		name             string
		accessToken      string // sandbox annotation; empty = unset
		usernameOverride string
		content          []byte
		filePath         string
		serverStatus     int
		serverBody       string
		expectError      string // empty == expect success
		expectStatusCode int
		expectUsername   string
		expectXToken     string // value expected on X-Access-Token header
	}

	tests := []caseSpec{
		{
			name:             "happy path with default username and access token",
			accessToken:      "runtime-token-1",
			content:          []byte(`{"accessToken":"tok-1"}`),
			filePath:         "/var/opt/sandbox/agent-token/abcd.token",
			serverStatus:     http.StatusOK,
			expectStatusCode: http.StatusOK,
			expectUsername:   "root",
			expectXToken:     "runtime-token-1",
		},
		{
			name:             "custom username overrides default root",
			accessToken:      "runtime-token-2",
			usernameOverride: "agent",
			content:          []byte("hello"),
			filePath:         "/data/file.bin",
			serverStatus:     http.StatusOK,
			expectStatusCode: http.StatusOK,
			expectUsername:   "agent",
			expectXToken:     "runtime-token-2",
		},
		{
			name:             "missing access token annotation does not send X-Access-Token header",
			accessToken:      "", // unset
			content:          []byte("payload"),
			filePath:         "/tmp/no-token",
			serverStatus:     http.StatusOK,
			expectStatusCode: http.StatusOK,
			expectUsername:   "root",
			expectXToken:     "", // header should not be set
		},
		{
			name:             "server returns 4xx with body is wrapped into error",
			accessToken:      "runtime-token-3",
			content:          []byte("x"),
			filePath:         "/tmp/forbidden",
			serverStatus:     http.StatusForbidden,
			serverBody:       "forbidden by policy",
			expectError:      "runtime files API returned status 403: forbidden by policy",
			expectStatusCode: http.StatusForbidden,
			expectUsername:   "root",
			expectXToken:     "runtime-token-3",
		},
		{
			name:             "server returns 5xx is wrapped into error",
			accessToken:      "runtime-token-4",
			content:          []byte("x"),
			filePath:         "/tmp/boom",
			serverStatus:     http.StatusInternalServerError,
			serverBody:       "internal",
			expectError:      "runtime files API returned status 500: internal",
			expectStatusCode: http.StatusInternalServerError,
			expectUsername:   "root",
			expectXToken:     "runtime-token-4",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			captured := &capturedFilesRequest{}
			server := newRuntimeFilesServer(t, tt.serverStatus, tt.serverBody, captured)
			defer server.Close()

			sbx := newWriteFileSandbox(server.URL, tt.accessToken)
			args := WriteFileArgs{
				Sbx:      sbx,
				FilePath: tt.filePath,
				Content:  tt.content,
				Username: tt.usernameOverride,
			}

			res, err := WriteFileWithRuntime(context.Background(), args)

			if tt.expectError == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
			assert.Equal(t, tt.expectStatusCode, res.StatusCode)

			// Server-side observed request invariants.
			assert.Equal(t, http.MethodPost, captured.method)
			assert.Equal(t, "/files", captured.path)
			assert.Equal(t, tt.filePath, captured.query.Get("path"))
			assert.Equal(t, tt.expectUsername, captured.query.Get("username"))
			assert.Equal(t, tt.expectXToken, captured.accessToken,
				"X-Access-Token header should match the access token annotation (empty when unset)")
			assert.Equal(t, "Basic cm9vdDo=", captured.authorization,
				"Authorization header must mirror RunCommandWithRuntime")
			assert.True(t, strings.HasPrefix(captured.contentType, "multipart/form-data"),
				"Content-Type should be multipart/form-data, got %q", captured.contentType)

			// Multipart "file" field must carry the exact bytes we passed in.
			require.NotNil(t, captured.multipartFields, "multipart body should be parsable")
			assert.Equal(t, tt.content, captured.multipartFields["file"],
				"multipart 'file' field must carry the original Content bytes verbatim")
		})
	}
}

// TestWriteFileWithRuntime_TransportError exercises the path where the HTTP transport
// itself fails (server already closed before the call). Errors must be wrapped with the
// "failed to call runtime files API" prefix and StatusCode must remain zero.
func TestWriteFileWithRuntime_TransportError(t *testing.T) {
	captured := &capturedFilesRequest{}
	server := newRuntimeFilesServer(t, http.StatusOK, "", captured)
	// Tear it down before issuing the request to force a connection-refused style error.
	server.Close()

	sbx := newWriteFileSandbox(server.URL, "runtime-token")
	args := WriteFileArgs{Sbx: sbx, FilePath: "/tmp/x", Content: []byte("x")}

	res, err := WriteFileWithRuntime(context.Background(), args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to call runtime files API")
	assert.Equal(t, 0, res.StatusCode)
}

// TestWriteFileWithRuntime_ContextTimeout verifies that a small args.Timeout aborts a
// slow server response with a transport error. The error is wrapped with the standard
// "failed to call runtime files API" prefix and StatusCode is left zero.
func TestWriteFileWithRuntime_ContextTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(2 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
	}))
	defer server.Close()

	sbx := newWriteFileSandbox(server.URL, "runtime-token")
	args := WriteFileArgs{
		Sbx:      sbx,
		FilePath: "/tmp/slow",
		Content:  []byte("x"),
		Timeout:  50 * time.Millisecond,
	}

	start := time.Now()
	res, err := WriteFileWithRuntime(context.Background(), args)
	assert.Less(t, time.Since(start), 1500*time.Millisecond, "timeout should fire well before the server replies")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed to call runtime files API")
	assert.Equal(t, 0, res.StatusCode)
}

// TestWriteFileWithRuntime_HonorsLargeArgsTimeout regression-tests the contract that
// args.Timeout is the single source of truth for the request deadline. Earlier
// revisions installed a package-level http.Client.Timeout = defaultRuntimeWriteTimeout
// (10s), which silently capped any args.Timeout above 10s — args.Timeout = 30s would
// still be killed at 10s. After the fix the client carries no Timeout, so a server
// that replies quickly must succeed even when args.Timeout sits well above the old cap.
func TestWriteFileWithRuntime_HonorsLargeArgsTimeout(t *testing.T) {
	captured := &capturedFilesRequest{}
	server := newRuntimeFilesServer(t, http.StatusOK, "", captured)
	defer server.Close()

	sbx := newWriteFileSandbox(server.URL, "runtime-token")
	args := WriteFileArgs{
		Sbx:      sbx,
		FilePath: "/tmp/large-timeout",
		Content:  []byte("payload"),
		// Far above the legacy client-level cap of 10s. Must not be silently capped.
		Timeout: 30 * time.Second,
	}

	start := time.Now()
	res, err := WriteFileWithRuntime(context.Background(), args)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, res.StatusCode)
	// The request itself is fast; we only assert it returned in well under the timeout
	// to make sure no hidden cap aborted it. Generous bound to keep CI stable.
	assert.Less(t, time.Since(start), 5*time.Second)
}

func TestBuildRuntimeFilesEndpoint(t *testing.T) {
	tests := []struct {
		name       string
		runtimeURL string
		filePath   string
		username   string
		expect     string
	}{
		{
			name:       "plain url and absolute path",
			runtimeURL: "http://10.0.0.1:49983",
			filePath:   "/tmp/foo.txt",
			username:   "root",
			expect:     "http://10.0.0.1:49983/files?path=%2Ftmp%2Ffoo.txt&username=root",
		},
		{
			name:       "trailing slash on runtime url is stripped",
			runtimeURL: "http://10.0.0.1:49983/",
			filePath:   "/tmp/foo.txt",
			username:   "root",
			expect:     "http://10.0.0.1:49983/files?path=%2Ftmp%2Ffoo.txt&username=root",
		},
		{
			name:       "path with spaces and special chars is URL-encoded",
			runtimeURL: "http://host:8080",
			filePath:   "/tmp/a b/c&d.txt",
			username:   "agent user",
			expect:     "http://host:8080/files?path=%2Ftmp%2Fa+b%2Fc%26d.txt&username=agent+user",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildRuntimeFilesEndpoint(tt.runtimeURL, tt.filePath, tt.username)
			assert.Equal(t, tt.expect, got)
		})
	}
}

func TestBuildRuntimeFilesMultipartBody(t *testing.T) {
	tests := []struct {
		name     string
		filePath string
		content  []byte
		wantName string // expected multipart filename derived from filePath
	}{
		{
			name:     "absolute path uses its base name as filename",
			filePath: "/var/opt/sandbox/agent-token/abcd.token",
			content:  []byte(`{"accessToken":"tok-1"}`),
			wantName: "abcd.token",
		},
		{
			name:     "empty content still produces a parsable file part",
			filePath: "/tmp/empty.bin",
			content:  []byte{},
			wantName: "empty.bin",
		},
		{
			name:     "binary content is preserved byte-for-byte",
			filePath: "/tmp/bin.dat",
			content:  []byte{0x00, 0x01, 0xFF, 0x7F, 0x80, 0x00},
			wantName: "bin.dat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, contentType, err := buildRuntimeFilesMultipartBody(tt.filePath, tt.content)
			require.NoError(t, err)
			require.NotNil(t, body)

			// contentType must include a boundary token usable for parsing.
			assert.True(t, strings.HasPrefix(contentType, "multipart/form-data"))
			boundaryIdx := strings.Index(contentType, "boundary=")
			require.GreaterOrEqual(t, boundaryIdx, 0)
			boundary := contentType[boundaryIdx+len("boundary="):]

			mr := multipart.NewReader(bytes.NewReader(body.Bytes()), boundary)
			part, err := mr.NextPart()
			require.NoError(t, err)
			assert.Equal(t, runtimeFilesFieldName, part.FormName(), "form field must be 'file'")
			assert.Equal(t, tt.wantName, part.FileName(), "filename must be the base of filePath")

			gotContent, err := io.ReadAll(part)
			require.NoError(t, err)
			assert.Equal(t, tt.content, gotContent, "content must round-trip byte-for-byte")

			// Only one part is expected.
			_, err = mr.NextPart()
			assert.ErrorIs(t, err, io.EOF, "only a single 'file' part is expected")
		})
	}
}

func TestGetRuntimeURL(t *testing.T) {
	tests := []struct {
		name     string
		sandbox  *agentsv1alpha1.Sandbox
		expected string
	}{
		{
			name:     "nil sandbox returns empty string",
			sandbox:  nil,
			expected: "",
		},
		{
			name: "runtime-url annotation hits and is returned directly",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeURL: "http://runtime.example.com",
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
				},
			},
			expected: "http://runtime.example.com",
		},
		{
			name: "legacy envd-url annotation hits when runtime-url missing",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationEnvdURL: "http://envd.example.com",
					},
				},
			},
			expected: "http://envd.example.com",
		},
		{
			name: "runtime-url takes precedence over legacy envd-url",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeURL: "http://runtime.example.com",
						agentsv1alpha1.AnnotationEnvdURL:    "http://envd.example.com",
					},
				},
			},
			expected: "http://runtime.example.com",
		},
		{
			name: "no annotation but PodIP present falls back to ip:port",
			sandbox: &agentsv1alpha1.Sandbox{
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "10.0.0.1",
					},
				},
			},
			expected: fmt.Sprintf("http://10.0.0.1:%d", utils.RuntimePort),
		},
		{
			name: "empty annotation value falls back to ip:port",
			sandbox: &agentsv1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						agentsv1alpha1.AnnotationRuntimeURL: "",
						agentsv1alpha1.AnnotationEnvdURL:    "",
					},
				},
				Status: agentsv1alpha1.SandboxStatus{
					Phase: agentsv1alpha1.SandboxRunning,
					PodInfo: agentsv1alpha1.PodInfo{
						PodIP: "10.0.0.2",
					},
				},
			},
			expected: fmt.Sprintf("http://10.0.0.2:%d", utils.RuntimePort),
		},
		{
			name:     "no annotation and no PodIP returns empty string",
			sandbox:  &agentsv1alpha1.Sandbox{},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, GetRuntimeURL(tt.sandbox))
		})
	}
}

// TestChmodFileOnRuntime is a table-driven contract test for ChmodFileOnRuntime.
// It pins three behaviors:
//  1. The function delegates to RunCommandWithRuntime with cmd="chmod" and
//     args=[mode, filePath] in that exact order;
//  2. A non-zero exit code surfaces as an error containing the exit code and stderr,
//     and the underlying transport error path is wrapped with "chmod command failed";
//  3. Missing runtime URL on the sandbox short-circuits to a wrapped error,
//     i.e. it does not panic and does not silently succeed.
func TestChmodFileOnRuntime(t *testing.T) {
	tests := []struct {
		name     string
		filePath string
		mode     string
		// startFn is the mock Process.Start handler. When nil, the test uses a
		// sandbox without runtime URL to exercise the short-circuit path.
		startFn      func(ctx context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error
		noRuntimeURL bool
		expectError  string // empty means no error expected; otherwise err.Error() must contain this
	}{
		{
			name:     "success: exit code 0 returns nil and forwards correct cmd/args",
			filePath: "/var/opt/sandbox/agent-token/access.token",
			mode:     "0600",
			startFn: func(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				// Contract: cmd is "chmod", args are [mode, filePath] in that order.
				cfg := req.Msg.GetProcess()
				if cfg == nil || cfg.Cmd != "chmod" {
					return fmt.Errorf("expected cmd=chmod, got %+v", cfg)
				}
				if len(cfg.Args) != 2 || cfg.Args[0] != "0600" || cfg.Args[1] != "/var/opt/sandbox/agent-token/access.token" {
					return fmt.Errorf("unexpected args, got %v", cfg.Args)
				}
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}},
				}})
			},
			expectError: "",
		},
		{
			name:     "non-zero exit surfaces stderr and exit code",
			filePath: "/etc/shadow",
			mode:     "0644",
			startFn: func(_ context.Context, _ *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
				if err := stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
						Output: &process.ProcessEvent_DataEvent_Stderr{Stderr: []byte("Operation not permitted")},
					}},
				}}); err != nil {
					return err
				}
				return stream.Send(&process.StartResponse{Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 1, Exited: true}},
				}})
			},
			expectError: "chmod exited with code 1",
		},
		{
			name:         "missing runtime URL is wrapped as chmod command failed",
			filePath:     "/tmp/x",
			mode:         "0600",
			noRuntimeURL: true,
			expectError:  "chmod command failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sbx *agentsv1alpha1.Sandbox
			if tt.noRuntimeURL {
				sbx = &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{Name: "test-sandbox", Namespace: "default"},
				}
			} else {
				_, sbx = newMockRuntimeServer(t, &mockProcessHandler{startFn: tt.startFn})
			}

			err := ChmodFileOnRuntime(context.Background(), sbx, tt.filePath, tt.mode)
			if tt.expectError == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
		})
	}
}
