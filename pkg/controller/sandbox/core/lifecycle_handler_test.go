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

package core

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
)

func newTestSandbox(annotations map[string]string, sandboxIP string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test-sandbox",
			Namespace:   "default",
			Annotations: annotations,
		},
		Status: agentsv1alpha1.SandboxStatus{
			SandboxIp: sandboxIP,
		},
	}
}

func TestLifecycleHookFunc_NilBundle(t *testing.T) {
	hookFunc := NewLifecycleHookFunc(nil)
	tests := []struct {
		name             string
		box              *agentsv1alpha1.Sandbox
		hook             *agentsv1alpha1.UpgradeAction
		expectedExitCode int32
		expectedStdout   string
		expectedStderr   string
		expectedErr      string
		expectError      bool
	}{
		{
			name:             "nil hook returns success",
			box:              newTestSandbox(nil, "10.0.0.1"),
			hook:             nil,
			expectedExitCode: 0,
			expectedStdout:   "",
			expectedStderr:   "",
			expectError:      false,
		},
		{
			name:             "exec is nil returns success",
			box:              newTestSandbox(nil, "10.0.0.1"),
			hook:             &agentsv1alpha1.UpgradeAction{TimeoutSeconds: 30},
			expectedExitCode: 0,
			expectedStdout:   "",
			expectedStderr:   "",
			expectError:      false,
		},
		{
			name:             "runtime URL not found returns error",
			box:              newTestSandbox(nil, ""),
			hook:             &agentsv1alpha1.UpgradeAction{Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo hello"}}},
			expectedExitCode: -1,
			expectedStdout:   "",
			expectedStderr:   "",
			expectedErr:      "runtime URL not found on sandbox default/test-sandbox",
			expectError:      true,
		},
		{
			name: "runtime TLS stamp is ignored and the plaintext readiness gate applies",
			box: newTestSandbox(map[string]string{
				agentsv1alpha1.AnnotationRuntimeTLSPort: "49984",
			}, ""),
			hook: &agentsv1alpha1.UpgradeAction{
				Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo tls"}},
			},
			expectedExitCode: -1,
			expectedStdout:   "",
			expectedStderr:   "",
			expectedErr:      "runtime URL not found on sandbox default/test-sandbox",
			expectError:      true,
		},
		{
			name: "runtime URL found with explicit timeout, RunCommandWithRuntime fails",
			box: newTestSandbox(map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL: "http://127.0.0.1:19999",
			}, "10.0.0.1"),
			hook: &agentsv1alpha1.UpgradeAction{
				Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo hello"}},
				TimeoutSeconds: 5,
			},
			expectedExitCode: -1,
			expectedStdout:   "",
			expectedStderr:   "",
			expectError:      true,
		},
		{
			name: "runtime URL found with default timeout (TimeoutSeconds=0), RunCommandWithRuntime fails",
			box: newTestSandbox(map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL: "http://127.0.0.1:19999",
			}, "10.0.0.1"),
			hook: &agentsv1alpha1.UpgradeAction{
				Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo default-timeout"}},
				TimeoutSeconds: 0,
			},
			expectedExitCode: -1,
			expectedStdout:   "",
			expectedStderr:   "",
			expectError:      true,
		},
		{
			name: "runtime URL found with negative timeout uses default, RunCommandWithRuntime fails",
			box: newTestSandbox(map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL: "http://127.0.0.1:19999",
			}, "10.0.0.1"),
			hook: &agentsv1alpha1.UpgradeAction{
				Exec:           &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo negative-timeout"}},
				TimeoutSeconds: -10,
			},
			expectedExitCode: -1,
			expectedStdout:   "",
			expectedStderr:   "",
			expectError:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			exitCode, stdout, stderr, err := hookFunc(ctx, tt.box, tt.hook)

			if exitCode != tt.expectedExitCode {
				t.Errorf("lifecycle hook exitCode = %d, want %d", exitCode, tt.expectedExitCode)
			}
			if stdout != tt.expectedStdout {
				t.Errorf("lifecycle hook stdout = %q, want %q", stdout, tt.expectedStdout)
			}
			if stderr != tt.expectedStderr {
				t.Errorf("lifecycle hook stderr = %q, want %q", stderr, tt.expectedStderr)
			}
			if tt.expectError && err == nil {
				t.Error("Expected error but got none")
			}
			if !tt.expectError && err != nil {
				t.Errorf("Unexpected error: %v", err)
			}
			if tt.expectedErr != "" && err != nil && err.Error() != tt.expectedErr {
				t.Fatalf("lifecycle hook error = %q, want %q", err.Error(), tt.expectedErr)
			}
		})
	}
}

