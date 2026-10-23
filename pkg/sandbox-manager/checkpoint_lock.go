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

package sandbox_manager

import (
	"context"
	"time"

	"k8s.io/klog/v2"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

const (
	CheckpointOperationFork     = "fork"
	CheckpointOperationSnapshot = "snapshot"

	checkpointLockCleanupAllowance = 30 * time.Second
	checkpointLockMargin           = 30 * time.Second
	checkpointLockReleaseTimeout   = 10 * time.Second
)

// CreateCheckpointOptions configures a checkpoint created under the Sandbox
// checkpoint lock.
type CreateCheckpointOptions struct {
	// Operation names the requester; it is written into the lock and reported
	// in conflict errors.
	Operation string
	Infra     infra.CreateCheckpointOptions
	// BeforeCheckpoint optionally runs inside the lock on the Sandbox returned
	// by acquisition; an error aborts before any checkpoint is created.
	BeforeCheckpoint func(ctx context.Context, sbx infra.Sandbox) error
}

// checkpointLockTTL covers the checkpoint wait, abandoned-checkpoint cleanup,
// and release latency plus clock skew between sandbox-manager replicas.
func checkpointLockTTL(waitTimeout time.Duration) time.Duration {
	if waitTimeout <= 0 {
		waitTimeout = consts.DefaultWaitCheckpointTimeout
	}
	return waitTimeout + checkpointLockCleanupAllowance + checkpointLockMargin
}

// CreateCheckpoint is the only sandbox-manager entry point that creates a
// checkpoint. Checkpoints of one Sandbox are serialized by its checkpoint lock;
// a concurrent checkpoint fails with a conflict.
func (m *SandboxManager) CreateCheckpoint(ctx context.Context, sbx infra.Sandbox, opts CreateCheckpointOptions) (string, error) {
	ttl := checkpointLockTTL(opts.Infra.WaitSuccessTimeout)
	lock, err := sbx.AcquireCheckpointLock(ctx, infra.CheckpointLockOptions{Operation: opts.Operation, TTL: ttl})
	if err != nil {
		return "", err
	}
	defer releaseCheckpointLock(ctx, sbx, lock)

	// Bounding protected work by the wait timeout leaves the cleanup allowance
	// and the margin for detached cleanup and release, keeping both within the TTL.
	workCtx, cancel := context.WithTimeout(ctx, ttl-checkpointLockCleanupAllowance-checkpointLockMargin)
	defer cancel()
	if opts.BeforeCheckpoint != nil {
		if err := opts.BeforeCheckpoint(workCtx, sbx); err != nil {
			return "", err
		}
	}
	return sbx.CreateCheckpoint(workCtx, opts.Infra)
}

// releaseCheckpointLock runs on a detached context so a canceled request still
// frees the lock; a failed release leaves the lock to expire.
func releaseCheckpointLock(ctx context.Context, sbx infra.Sandbox, lock v1alpha1.CheckpointLock) {
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), checkpointLockReleaseTimeout)
	defer cancel()
	if err := sbx.ReleaseCheckpointLock(releaseCtx, lock); err != nil {
		klog.FromContext(ctx).Error(err, "failed to release checkpoint lock; it remains until its expire time",
			"sandbox", klog.KObj(sbx), "operation", lock.Operation, "expireTime", lock.ExpireTime)
	}
}
