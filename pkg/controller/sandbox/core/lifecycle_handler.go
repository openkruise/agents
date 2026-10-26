/*
Copyright 2025.

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

package core

import (
	"context"
	"fmt"
	"strings"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	agentsruntime "github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/proto/envd/process"
)

// LifecycleHookFunc is the function type for executing lifecycle hooks.
type LifecycleHookFunc func(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.LifecycleAction) (exitCode int32, stdout, stderr string, err error)

// lifecycleHookMaxOutputBytes bounds the stdout/stderr bytes captured for one
// pause/resume hook execution. Captured output is only ever joined into the
// hook wrapper's return values; conditions and events carry a safe failure
// summary instead.
const lifecycleHookMaxOutputBytes = 1024

// NewLifecycleHookFunc returns the executor used by upgrade hooks. It preserves
// their root execution identity while sharing the server-side process deadline
// used by every lifecycle hook.
func NewLifecycleHookFunc(tlsBundle *agentsruntime.TLSBundle) LifecycleHookFunc {
	return func(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.LifecycleAction) (int32, string, string, error) {
		return executeLifecycleHook(ctx, box, action, tlsBundle, "root")
	}
}

// NewPauseResumeLifecycleHookFunc returns the hook executor used by PrePause
// and PostResume. Hooks run as root, matching upgrade hooks: older envd
// builds have no server-side default-user resolution and reject credential-less
// requests, so the identity is explicit on every wire call. Selecting the
// running user per hook is future work recorded in the proposal.
func NewPauseResumeLifecycleHookFunc(tlsBundle *agentsruntime.TLSBundle) LifecycleHookFunc {
	return func(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.LifecycleAction) (exitCode int32, stdout, stderr string, err error) {
		return executeLifecycleHook(ctx, box, action, tlsBundle, "root")
	}
}

// executeLifecycleHook executes a lifecycle action inside the sandbox pod via
// envd, resolving the runtime transport from the sandbox capability stamp.
func executeLifecycleHook(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.LifecycleAction,
	tlsBundle *agentsruntime.TLSBundle, authUser string) (exitCode int32, stdout, stderr string, err error) {
	if action == nil || action.Exec == nil || len(action.Exec.Command) == 0 {
		return 0, "", "", nil
	}

	rtOpts, err := agentsruntime.TransportOptionsFor(box, tlsBundle)
	if err != nil {
		return -1, "", "", err
	}
	// Readiness gates per transport: plaintext is addressed by the runtime URL
	// (with a Pod-IP fallback), while TLS dials the Pod IP directly.
	if len(rtOpts) == 0 && agentsruntime.GetRuntimeURL(box) == "" {
		return -1, "", "", fmt.Errorf("runtime URL not found on sandbox %s/%s", box.Namespace, box.Name)
	}
	if len(rtOpts) > 0 && box.Status.PodInfo.PodIP == "" {
		return -1, "", "", fmt.Errorf("pod IP not ready on sandbox %s/%s", box.Namespace, box.Name)
	}

	// Determine timeout
	timeout := time.Duration(action.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second // default 60s
	}

	processTimeout := timeout
	// Keep the client stream open for five seconds beyond envd's server-side
	// process deadline so it can deliver the terminal End event after timeout
	// cleanup. This grace period does not extend the command's allowed runtime.
	clientTimeout := timeout + 5*time.Second
	// Pause/resume hooks bound captured output and suppress the result log:
	// the manager keeps only a safe failure summary, and the proposal forbids
	// raw hook output from reaching controller logs or memory unbounded.
	outputOptions := agentsruntime.ProcessOutputOptions{
		MaxBytes:          lifecycleHookMaxOutputBytes,
		SuppressResultLog: true,
	}

	result, err := agentsruntime.RunCommandWithRuntime(ctx, agentsruntime.RunCmdFuncArgs{
		Sbx: box,
		ProcessConfig: &process.ProcessConfig{
			Cmd:  action.Exec.Command[0],
			Args: action.Exec.Command[1:],
		},
		Timeout:        clientTimeout,
		ProcessTimeout: processTimeout,
		OutputOptions:  outputOptions,
		AuthUser:       authUser,
	}, rtOpts...)
	if err != nil {
		return -1, strings.Join(result.Stdout, ""), strings.Join(result.Stderr, ""), err
	}

	return result.ExitCode, strings.Join(result.Stdout, ""), strings.Join(result.Stderr, ""), nil
}