func TestNewLifecycleHookFunc_IgnoresRuntimeTLS(t *testing.T) {
	tests := []struct {
		name string
		// withRuntimeURL points the plaintext runtime URL at the fake envd.
		withRuntimeURL   bool
		podIP            string
		expectedExitCode int32
		expectedStdout   string
		expectedErr      string
		expectedStarts   int32
	}{
		{
			name:           "configured bundle is ignored and the hook runs on the plaintext runtime URL",
			withRuntimeURL: true,
			podIP:          "10.0.0.1",
			expectedStdout: "/bin/bash -c echo tls",
			expectedStarts: 1,
		},
		{
			name:             "without a runtime URL the plaintext readiness gate applies",
			expectedExitCode: -1,
			expectedErr:      "runtime URL not found on sandbox default/test-sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &recordingProcessHandler{}
			path, h := processconnect.NewProcessHandler(handler)
			mux := http.NewServeMux()
			mux.Handle(path, h)
			server := httptest.NewServer(mux)
			defer server.Close()

			annotations := map[string]string{
				agentsv1alpha1.AnnotationRuntimeTLSPort: "49984",
			}
			if tt.withRuntimeURL {
				annotations[agentsv1alpha1.AnnotationRuntimeURL] = server.URL
			}
			box := newTestSandbox(annotations, "")
			box.Status.PodInfo.PodIP = tt.podIP
			hookFunc := NewLifecycleHookFunc(&agentsruntime.TLSBundle{})

			exitCode, stdout, stderr, err := hookFunc(context.Background(), box, &agentsv1alpha1.UpgradeAction{
				Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo tls"}},
			})

			if tt.expectedErr != "" {
				if err == nil || err.Error() != tt.expectedErr {
					t.Fatalf("hook error = %v, want %q", err, tt.expectedErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected hook error: %v", err)
			}
			if exitCode != tt.expectedExitCode {
				t.Errorf("hook exitCode = %d, want %d", exitCode, tt.expectedExitCode)
			}
			if stdout != tt.expectedStdout || stderr != "" {
				t.Errorf("hook output = stdout %q stderr %q, want stdout %q and empty stderr", stdout, stderr, tt.expectedStdout)
			}
			if got := handler.starts.Load(); got != tt.expectedStarts {
				t.Errorf("runtime process starts = %d, want %d", got, tt.expectedStarts)
			}
		})
	}
}

// recordingProcessHandler stands in for the envd process API on the plaintext
// runtime URL: it counts Start calls and echoes the command line on stdout
// before reporting a clean exit.
type recordingProcessHandler struct {
	processconnect.UnimplementedProcessHandler
	starts atomic.Int32
}

func (h *recordingProcessHandler) Start(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	h.starts.Add(1)
	cmdline := strings.Join(append([]string{req.Msg.GetProcess().GetCmd()}, req.Msg.GetProcess().GetArgs()...), " ")
	for _, event := range []*process.ProcessEvent{
		{Event: &process.ProcessEvent_Start{Start: &process.ProcessEvent_StartEvent{Pid: 1}}},
		{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{
			Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte(cmdline)},
		}}},
		{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}}},
	} {
		if err := stream.Send(&process.StartResponse{Event: event}); err != nil {
			return err
		}
	}
	return nil
}

