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
	// SandboxFinalizer is sandbox finalizer
	SandboxFinalizer = "agents.kruise.io/sandbox"
	// PodAnnotationCreatedBy is used to identify Pod source: created by Sandbox controller or externally created (bypassing Sandbox syntax sugar)
	PodAnnotationCreatedBy = "agents.kruise.io/created-by"
	// PodLabelCreatedBy is a label mirroring PodAnnotationCreatedBy, used as a label selector
	// for informer filtering so that only agent-related pods are cached.
	PodLabelCreatedBy = "agents.kruise.io/created-by"

	// default sandbox deploy namespace
	DefaultSandboxDeployNamespace = "sandbox-system"

	PodConditionContainersPaused  = "ContainersPaused"
	PodConditionContainersResumed = "ContainersResumed"

	// MaxConditionMessageLen was moved to a var block below for env-based configuration.
)

const (
	True  = "true"
	False = "false"

	CreatedByExternal = "external"
	CreatedBySandbox  = "sandbox"
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
