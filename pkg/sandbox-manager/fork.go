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
	"fmt"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	v1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/pausedretention"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

const (
	forkCloneWorkers            = 10
	maxForkSandboxes            = 100
	forkCheckpointTTL           = "1h"
	forkChildPreparationTimeout = 5 * time.Minute
	forkSourceLookupTimeout     = 2 * time.Second
)

// ForkSandboxOptions defines a protocol-neutral running sandbox clone request.
type ForkSandboxOptions struct {
	SourceID                  string
	Namespace                 string
	User                      string
	Count                     int
	Quota                     *quotaspec.QuotaSpec
	TimeoutSeconds            int
	AutoPause                 bool
	PausedRetention           time.Duration
	PausedRetentionAnnotation string
}

// ForkSandboxResult is one child creation attempt from a shared checkpoint.
type ForkSandboxResult struct {
	Sandbox infra.Sandbox
	Err     error
}

func forkTimeoutOptions(now time.Time, opts ForkSandboxOptions) timeout.Options {
	if opts.AutoPause {
		pauseTime := timeout.NormalizeTime(now.Add(time.Duration(opts.TimeoutSeconds) * time.Second))
		return timeout.Options{
			PauseTime:    pauseTime,
			ShutdownTime: pausedretention.PausedShutdownTime(pauseTime, opts.PausedRetention),
		}
	}
	return timeout.Options{ShutdownTime: now.Add(time.Duration(opts.TimeoutSeconds) * time.Second)}
}

func forkPreparationTimeout(now time.Time) timeout.Options {
	return timeout.Options{ShutdownTime: now.Add(forkChildPreparationTimeout)}
}

func classifyForkSourceError(err error, sourceID, operation string) error {
	if err == nil || managererrors.GetErrCode(err) != managererrors.ErrorUnknown {
		return err
	}
	if apierrors.IsNotFound(err) {
		return managererrors.WrapError(managererrors.ErrorNotFound, err, "source sandbox %s disappeared while %s", sourceID, operation)
	}
	if apierrors.IsServerTimeout(err) || apierrors.IsTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) {
		return managererrors.WrapError(managererrors.ErrorUnavailable, err, "source sandbox %s unavailable while %s", sourceID, operation)
	}
	return managererrors.WrapError(managererrors.ErrorInternal, err, "source sandbox %s failed while %s", sourceID, operation)
}

// ForkSandbox checkpoints Source once, then creates Count independent children
// from that checkpoint. A child failure does not stop the remaining attempts.
func (m *SandboxManager) ForkSandbox(ctx context.Context, opts ForkSandboxOptions) ([]ForkSandboxResult, error) {
	if opts.SourceID == "" {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "source sandbox ID is required")
	}
	if opts.Namespace == "" {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "namespace is required")
	}
	if opts.User == "" {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "user is required")
	}
	if opts.Count < 1 || opts.Count > maxForkSandboxes {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "fork count must be between 1 and %d", maxForkSandboxes)
	}

	lookupCtx, cancelLookup := context.WithTimeout(ctx, forkSourceLookupTimeout)
	defer cancelLookup()
	source, err := m.GetSandbox(lookupCtx, opts.User, nil, infra.GetSandboxOptions{
		Namespace: opts.Namespace,
		SandboxID: opts.SourceID,
	})
	if err != nil {
		return nil, err
	}
	sourceUID := string(source.GetUID())
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), consts.DefaultWaitCheckpointTimeout)
	defer cancel()
	lease, err := m.acquireForkLease(checkpointCtx, source)
	if err != nil {
		return nil, err
	}
	var checkpointID string
	var networkPolicy *infra.SandboxNetworkConfig
	checkpointErr := m.withLease(checkpointCtx, lease, func(ctx context.Context) error {
		if err := source.RefreshForExclusiveOperation(ctx); err != nil {
			return classifyForkSourceError(err, opts.SourceID, "refreshing before fork")
		}
		if string(source.GetUID()) != sourceUID {
			return managererrors.NewError(managererrors.ErrorNotFound, "source sandbox %s disappeared while preparing fork", opts.SourceID)
		}
		if source.GetAnnotations()[v1alpha1.AnnotationCleanup] == v1alpha1.True {
			return managererrors.NewError(managererrors.ErrorConflict, "sandbox %s is recycling", source.GetSandboxID())
		}
		state, reason := source.GetState()
		if state == v1alpha1.SandboxStateDead && reason != "RunningResourceClaimedButNotReady" {
			return managererrors.NewError(managererrors.ErrorNotFound, "source sandbox %s no longer exists", source.GetSandboxID())
		}
		if state != v1alpha1.SandboxStateRunning {
			return managererrors.NewError(managererrors.ErrorConflict, "sandbox %s cannot be forked while in %s state: %s", source.GetSandboxID(), state, reason)
		}
		sourceNetworkPolicy, err := source.SelectNetworkPolicy(ctx)
		if err != nil {
			return classifyForkSourceError(err, opts.SourceID, "reading network policy")
		}
		if sourceNetworkPolicy != nil {
			networkPolicy = &infra.SandboxNetworkConfig{
				AllowOut: append([]string(nil), sourceNetworkPolicy.AllowOut...),
				DenyOut:  append([]string(nil), sourceNetworkPolicy.DenyOut...),
			}
		}
		checkpointID, err = source.CreateCheckpoint(ctx, infra.CreateCheckpointOptions{
			KeepRunning: ptr.To(true),
			TTL:         ptr.To(forkCheckpointTTL),
			Fork:        true,
		})
		return classifyForkSourceError(err, opts.SourceID, "creating checkpoint")
	})
	if checkpointErr != nil {
		return nil, classifyForkSourceError(checkpointErr, opts.SourceID, "creating checkpoint")
	}

	results := make([]ForkSandboxResult, opts.Count)
	if err := ctx.Err(); err != nil {
		for index := range results {
			results[index].Err = err
		}
		return results, nil
	}
	workers := min(opts.Count, forkCloneWorkers)
	jobs := make(chan int)
	var workersWG sync.WaitGroup
	workersWG.Add(workers)
	for range workers {
		go func() {
			defer workersWG.Done()
			for index := range jobs {
				results[index] = m.cloneForkChild(ctx, opts, checkpointID, networkPolicy)
			}
		}()
	}

	for index := range opts.Count {
		select {
		case jobs <- index:
		case <-ctx.Done():
			for remaining := index; remaining < opts.Count; remaining++ {
				results[remaining].Err = ctx.Err()
			}
			close(jobs)
			workersWG.Wait()
			return results, nil
		}
	}
	close(jobs)
	workersWG.Wait()
	for index := range results {
		if results[index].Err != nil {
			return results, nil
		}
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), consts.DefaultWaitCheckpointTimeout)
	defer cleanupCancel()
	if err := m.infra.DeleteForkCheckpoint(cleanupCtx, opts.Namespace, sourceUID, checkpointID); err != nil {
		klog.FromContext(ctx).Error(err, "failed to delete completed fork checkpoint", "checkpointID", checkpointID)
	}
	return results, nil
}

