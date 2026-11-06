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

// NewLifecycleHookFunc builds the lifecycle hook path used by sandbox upgrades.
//
// Upgrade hooks always run over the plaintext runtime URL, even for sandboxes
// that advertise the runtime TLS port (AnnotationRuntimeTLSPort): they execute
// commands through the envd process API, which is served on the plaintext
// runtime port. The TLS bundle is accepted so call sites stay stable and is not
// used until the process API is served over TLS; routing hooks over TLS then
// only means passing TransportOptionsFor's options to executeLifecycleHook.
func NewLifecycleHookFunc(_ *agentsruntime.TLSBundle) LifecycleHookFunc {
	return func(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.LifecycleAction) (exitCode int32, stdout, stderr string, err error) {
		return executeLifecycleHook(ctx, box, action, nil, "root")
	}
}

// NewPauseResumeLifecycleHookFunc returns the hook executor used by PrePause
// and PostResume. Like upgrade hooks it runs as root — older envd builds have
// no server-side default-user resolution and reject credential-less requests —
// and over the plaintext runtime URL, because the envd process API is served
// on the plaintext runtime port. Deployments whose envd resolves the sandbox's
// default user select the identity through NewPauseResumeLifecycleHookFuncWithUser.
func NewPauseResumeLifecycleHookFunc(tls *agentsruntime.TLSBundle) LifecycleHookFunc {
	return NewPauseResumeLifecycleHookFuncWithUser(tls, "root")
}

// NewPauseResumeLifecycleHookFuncWithUser returns the hook executor used by
// PrePause and PostResume, running as the given authUser: a non-empty user is
// sent as the Basic Authorization identity on every wire call, while an empty
// user sends no credential and lets envd resolve the sandbox's default user.
func NewPauseResumeLifecycleHookFuncWithUser(_ *agentsruntime.TLSBundle, authUser string) LifecycleHookFunc {
	return func(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.LifecycleAction) (exitCode int32, stdout, stderr string, err error) {
		return executeLifecycleHook(ctx, box, action, nil, authUser)
	}
}

// executeLifecycleHook executes a lifecycle action inside the sandbox pod via
// envd over the transport selected by rtOpts: non-empty options reach the
// runtime over HTTPS at the Pod IP, empty options use the plaintext runtime
// URL. It does not read the sandbox capability stamp; the caller chooses the
// transport, and the hook constructors pass none. authUser is the identity the
// command runs as on the wire.
func executeLifecycleHook(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.LifecycleAction,
	rtOpts []agentsruntime.Option, authUser string) (exitCode int32, stdout, stderr string, err error) {
	if action == nil || action.Exec == nil || len(action.Exec.Command) == 0 {
		return 0, "", "", nil
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
