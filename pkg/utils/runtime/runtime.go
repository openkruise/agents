/*
Copyright 2026.
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
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

var AccessToken = "access-token"

func GetRuntimeURL(sbx *agentsv1alpha1.Sandbox) string {
	// firstly, get runtime url from the annotation
	url := sbx.GetAnnotations()[agentsv1alpha1.AnnotationRuntimeURL]
	if url == "" {
		url = sbx.GetAnnotations()[agentsv1alpha1.AnnotationEnvdURL] // legacy
	}
	if url != "" {
		return url
	}
	// secondly, calculate runtime url from the route
	route := sandboxutils.GetRouteFromSandbox(sbx)
	if route.IP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:%d", route.IP, consts.RuntimePort)
}

func GetAccessToken(sbx metav1.Object) string {
	token := sbx.GetAnnotations()[agentsv1alpha1.AnnotationRuntimeAccessToken]
	if token == "" {
		token = sbx.GetAnnotations()[agentsv1alpha1.AnnotationEnvdAccessToken] // legacy
	}
	return token
}

type RunCommandResult struct {
	PID      uint32
	Stdout   []string
	Stderr   []string
	ExitCode int32
	Exited   bool
	Error    error
}

type RunCmdFuncArgs struct {
	Sbx           *agentsv1alpha1.Sandbox
	ProcessConfig *process.ProcessConfig
	Timeout       time.Duration
}

// sidecar runtime 提供的 run command 能力
func RunCommandWithRuntime(ctx context.Context, args RunCmdFuncArgs) (RunCommandResult, error) {
	sbx, processConfig, timeout := args.Sbx, args.ProcessConfig, args.Timeout
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(consts.DebugLogLevel)
	url := GetRuntimeURL(sbx)
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
	callInfo.RequestHeader().Set("X-Access-Token", GetAccessToken(sbx))
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
