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

// process.go groups the native envd process-protocol capability of the runtime:
// starting a process over the connect/gRPC Process service and small helpers
// built on top of it. These map to the upstream envd protocol rather than to a
// KruiseAgents-specific extended HTTP route.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/proto/envd/process"
	"github.com/openkruise/agents/proto/envd/process/processconnect"
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

type RunCmdFuncArgs struct {
	Sbx           *agentsv1alpha1.Sandbox
	ProcessConfig *process.ProcessConfig
	Timeout       time.Duration
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password). Empty means no Authorization header is sent; callers
	// that need the runtime to resolve a user identity (e.g. "root") must set
	// it explicitly.
	AuthUser string
}

func RunCommandWithRuntime(ctx context.Context, args RunCmdFuncArgs) (RunCommandResult, error) {
	sbx, processConfig, timeout := args.Sbx, args.ProcessConfig, args.Timeout
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(utils.DebugLogLevel)
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
	callInfo.RequestHeader().Set("X-Access-Token", utils.GetAccessToken(sbx))
	// The Basic user credential is caller-supplied: it is sent only when
	// args.AuthUser expresses a user identity for the runtime to resolve.
	if args.AuthUser != "" {
		callInfo.RequestHeader().Set("Authorization", basicAuthHeader(args.AuthUser))
	}

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

// ChmodFileOnRuntime executes `chmod <mode> <filePath>` inside the sandbox runtime
// via RunCommandWithRuntime. This is a temporary measure to enforce file permissions
// until the agent-runtime (envd) natively honors the X-File-Mode header.
func ChmodFileOnRuntime(ctx context.Context, sbx *agentsv1alpha1.Sandbox, filePath, mode string) error {
	result, err := RunCommandWithRuntime(ctx, RunCmdFuncArgs{
		Sbx: sbx,
		ProcessConfig: &process.ProcessConfig{
			Cmd:  "chmod",
			Args: []string{mode, filePath},
		},
		Timeout: 5 * time.Second,
		// chmod must run as root to adjust files owned by arbitrary users.
		AuthUser: "root",
	})
	if err != nil {
		return fmt.Errorf("chmod command failed: %w", err)
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("chmod exited with code %d: stderr=%v", result.ExitCode, result.Stderr)
	}
	return nil
}
