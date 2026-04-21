package utils

import (
	"context"
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

	// MaxConditionMessageLen is the max length for a Condition.Message.
	MaxConditionMessageLen = 256
)

const (
	True  = "true"
	False = "false"

	CreatedByExternal = "external"
	CreatedBySandbox  = "sandbox"
)

var (
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

const (
	E2BKeyStorageDSNEnvVar = "E2B_KEY_STORAGE_DSN"
	E2BKeyHashPepperEnvVar = "E2B_KEY_HASH_PEPPER"
)
