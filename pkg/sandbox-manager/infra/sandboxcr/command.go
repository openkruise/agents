package sandboxcr

import (
	"context"
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
}

// runCommandWithEnvd is a temporary solution to run command inside the sandbox,
// which will be replaced by `sandbox-runtime` in the future
func (s *Sandbox) runCommandWithEnvd(ctx context.Context, processConfig *process.ProcessConfig, timeout time.Duration) (RunCommandResult, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(s.Sandbox)).V(consts.DebugLogLevel)
	url := s.GetAnnotations()[v1alpha1.AnnotationEnvdURL]
	if url == "" {
		return RunCommandResult{}, fmt.Errorf("envd url not found on sandbox")
	}
	client := processconnect.NewProcessClient(
		http.DefaultClient,
		url,
		connect.WithGRPC(),
	)

	clientContext, callInfo := connect.NewClientContext(ctx)
	callInfo.RequestHeader().Set("X-Access-Token", s.Annotations[v1alpha1.AnnotationEnvdAccessToken])

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
		err := stream.Close()
		if err != nil {
			log.Error(err, "failed to close stream")
		} else {
			log.Info("stream closed")
		}
	}()

	ch := make(chan *process.ProcessEvent)
	done := make(chan struct{})
	go func() {
		log.Info("receiving messages")
		for stream.Receive() {
			response := stream.Msg()
			ch <- response.Event
		}
		done <- struct{}{}
		log.Info("receiving stopped")
	}()

	var result RunCommandResult
	defer log.Info("run command result", "result", result)
	timer := time.NewTimer(timeout)

Processor:
	for {
		select {
		case event := <-ch:
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
					return result, fmt.Errorf("process error: %s", *evt.End.Error)
				}

			case *process.ProcessEvent_Keepalive:
				continue
			}
		case <-timer.C:
			return result, fmt.Errorf("execution timeout")
		case <-done:
			break Processor
		}
	}

	if stream.Err() != nil {
		return result, fmt.Errorf("stream error: %v", stream.Err())
	}
	return result, nil
}
