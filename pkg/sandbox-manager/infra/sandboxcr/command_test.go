// command_test.go
package sandboxcr

import (
	"context"
	"testing"
	"time"

	testutils "github.com/openkruise/agents/test/utils"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/api/v1alpha1"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/proto/envd/process"
)

func TestSandbox_runCommandWithEnvd(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name         string
		timeout      time.Duration
		accessToken  string
		immediately  bool
		result       testutils.RunCommandResult
		processError *string
		expectError  string
	}{
		{
			name:        "success",
			timeout:     100 * time.Second,
			immediately: true,
			accessToken: testutils.AccessToken,
			result: testutils.RunCommandResult{
				PID:      10086,
				Stdout:   []string{"Hello", "World"},
				Stderr:   []string{"Some", "Error"},
				ExitCode: 5,
				Exited:   true,
			},
		},
		{
			name:        "error",
			timeout:     100 * time.Second,
			immediately: true,
			accessToken: testutils.AccessToken,
			result: testutils.RunCommandResult{
				PID:      10086,
				Stdout:   []string{"Hello", "World"},
				Stderr:   []string{"Some", "Error"},
				ExitCode: 5,
				Exited:   true,
			},
			processError: ptr.To("some error"),
			expectError:  "some error",
		},
		{
			name:        "timeout",
			timeout:     100 * time.Millisecond,
			immediately: false,
			accessToken: testutils.AccessToken,
			result: testutils.RunCommandResult{
				PID:    10086,
				Stdout: []string{"Hello", "World"},
				Stderr: []string{"Some", "Error"},
				Exited: false,
			},
			expectError: "deadline_exceeded: context deadline exceeded",
		},
		{
			name:        "unauthorized",
			timeout:     100 * time.Millisecond,
			immediately: true,
			result: testutils.RunCommandResult{
				PID:    10086,
				Stdout: []string{"Hello", "World"},
				Stderr: []string{"Some", "Error"},
				Exited: false,
			},
			expectError: "unauthenticated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := testutils.TestRuntimeServerOptions{
				RunCommandResult:      tt.result,
				RunCommandImmediately: tt.immediately,
				RunCommandError:       tt.processError,
			}
			server := testutils.NewTestRuntimeServer(opts)
			defer server.Close()

			cache, clientSet, err := NewTestCache(t)
			assert.NoError(t, err)
			defer cache.Stop()
			client := clientSet.SandboxClient
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox",
					Annotations: map[string]string{
						v1alpha1.AnnotationRuntimeURL:         server.URL,
						v1alpha1.AnnotationRuntimeAccessToken: tt.accessToken,
					},
				},
			}
			sandbox := AsSandbox(sbx, cache, client)
			result, err := sandbox.runCommandWithRuntime(context.Background(), &process.ProcessConfig{}, tt.timeout)

			if tt.expectError != "" {
				assert.Error(t, err)
				if err != nil {
					assert.Contains(t, err.Error(), tt.expectError)
				}
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.result, result)
			}
		})
	}
}
