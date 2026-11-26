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

package utils

import (
	"context"
	"os"
	"strconv"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

const (
	// RuntimePort is the well-known port for the agent-runtime sidecar.
	// Previously defined in pkg/sandbox-manager/consts; moved here to break the
	// utils → business-layer dependency.
	RuntimePort = 49983
)

const (

	// PodAnnotationCreatedBy is used to identify Pod source: created by Sandbox controller or externally created (bypassing Sandbox syntax sugar)
	PodAnnotationCreatedBy = "agents.kruise.io/created-by"
	// PodLabelCreatedBy is a label mirroring PodAnnotationCreatedBy, used as a label selector
	// for informer filtering so that only agent-related pods are cached.
	PodLabelCreatedBy = "agents.kruise.io/created-by"

	// default sandbox deploy namespace
	DefaultSandboxDeployNamespace = "sandbox-system"

	// MaxConditionMessageLen was moved to a var block below for env-based configuration.
)

const (
	CreatedByExternal = "external"
	CreatedBySandbox  = "sandbox"
	CreatedByCommit   = "commit"
)

// Deferred-start wrapper contract (spec.startupMode=Deferred). The wrapper holds
// the sandbox main process until the claim-time startup files and readiness
// signal are delivered, then exec's the claim or template command.
const (
	// RunWithEnvsScript is the PID-1 wrapper script path inside the sandbox,
	// staged into the per-pod envd volume by the agent-runtime image.
	RunWithEnvsScript = "/mnt/envd/run_with_envs.sh"

	// SandboxEnvClaimDir is the claim directory inside the envd volume that holds
	// env.sh, cmd.sh and the .env_ready readiness signal.
	SandboxEnvClaimDir = "/mnt/envd/claim"

	// EnvSandboxEnvDir is the wrapper environment variable carrying the claim
	// directory.
	EnvSandboxEnvDir = "SANDBOX_ENV_DIR"

	// EnvSandboxPodUID is the wrapper environment variable carrying the pod UID
	// (downward API, metadata.uid) used for the readiness-signal generation check.
	EnvSandboxPodUID = "SANDBOX_POD_UID"
)

var (
	// MaxConditionMessageLen is the max length for a Condition.Message.
	// Configurable via MAX_CONDITION_MESSAGE_LEN env var, defaults to 1024.
	MaxConditionMessageLen = getEnvIntOrDefault("MAX_CONDITION_MESSAGE_LEN", 1024)

	CacheBackoff = wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   2.0,
		Steps:    10,
		Jitter:   1.1,
	}
)

func RetryIfContextNotCanceled(ctx context.Context) func(err error) bool {
	return func(err error) bool {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
}

func getEnvIntOrDefault(key string, defaultVal int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil && i > 0 {
			return i
		}
	}
	return defaultVal
}
