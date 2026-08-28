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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
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
			name: "runtime TLS sandbox without client bundle returns TLS transport error",
			box: newTestSandbox(map[string]string{
				agentsv1alpha1.AnnotationRuntimeTLSPort: "49984",
			}, ""),
			hook: &agentsv1alpha1.UpgradeAction{
				Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo tls"}},
			},
			expectedExitCode: -1,
			expectedStdout:   "",
			expectedStderr:   "",
			expectedErr:      "sandbox default/test-sandbox advertises runtime TLS port 49984 but no client TLS bundle is configured",
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

func TestNewLifecycleHookFunc_UsesRuntimeTLSBundle(t *testing.T) {
	tests := []struct {
		name        string
		box         *agentsv1alpha1.Sandbox
		expectedErr string
	}{
		{
			name: "configured bundle reaches TLS transport validation",
			box: func() *agentsv1alpha1.Sandbox {
				box := newTestSandbox(map[string]string{
					agentsv1alpha1.AnnotationRuntimeTLSPort: "49984",
				}, "")
				box.Status.PodInfo.PodIP = "10.0.0.1"
				return box
			}(),
			expectedErr: "invalid runtime TLS configuration: runtime TLS CA bundle is required",
		},
		{
			name: "pod IP not ready returns readiness error",
			box: newTestSandbox(map[string]string{
				agentsv1alpha1.AnnotationRuntimeTLSPort: "49984",
			}, ""),
			expectedErr: "pod IP not ready on sandbox default/test-sandbox",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hookFunc := NewLifecycleHookFunc(&agentsruntime.TLSBundle{})

			exitCode, stdout, stderr, err := hookFunc(context.Background(), tt.box, &agentsv1alpha1.UpgradeAction{
				Exec: &corev1.ExecAction{Command: []string{"/bin/bash", "-c", "echo tls"}},
			})

			if exitCode != -1 {
				t.Fatalf("hook exitCode = %d, want -1", exitCode)
			}
			if stdout != "" || stderr != "" {
				t.Fatalf("hook output = stdout %q stderr %q, want empty output", stdout, stderr)
			}
			if err == nil {
				t.Fatal("expected error but got nil")
			}
			if err.Error() != tt.expectedErr {
				t.Fatalf("hook error = %q, want %q", err.Error(), tt.expectedErr)
			}
		})
	}
}
