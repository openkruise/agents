// command_test.go
package sandboxcr

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/openkruise/agents/api/v1alpha1"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

var AccessToken = "access-token"

type TestProcessService struct {
	responses []process.StartResponse
	stop      bool
}

func (s *TestProcessService) List(context.Context, *connect.Request[process.ListRequest]) (*connect.Response[process.ListResponse], error) {
	return connect.NewResponse(&process.ListResponse{}), nil
}

func (s *TestProcessService) Connect(context.Context, *connect.Request[process.ConnectRequest], *connect.ServerStream[process.ConnectResponse]) error {
	return nil
}

func (s *TestProcessService) Start(_ context.Context, req *connect.Request[process.StartRequest], stream *connect.ServerStream[process.StartResponse]) error {
	if req.Header().Get("X-Access-Token") != AccessToken {
		return connect.NewError(connect.CodeUnauthenticated, nil)
	}
	for i := range s.responses {
		if err := stream.Send(&s.responses[i]); err != nil {
			return err
		}
	}
	if !s.stop {
		time.Sleep(500 * time.Millisecond)
	}
	return nil
}

func (s *TestProcessService) Update(context.Context, *connect.Request[process.UpdateRequest]) (*connect.Response[process.UpdateResponse], error) {
	return connect.NewResponse(&process.UpdateResponse{}), nil
}

func (s *TestProcessService) StreamInput(context.Context, *connect.ClientStream[process.StreamInputRequest]) (*connect.Response[process.StreamInputResponse], error) {
	return connect.NewResponse(&process.StreamInputResponse{}), nil
}

func (s *TestProcessService) SendInput(context.Context, *connect.Request[process.SendInputRequest]) (*connect.Response[process.SendInputResponse], error) {
	return connect.NewResponse(&process.SendInputResponse{}), nil
}

func (s *TestProcessService) SendSignal(context.Context, *connect.Request[process.SendSignalRequest]) (*connect.Response[process.SendSignalResponse], error) {
	return connect.NewResponse(&process.SendSignalResponse{}), nil
}

func NewTestEnvdServer(result RunCommandResult, stop bool, err *string) *httptest.Server {
	testResponses := []process.StartResponse{
		{
			Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Start{
					Start: &process.ProcessEvent_StartEvent{
						Pid: result.PID,
					},
				},
			},
		},
	}

	for _, stdout := range result.Stdout {
		testResponses = append(testResponses, process.StartResponse{
			Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Data{
					Data: &process.ProcessEvent_DataEvent{
						Output: &process.ProcessEvent_DataEvent_Stdout{
							Stdout: []byte(stdout),
						},
					},
				},
			},
		})
	}

	for _, stderr := range result.Stderr {
		testResponses = append(testResponses,
			process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_Keepalive{},
				},
			}, process.StartResponse{
				Event: &process.ProcessEvent{
					Event: &process.ProcessEvent_Data{
						Data: &process.ProcessEvent_DataEvent{
							Output: &process.ProcessEvent_DataEvent_Stderr{
								Stderr: []byte(stderr),
							},
						},
					},
				},
			})
	}

	testResponses = append(testResponses, process.StartResponse{
		Event: &process.ProcessEvent{
			Event: &process.ProcessEvent_End{
				End: &process.ProcessEvent_EndEvent{
					ExitCode: result.ExitCode,
					Exited:   result.Exited,
					Error:    err,
				},
			},
		},
	})

	service := &TestProcessService{
		responses: testResponses,
		stop:      stop,
	}
	_, handler := processconnect.NewProcessHandler(service)
	server := httptest.NewServer(handler)
	return server
}

func TestSandbox_runCommandWithEnvd(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name         string
		timeout      time.Duration
		accessToken  string
		stop         bool
		result       RunCommandResult
		processError *string
		expectError  string
	}{
		{
			name:        "success",
			timeout:     10 * time.Second,
			stop:        true,
			accessToken: AccessToken,
			result: RunCommandResult{
				PID:      10086,
				Stdout:   []string{"Hello", "World"},
				Stderr:   []string{"Some", "Error"},
				ExitCode: 5,
				Exited:   true,
			},
		},
		{
			name:        "error",
			timeout:     10 * time.Second,
			stop:        true,
			accessToken: AccessToken,
			result: RunCommandResult{
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
			stop:        false,
			accessToken: AccessToken,
			result: RunCommandResult{
				PID:    10086,
				Stdout: []string{"Hello", "World"},
				Stderr: []string{"Some", "Error"},
				Exited: false,
			},
			expectError: "execution timeout",
		},
		{
			name:    "unauthorized",
			timeout: 100 * time.Millisecond,
			stop:    true,
			result: RunCommandResult{
				PID:    10086,
				Stdout: []string{"Hello", "World"},
				Stderr: []string{"Some", "Error"},
				Exited: false,
			},
			expectError: "stream error: unauthenticated",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := NewTestEnvdServer(tt.result, tt.stop, tt.processError)
			defer server.Close()

			cache, client := NewTestCache(t)
			sbx := &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Name: "test-sandbox",
					Annotations: map[string]string{
						v1alpha1.AnnotationEnvdURL:         server.URL,
						v1alpha1.AnnotationEnvdAccessToken: tt.accessToken,
					},
				},
			}
			sandbox := AsSandboxForTest(sbx, client, cache)
			result, err := sandbox.runCommandWithEnvd(t.Context(), &process.ProcessConfig{}, tt.timeout)

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
