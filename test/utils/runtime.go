package utils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"time"

	"connectrpc.com/connect"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
	"k8s.io/klog/v2"
)

var AccessToken = "access-token"

type RunCommandResult struct {
	PID      uint32
	Stdout   []string
	Stderr   []string
	ExitCode int32
	Exited   bool
	Error    error
}

type TestRuntimeServerOptions struct {
	RunCommandResult      RunCommandResult
	RunCommandImmediately bool
	RunCommandError       *string
	InitErrCode           int
}

func NewTestRuntimeServer(opts TestRuntimeServerOptions) *httptest.Server {
	testResponses := []process.StartResponse{
		{
			Event: &process.ProcessEvent{
				Event: &process.ProcessEvent_Start{
					Start: &process.ProcessEvent_StartEvent{
						Pid: opts.RunCommandResult.PID,
					},
				},
			},
		},
	}

	for _, stdout := range opts.RunCommandResult.Stdout {
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

	for _, stderr := range opts.RunCommandResult.Stderr {
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
					ExitCode: opts.RunCommandResult.ExitCode,
					Exited:   opts.RunCommandResult.Exited,
					Error:    opts.RunCommandError,
				},
			},
		},
	})

	service := &TestProcessService{
		responses:   testResponses,
		immediately: opts.RunCommandImmediately,
	}
	mux := http.NewServeMux()
	grpcPath, handler := processconnect.NewProcessHandler(service)
	mux.Handle(grpcPath, handler)
	web.RegisterRoute(mux, http.MethodPost, "/init", func(r *http.Request) (response web.ApiResponse[struct{}], err *web.ApiError) {
		if opts.InitErrCode > 300 {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code: opts.InitErrCode,
			}
		}
		return web.ApiResponse[struct{}]{
			Code: http.StatusNoContent,
			Body: struct{}{},
		}, nil
	})
	return httptest.NewServer(mux)
}

type TestProcessService struct {
	responses   []process.StartResponse
	immediately bool
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
	start := time.Now()
	for i := range s.responses[:len(s.responses)-1] {
		if err := stream.Send(&s.responses[i]); err != nil {
			return err
		}
	}
	if !s.immediately {
		time.Sleep(500 * time.Millisecond)
	}
	if err := stream.Send(&s.responses[len(s.responses)-1]); err != nil {
		return err
	}
	klog.InfoS("all messages are sent", "duration", time.Since(start))
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