func (m *SandboxManager) cloneForkChild(ctx context.Context, opts ForkSandboxOptions, checkpointID string, networkPolicy *infra.SandboxNetworkConfig) ForkSandboxResult {
	var autoPausePolicy *v1alpha1.AutoPausePolicy
	child, cloneErr := m.CloneSandbox(ctx, CloneSandboxOptions{
		Infra: infra.CloneSandboxOptions{
			Namespace:    opts.Namespace,
			User:         opts.User,
			CheckPointID: checkpointID,
			Modifier: func(sbx infra.Sandbox) error {
				if opts.AutoPause && opts.PausedRetentionAnnotation != "" {
					annotations := sbx.GetAnnotations()
					if annotations == nil {
						annotations = map[string]string{}
					}
					annotations[v1alpha1.AnnotationReservePausedSandboxDuration] = opts.PausedRetentionAnnotation
					sbx.SetAnnotations(annotations)
				}
				autoPausePolicy = sbx.GetAutoPausePolicy()
				sbx.SetAutoPausePolicy(nil)
				sbx.SetTimeout(forkPreparationTimeout(time.Now()))
				return nil
			},
			ReserveFailedSandboxFor:  ptr.To(consts.ReserveFailedSandboxNever),
			RotateRuntimeAccessToken: true,
			NetworkPolicy:            networkPolicy,
			AllowForkCheckpoint:      true,
		},
		Quota: opts.Quota,
	})
	if cloneErr == nil {
		cloneErr = m.saveForkChildTimeout(ctx, child, opts, autoPausePolicy)
		if cloneErr != nil {
			child = nil
		}
	}
	return ForkSandboxResult{Sandbox: child, Err: cloneErr}
}

func (m *SandboxManager) saveForkChildTimeout(ctx context.Context, child infra.Sandbox, opts ForkSandboxOptions, autoPausePolicy *v1alpha1.AutoPausePolicy) error {
	finalTimeoutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), consts.DefaultWaitCheckpointTimeout)
	_, timeoutErr := child.SaveTimeoutWithPolicy(finalTimeoutCtx, infra.SaveTimeoutOptions{
		Timeout:            forkTimeoutOptions(time.Now(), opts),
		AutoPausePolicy:    autoPausePolicy,
		SetAutoPausePolicy: true,
	}, timeout.UpdatePolicyAlways)
	cancel()
	if timeoutErr == nil {
		return nil
	}

	err := fmt.Errorf("save final fork timeout: %w", timeoutErr)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), consts.DefaultWaitCheckpointTimeout)
	defer cleanupCancel()
	if killErr := child.Kill(cleanupCtx); killErr != nil && !apierrors.IsNotFound(killErr) {
		klog.FromContext(cleanupCtx).Error(killErr, "failed to delete fork child after timeout update failure", "sandbox", klog.KObj(child))
		return err
	}
	m.deleteRouteAndSync(cleanupCtx, child)
	m.releaseQuotaAfterDelete(cleanupCtx, DeleteSandboxOptions{
		Sandbox: child,
		User:    opts.User,
		Quota:   opts.Quota,
	})
	return err
}
