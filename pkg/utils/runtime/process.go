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

// runtimeGRPCHTTPClient is the package-level HTTP client shared by the connect
// (gRPC) callers of the runtime: the Process service here and the Filesystem
// service in filesystem.go. It deliberately does not use http.DefaultClient so
// tests can substitute their own transport without monkey-patching the global,
// mirroring runtimeFilesHTTPClient which backs the multipart /files path.
//
// The two capabilities speak the same connect-over-gRPC protocol against the
// same runtime endpoint, so they must not diverge on transport. Keeping a
// single variable makes any future transport tuning (for example the pending
// migration to the pinned TLS transport) apply to both at once.
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

// ProcessAPI is the runtime process capability group, backed by the envd Process
// service. Like every other group it is addressed through the transport
// resolved by the owning runtimeClient, so a TLS-capable sandbox is reached over
// HTTPS with forced resolution while a legacy sandbox keeps the plaintext
// runtime URL. The group is bound to one sandbox at construction time, so no
// method takes a *Sandbox argument.
type ProcessAPI interface {
	// Run starts a process in the sandbox and consumes its event stream until the
	// process exits, returning the collected output and exit status.
	Run(ctx context.Context, req RunCommandRequest) (RunCommandResult, error)
	// Chmod applies mode to filePath inside the sandbox by running chmod as root.
	Chmod(ctx context.Context, filePath, mode string) error
}

// RunCommandRequest describes a single process execution. It is RunCmdFuncArgs
// without the sandbox, which the capability group already carries.
type RunCommandRequest struct {
	// ProcessConfig is the command, arguments and environment to execute.
	ProcessConfig *process.ProcessConfig
	// Timeout bounds the whole execution, including consuming the event stream.
	Timeout time.Duration
	// AuthUser is the user identity sent as an HTTP Basic Authorization header
	// (empty password). Empty means no Authorization header is sent.
	AuthUser string
}

// processAPI is the default ProcessAPI implementation. It delegates transport to
// the owning runtimeClient and carries no domain logic of its own.
type processAPI struct {
	r *runtimeClient
}

// RunCommandWithRuntime starts a process in the sandbox runtime and waits for it
// to exit.
//
// rtOpts selects the transport for this sandbox: non-empty (typically the TLS
// options resolved by TransportOptionsFor) routes the call over HTTPS to the
// agent-runtime, empty keeps the legacy plaintext runtime URL. The call is
// delegated to ProcessAPI.Run, which owns the implementation.
func RunCommandWithRuntime(ctx context.Context, args RunCmdFuncArgs, rtOpts ...Option) (RunCommandResult, error) {
	// Keep the nil-sandbox guard here: NewRuntime binds the sandbox at
	// construction time, so a nil one would otherwise surface later as an
	// unresolved endpoint instead of naming the actual programming error.
	if args.Sbx == nil {
		return RunCommandResult{}, fmt.Errorf("sandbox is nil")
	}
	return NewRuntime(args.Sbx, rtOpts...).Process().Run(ctx, RunCommandRequest{
		ProcessConfig: args.ProcessConfig,
		Timeout:       args.Timeout,
		AuthUser:      args.AuthUser,
	})
}

// Run implements ProcessAPI. It is the single implementation of the process
// execution path, shared by the capability group and the RunCommandWithRuntime
// convenience wrapper.
func (p *processAPI) Run(ctx context.Context, req RunCommandRequest) (RunCommandResult, error) {
	sbx, processConfig, timeout := p.r.sbx, req.ProcessConfig, req.Timeout
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(utils.DebugLogLevel)
	// The process RPC shares the transport decision with every other capability
	// group; runtimeGRPCHTTPClient stays the plaintext client so the existing
	// test seam keeps working.
	base, httpClient, err := p.r.resolveTransport(sbx, runtimeGRPCHTTPClient)
	if err != nil {
		return RunCommandResult{}, err
	}
	client := processconnect.NewProcessClient(
		httpClient,
		base,
		connect.WithGRPC(),
	)

	ctxWithTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	clientContext, callInfo := connect.NewClientContext(ctxWithTimeout)
	callInfo.RequestHeader().Set(accessTokenHeader, utils.GetAccessToken(sbx))
	// The Basic user credential is caller-supplied: it is sent only when
	// req.AuthUser expresses a user identity for the runtime to resolve.
	if req.AuthUser != "" {
		callInfo.RequestHeader().Set("Authorization", basicAuthHeader(req.AuthUser))
	}

	startReq := connect.NewRequest(&process.StartRequest{
		Process: processConfig,
		Tag:     nil,
		Pty:     nil,
		Stdin:   nil,
	})
	stream, err := client.Start(clientContext, startReq)
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
// via the process capability group. This is a temporary measure to enforce file
// permissions until the agent-runtime (envd) natively honors the X-File-Mode header.
//
// rtOpts selects the transport for this sandbox exactly as in
// RunCommandWithRuntime. The call is delegated to ProcessAPI.Chmod.
func ChmodFileOnRuntime(ctx context.Context, sbx *agentsv1alpha1.Sandbox, filePath, mode string, rtOpts ...Option) error {
	if sbx == nil {
		return fmt.Errorf("sandbox is nil")
	}
	return NewRuntime(sbx, rtOpts...).Process().Chmod(ctx, filePath, mode)
}

// Chmod implements ProcessAPI. It is the single implementation of the chmod
// path, shared by the capability group and the ChmodFileOnRuntime convenience
// wrapper.
func (p *processAPI) Chmod(ctx context.Context, filePath, mode string) error {
	result, err := p.Run(ctx, RunCommandRequest{
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
