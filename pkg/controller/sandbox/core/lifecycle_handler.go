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
type LifecycleHookFunc func(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.UpgradeAction) (exitCode int32, stdout, stderr string, err error)

// ExecuteLifecycleHook executes an upgrade action inside the sandbox pod via envd.
// It uses the shared RunCommandWithRuntime from pkg/utils/runtime.
func ExecuteLifecycleHook(ctx context.Context, box *agentsv1alpha1.Sandbox, action *agentsv1alpha1.UpgradeAction) (exitCode int32, stdout, stderr string, err error) {
	if action == nil || action.Exec == nil || len(action.Exec.Command) == 0 {
		return 0, "", "", nil
	}

	// Check runtime URL availability
	runtimeURL := agentsruntime.GetRuntimeURL(box)
	if runtimeURL == "" {
		return -1, "", "", fmt.Errorf("runtime URL not found on sandbox %s/%s", box.Namespace, box.Name)
	}

	// Determine timeout
	timeout := time.Duration(action.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second // default 60s
	}

	// Execute the command via shared runtime utility
	result, err := agentsruntime.RunCommandWithRuntime(ctx, agentsruntime.RunCmdFuncArgs{
		Sbx: box,
		ProcessConfig: &process.ProcessConfig{
			Cmd:  action.Exec.Command[0],
			Args: action.Exec.Command[1:],
		},
		Timeout: timeout,
	})
	if err != nil {
		return -1, "", "", err
	}

	return result.ExitCode, strings.Join(result.Stdout, ""), strings.Join(result.Stderr, ""), nil
}
