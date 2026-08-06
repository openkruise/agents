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

// runtimeGRPCHTTPClient is the package-level HTTP client used by the connect (gRPC)
// callers whose result does not ride in HTTP trailers — today the Filesystem service
// in filesystem.go. It deliberately does not use http.DefaultClient so tests can
// substitute their own transport without monkey-patching the global, mirroring
// runtimeFilesHTTPClient which backs the multipart /files path.
//
// The Process/Start RPC deliberately does NOT use it: its outcome arrives in the
// grpc-status trailer, i.e. the very last bytes of the response, so it gets
// runtimeCommandHTTPClient with keep-alives disabled instead (see
// command_transport.go).
//
// Intentionally no http.Client.Timeout is set: every caller wraps the context
// with context.WithTimeout, which stays the single source of truth for the RPC
// deadline (mirrors runtimeFilesHTTPClient).
var runtimeGRPCHTTPClient = &http.Client{}

type RunCommandResult struct {
	PID      uint32
	Stdout   []string
	Stderr   []string
	ExitCode int32
	Exited   bool
	Error    error
	// EndReceived reports whether the runtime delivered a Process End event, i.e. whether
	// ExitCode and Exited describe an actually observed process termination rather than
	// their zero values. Callers must consult it before trusting ExitCode == 0 on a call
	// that also returned an error.
	EndReceived bool
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
	// HTTPClient overrides the client used for the Process/Start RPC. It exists for tests
	// and for ad-hoc transport experiments (for example forcing cleartext HTTP/2 while
	// diagnosing trailer delivery); production callers leave it nil and get
	// runtimeCommandHTTPClient.
	HTTPClient *http.Client
}

func RunCommandWithRuntime(ctx context.Context, args RunCmdFuncArgs) (RunCommandResult, error) {
	sbx, processConfig, timeout := args.Sbx, args.ProcessConfig, args.Timeout
	baseLog := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	log := baseLog.V(utils.DebugLogLevel)
	url := GetRuntimeURL(sbx)
	if url == "" {
		return RunCommandResult{}, fmt.Errorf("runtime url not found on sandbox")
	}
	httpClient := args.HTTPClient
	if httpClient == nil {
		httpClient = runtimeCommandHTTPClient
	}
	client := processconnect.NewProcessClient(
		httpClient,
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
			result.EndReceived = true
			result.ExitCode = evt.End.ExitCode
			result.Exited = evt.End.Exited
			if evt.End.Error != nil {
				result.Error = fmt.Errorf("process error: %s", *evt.End.Error)
			}

		default: // ProcessEvent_Keepalive
			continue
		}
	}
	streamErr := stream.Err()
	log.Info("all messages are received", "cost", time.Since(start), "result", result)
	// A missing grpc-status trailer only means the terminal status metadata was lost; the
	// End event already told us how the process finished, so the command result stands and
	// the caller must not be forced to redo work that demonstrably ran. Log it at default
	// verbosity: it still points at a transport or runtime defect worth chasing.
	if streamErr != nil && result.EndReceived && IsMissingGRPCStatusTrailer(streamErr) {
		baseLog.Info("tolerating missing grpc-status trailer because the process end event was received",
			"cmd", processConfig.GetCmd(), "pid", result.PID, "exitCode", result.ExitCode,
			"exited", result.Exited, "cost", time.Since(start),
			"responseTrailer", stream.ResponseTrailer(), "protocolError", streamErr.Error())
		streamErr = nil
	}
	return result, errors.Join(result.Error, streamErr)
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
