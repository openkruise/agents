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

package cache

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
)

// Provider is a read-through, informer-backed local cache for all Kubernetes resources
// used by the sandbox manager. It provides low-latency lookups without hitting the API server
// on every request, and exposes async-wait primitives so callers can block until a resource
// reaches a desired state (e.g., sandbox becomes Running after Resume).
//
// All Get/List methods read exclusively from the in-process informer store; they never issue
// live API calls. The underlying manager cache must be synced before any other method is used.
// Call Run for a Cache with an owned manager. When a Cache reuses another operator's manager,
// do not call Run; the owner manager starts and syncs the cache during manager.Start.
type Provider interface {
	GetClaimedSandbox(ctx context.Context, opts GetClaimedSandboxOptions) (*agentsv1alpha1.Sandbox, error)

	GetCheckpoint(ctx context.Context, opts GetCheckpointOptions) (*agentsv1alpha1.Checkpoint, error)

	PickSandboxSet(ctx context.Context, opts PickSandboxSetOptions) (*agentsv1alpha1.SandboxSet, error)

	ListSandboxSets(ctx context.Context, opts ListSandboxSetsOptions) ([]*agentsv1alpha1.SandboxSet, error)

	// ListSandboxes returns Sandbox CRD objects filtered by namespace and optional owner.
	// Ownership is determined by the AnnotationOwner annotation on the Sandbox resource when User is set.
	ListSandboxes(ctx context.Context, opts ListSandboxesOptions) ([]*agentsv1alpha1.Sandbox, error)

	// ListCheckpoints returns Checkpoint CRD objects filtered by namespace and optional owner.
	// Ownership is determined by the AnnotationOwner annotation on the Checkpoint resource when User is set.
	ListCheckpoints(ctx context.Context, opts ListCheckpointsOptions) ([]*agentsv1alpha1.Checkpoint, error)

	ListSandboxesInPool(ctx context.Context, opts ListSandboxesInPoolOptions) ([]*agentsv1alpha1.Sandbox, error)

	// NewSandboxPauseTask builds an immutable wait task encapsulating the Pause
	// readiness check. See pkg/cache/tasks.go for the checker definition.
	NewSandboxPauseTask(ctx context.Context, sbx *agentsv1alpha1.Sandbox) *cacheutils.WaitTask[*agentsv1alpha1.Sandbox]

	// NewSandboxResumeTask builds an immutable wait task for Resume. The task
	// succeeds when the sandbox reaches SandboxStateRunning.
	NewSandboxResumeTask(ctx context.Context, sbx *agentsv1alpha1.Sandbox) *cacheutils.WaitTask[*agentsv1alpha1.Sandbox]

	// NewSandboxWaitReadyTask builds an immutable wait task for post-claim
	// readiness (Generation observed + Ready condition not StartContainerFailed
	// + not InplaceUpdating + Running + PodIP set).
	NewSandboxWaitReadyTask(ctx context.Context, sbx *agentsv1alpha1.Sandbox) *cacheutils.WaitTask[*agentsv1alpha1.Sandbox]

	// NewCheckpointTask builds an immutable wait task for a Checkpoint that
	// succeeds when Status.Phase == CheckpointSucceeded (with non-empty
	// CheckpointId); fails on Terminating/Failed.
	NewCheckpointTask(ctx context.Context, cp *agentsv1alpha1.Checkpoint) *cacheutils.WaitTask[*agentsv1alpha1.Checkpoint]

	// Run starts an owned manager and waits for cache sync.
	// Do not call Run for a cache backed by an externally owned manager.
	Run(ctx context.Context) error

	// Stop shuts down all underlying informers and releases associated resources.
	// After Stop returns, no further cache lookups or wait operations should be performed.
	Stop(ctx context.Context)

	// GetClient returns the underlying controller-runtime client.Client for direct
	// Kubernetes API operations. This should be used sparingly, preferring the
	// cache-backed methods above for read operations to avoid API server load.
	// The returned client is the same instance used internally by the Provider.
	GetClient() client.Client

	// GetAPIReader returns a client.Reader that bypasses the cache and reads
	// directly from the API server. Use this when you need the latest state
	// and can tolerate the additional latency of an API server round-trip.
	// Prefer GetClient() for most operations; use this only when cache staleness
	// is unacceptable (e.g., validating critical state transitions).
	GetAPIReader() client.Reader
}

type GetClaimedSandboxOptions struct {
	Namespace string
	SandboxID string
}

type GetCheckpointOptions struct {
	Namespace    string
	CheckpointID string
}

type PickSandboxSetOptions struct {
	Namespace string
	Name      string
}

type ListSandboxSetsOptions struct {
	Namespace string
}

type ListSandboxesOptions struct {
	Namespace string
	User      string
}

type ListCheckpointsOptions struct {
	Namespace string
	User      string
}

type ListSandboxesInPoolOptions struct {
	Namespace string
	Pool      string
}
