package sandboxcr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
	"k8s.io/klog/v2"
)

type RunCommandResult struct {
	PID      uint32
	Stdout   []string
	Stderr   []string
	ExitCode int32
	Exited   bool
	Error    error
}

func (s *Sandbox) GetRuntimeURL() string {
	url := s.GetAnnotations()[v1alpha1.AnnotationRuntimeURL]
	if url == "" {
		url = s.GetAnnotations()[v1alpha1.AnnotationEnvdURL] // legacy
	}
	return url
}

func (s *Sandbox) GetAccessToken() string {
	token := s.Annotations[v1alpha1.AnnotationRuntimeAccessToken]
	if token == "" {
		token = s.Annotations[v1alpha1.AnnotationEnvdAccessToken] // legacy
	}
	return token
}

// runCommandWithRuntime is a solution to run command inside the sandbox.
func (s *Sandbox) runCommandWithRuntime(ctx context.Context, processConfig *process.ProcessConfig, timeout time.Duration) (RunCommandResult, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox)).V(consts.DebugLogLevel)
	url := s.GetRuntimeURL()
	if url == "" {
		return RunCommandResult{}, fmt.Errorf("runtime url not found on sandbox")
	}
	client := processconnect.NewProcessClient(
		http.DefaultClient,
		url,
		connect.WithGRPC(),
	)

	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	clientContext, callInfo := connect.NewClientContext(ctxWithTimeout)
	callInfo.RequestHeader().Set("X-Access-Token", s.GetAccessToken())
	callInfo.RequestHeader().Set("Authorization", "Basic cm9vdDo=") // Basic root:

	req := connect.NewRequest(&process.StartRequest{
		Process: processConfig,
		Tag:     nil,
		Pty:     nil,
		Stdin:   nil,
	})
	stream, err := client.Start(clientContext, req)
	if err != nil {
		return RunCommandResult{}, err
	}
	defer func() {
		if err := stream.Close(); err != nil {
			log.Error(err, "failed to close stream")
		} else {
			log.Info("stream closed")
		}
	}()

	var result RunCommandResult
	start := time.Now()
	log.Info("receiving messages", "timeout", timeout)
	for stream.Receive() {
		event := stream.Msg().Event
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
			}

		case *process.ProcessEvent_End:
			result.ExitCode = evt.End.ExitCode
			result.Exited = evt.End.Exited
			if evt.End.Error != nil {
				result.Error = fmt.Errorf("process error: %s", *evt.End.Error)
			}

		default: // ProcessEvent_Keepalive
			continue
		}
	}
	log.Info("all messages are received", "cost", time.Since(start), "result", result)
	return result, errors.Join(result.Error, stream.Err())
}