// TestExecuteLifecycleHook_UsesGivenTransport covers the TLS branch that
// NewLifecycleHookFunc does not take: the given options, not the sandbox
// capability stamp, select the transport.
func TestExecuteLifecycleHook_UsesGivenTransport(t *testing.T) {
	tlsOptions := []agentsruntime.Option{
		agentsruntime.WithTLS(agentsruntime.TLSBundle{}),
		agentsruntime.WithTLSPort(agentsruntime.RuntimeTLSPort),
	}
	tests := []struct {
		name             string
		action           *agentsv1alpha1.UpgradeAction
		expectedExitCode int32
		expectedErr      string
	}{
		{
			name:   "action without exec is a no-op before any readiness gate",
			action: &agentsv1alpha1.UpgradeAction{TimeoutSeconds: 30},
		},
		{
			name: "TLS options on an unstamped sandbox fail the TLS readiness gate",
			action: &agentsv1alpha1.UpgradeAction{
				Exec: &corev1.ExecAction{Command: []string{"echo", "hook"}},
			},
			expectedExitCode: -1,
			expectedErr:      "pod IP not ready on sandbox default/test-sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// The plaintext runtime URL is set, so only the TLS options can
			// route the hook to the Pod-IP readiness gate.
			box := newTestSandbox(map[string]string{
				agentsv1alpha1.AnnotationRuntimeURL: "http://127.0.0.1:19999",
			}, "")

			exitCode, stdout, stderr, err := executeLifecycleHook(context.Background(), box, tt.action, tlsOptions, "root")

			if tt.expectedErr != "" {
				if err == nil || err.Error() != tt.expectedErr {
					t.Fatalf("hook error = %v, want %q", err, tt.expectedErr)
				}
			} else if err != nil {
				t.Fatalf("unexpected hook error: %v", err)
			}
			if exitCode != tt.expectedExitCode {
				t.Errorf("hook exitCode = %d, want %d", exitCode, tt.expectedExitCode)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("hook output = stdout %q stderr %q, want empty output", stdout, stderr)
			}
		})
	}
}

type lifecycleProcessHandler struct {
	processconnect.UnimplementedProcessHandler
	headers chan http.Header
}

func (h *lifecycleProcessHandler) Start(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	h.headers <- req.Header().Clone()
	for _, event := range []*process.ProcessEvent{
		{Event: &process.ProcessEvent_Data{Data: &process.ProcessEvent_DataEvent{Output: &process.ProcessEvent_DataEvent_Stdout{Stdout: []byte("secret")}}}},
		{Event: &process.ProcessEvent_End{End: &process.ProcessEvent_EndEvent{ExitCode: 0, Exited: true}}},
	} {
		if err := stream.Send(&process.StartResponse{Event: event}); err != nil {
			return err
		}
	}
	return nil
}

func TestLifecycleHookExecutorsUseServerTimeoutAndExpectedUser(t *testing.T) {
	rootRequest, err := http.NewRequest(http.MethodPost, "http://example.com", nil)
	require.NoError(t, err)
	rootRequest.SetBasicAuth("root", "")

	tests := []struct {
		name               string
		newHook            LifecycleHookFunc
		expectedAuthHeader string
	}{
		{
			name:               "upgrade hook uses root",
			newHook:            NewLifecycleHookFunc(nil),
			expectedAuthHeader: rootRequest.Header.Get("Authorization"),
		},
		{
			name:               "pause resume hook defaults to root",
			newHook:            NewPauseResumeLifecycleHookFunc(nil),
			expectedAuthHeader: rootRequest.Header.Get("Authorization"),
		},
		{
			name:               "pause resume hook with configured root user sends that identity",
			newHook:            NewPauseResumeLifecycleHookFuncWithUser(nil, "root"),
			expectedAuthHeader: rootRequest.Header.Get("Authorization"),
		},
		{
			name:               "pause resume hook with empty user sends no credential",
			newHook:            NewPauseResumeLifecycleHookFuncWithUser(nil, ""),
			expectedAuthHeader: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &lifecycleProcessHandler{headers: make(chan http.Header, 1)}
			_, processHandler := processconnect.NewProcessHandler(handler)
			server := httptest.NewServer(processHandler)
			t.Cleanup(server.Close)
			box := newTestSandbox(map[string]string{agentsv1alpha1.AnnotationRuntimeURL: server.URL}, "10.0.0.1")

			exitCode, stdout, stderr, hookErr := tt.newHook(t.Context(), box, &agentsv1alpha1.LifecycleAction{
				Exec:           &corev1.ExecAction{Command: []string{"true"}},
				TimeoutSeconds: 1,
			})
			require.NoError(t, hookErr)
			assert.Equal(t, int32(0), exitCode)
			assert.Equal(t, "secret", stdout)
			assert.Empty(t, stderr)
			headers := <-handler.headers
			assert.Equal(t, "1000", headers.Get("Connect-Timeout-Ms"))
			assert.Equal(t, tt.expectedAuthHeader, headers.Get("Authorization"))
		})
	}

}
