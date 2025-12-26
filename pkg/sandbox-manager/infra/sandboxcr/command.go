package sandboxcr

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/proto/envd/process"
	"github.com/openkruise/agents/pkg/sandbox-manager/proto/envd/process/processconnect"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"k8s.io/klog/v2"
)

type RunCommandResult struct {
	PID      uint32
	Stdout   []string
	Stderr   []string
	ExitCode int32
	Exited   bool
}

// runCommandWithEnvd is a temporary solution to run command inside the sandbox,
// which will be replaced by `sandbox-runtime` in the future
func (s *Sandbox) runCommandWithEnvd(ctx context.Context, processConfig *process.ProcessConfig, timeout time.Duration) (RunCommandResult, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox))
	url := s.GetAnnotations()[models.AnnotationEnvdURL]
	if url == "" {
		return RunCommandResult{}, fmt.Errorf("envd url not found on sandbox")
	}
	client := processconnect.NewProcessClient(
		&http.Client{
			Timeout: timeout,
		},
		url,
		connect.WithGRPC(),
	)

	clientContext, callInfo := connect.NewClientContext(ctx)
	callInfo.RequestHeader().Set("X-Access-Token", s.Annotations[models.AnnotationEnvdAccessToken])

	req := connect.NewRequest(&process.StartRequest{
		Process: processConfig,
		Tag:     nil,
		Pty:     nil,
		Stdin:   nil,
	})

	var result RunCommandResult

	defer log.V(consts.DebugLogLevel).Info("run command result", "result", result)

	stream, err := client.Start(clientContext, req)
	if err != nil {
		return result, err
	}

	var end bool
	for stream.Receive() {
		response := stream.Msg()
		event := response.Event

		switch evt := event.Event.(type) {
		case *process.ProcessEvent_Start:
			pid := evt.Start.Pid
			result.PID = pid
		case *process.ProcessEvent_Data:
			switch data := evt.Data.Output.(type) {
			case *process.ProcessEvent_DataEvent_Stdout:
				result.Stdout = append(result.Stdout, string(data.Stdout))
			case *process.ProcessEvent_DataEvent_Stderr:
				result.Stderr = append(result.Stderr, string(data.Stderr))
			case *process.ProcessEvent_DataEvent_Pty: // not supported
			}

		case *process.ProcessEvent_End:
			result.ExitCode = evt.End.ExitCode
			result.Exited = evt.End.Exited
			if evt.End.Error != nil {
				return result, fmt.Errorf("process error: %s", *evt.End.Error)
			}
			end = true

		case *process.ProcessEvent_Keepalive:
			continue
		}

		if end {
			break
		}
	}

	if stream.Err() != nil {
		return result, fmt.Errorf("stream error: %v", stream.Err())
	}
	return result, nil
}
