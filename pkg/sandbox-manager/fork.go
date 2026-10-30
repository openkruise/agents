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
	Timeout                   time.Duration
	AutoPause                 bool
	PausedRetention           time.Duration
	PausedRetentionAnnotation string
}

// ForkSandboxResult is one child creation attempt from a shared checkpoint.
type ForkSandboxResult struct {
	Sandbox infra.Sandbox
	Err     error
}

// forkTimeoutOptions derives the final child deadlines from the request: a
// plain fork expires at ShutdownTime, while an auto-pausing fork pauses at
// PauseTime and shuts down after the paused-retention window.
func forkTimeoutOptions(now time.Time, opts ForkSandboxOptions) timeout.Options {
	if opts.AutoPause {
		pauseTime := timeout.NormalizeTime(now.Add(opts.Timeout))
		return timeout.Options{
			PauseTime:    pauseTime,
			ShutdownTime: pausedretention.PausedShutdownTime(pauseTime, opts.PausedRetention),
		}
	}
	return timeout.Options{ShutdownTime: now.Add(opts.Timeout)}
}

// forkPreparationTimeout is the boot deadline every fork child is created
// with: if the child cannot become Ready within it, the normal failed-sandbox
// lifecycle reclaims it instead of the user's requested lifetime being
// consumed by a slow boot. The requested lifecycle replaces this deadline only
// after the child is Ready (see saveForkChildTimeout), so its clock starts at
// readiness.
func forkPreparationTimeout(now time.Time) timeout.Options {
	return timeout.Options{ShutdownTime: now.Add(forkChildPreparationTimeout)}
}

// classifyForkSourceError refines source lookup, lock, and checkpoint errors:
// an already-classified manager error passes through untouched, a source that
// vanished becomes ErrorNotFound, and everything else is wrapped as internal
// with the operation context. Which HTTP status each code maps to is the API
// layer's decision.
func classifyForkSourceError(err error, sourceID, operation string) error {
	if err == nil {
		return nil
	}
	code := managererrors.GetErrCode(err)
	if code != managererrors.ErrorUnknown && code != managererrors.ErrorInternal {
		return err
	}
	if code == managererrors.ErrorInternal {
		return err
	}
	if apierrors.IsNotFound(err) {
		return managererrors.WrapError(managererrors.ErrorNotFound, err, "source sandbox %s disappeared while %s", sourceID, operation)
	}
	return managererrors.WrapError(managererrors.ErrorInternal, err, "source sandbox %s failed while %s", sourceID, operation)
}

// validateForkSource rejects a source that was replaced, is recycling, or is
// not Running.
func validateForkSource(source infra.Sandbox, sourceID, sourceUID string) error {
	if string(source.GetUID()) != sourceUID {
		return managererrors.NewError(managererrors.ErrorNotFound, "source sandbox %s disappeared while preparing fork", sourceID)
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
	return nil
}

// ForkSandbox checkpoints Source once, then creates Count independent children
// from that checkpoint. A child failure does not stop the remaining attempts.
// The checkpoint is private to this call: results never expose its ID,
// snapshots listing filters it, and public clone rejects it, so no caller can
// retry against it. Once created it is therefore deleted on every exit; the
// TTL only covers a crash between creation and cleanup or a failed deletion.
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
	if opts.Timeout <= 0 {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "fork timeout must be positive")
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
		return nil, classifyForkSourceError(err, opts.SourceID, "looking up source")
	}
	sourceUID := string(source.GetUID())
	// The informer view rejects an unforkable source before any lock write.
	if err := validateForkSource(source, opts.SourceID, sourceUID); err != nil {
		return nil, err
	}
	// A caller that is already gone gets no checkpoint; once the checkpoint
	// starts, source recovery runs on the detached context below.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), consts.DefaultWaitCheckpointTimeout)
	defer cancel()
	var networkPolicy *infra.SandboxNetworkConfig
	var securityRules string
	checkpointID, err := m.CreateCheckpoint(checkpointCtx, source, CreateCheckpointOptions{
		Operation: CheckpointOperationFork,
		Infra: infra.CreateCheckpointOptions{
			KeepRunning: ptr.To(true),
			TTL:         ptr.To(forkCheckpointTTL),
			Fork:        true,
		},
		BeforeCheckpoint: func(lockCtx context.Context, locked infra.Sandbox) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := validateForkSource(locked, opts.SourceID, sourceUID); err != nil {
				return err
			}
			securityRules = locked.GetAnnotations()[v1alpha1.AnnotationSecurityRules]
			sourceNetworkPolicy, err := locked.SelectNetworkPolicy(lockCtx)
			if err != nil {
				return classifyForkSourceError(err, opts.SourceID, "reading network policy")
			}
			if sourceNetworkPolicy != nil {
				networkPolicy = &infra.SandboxNetworkConfig{
					AllowOut: append([]string(nil), sourceNetworkPolicy.AllowOut...),
					DenyOut:  append([]string(nil), sourceNetworkPolicy.DenyOut...),
				}
			}
			return nil
		},
	})
	if err != nil {
		return nil, classifyForkSourceError(err, opts.SourceID, "creating checkpoint")
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), consts.DefaultWaitCheckpointTimeout)
		defer cleanupCancel()
		if err := m.infra.DeleteForkCheckpoint(cleanupCtx, opts.Namespace, sourceUID, checkpointID); err != nil {
			klog.FromContext(ctx).Error(err, "failed to delete fork checkpoint", "checkpointID", checkpointID)
		}
	}()

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
				results[index] = m.cloneForkChild(ctx, opts, checkpointID, networkPolicy, securityRules)
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
	return results, nil
}

func (m *SandboxManager) cloneForkChild(ctx context.Context, opts ForkSandboxOptions, checkpointID string, networkPolicy *infra.SandboxNetworkConfig, securityRules string) ForkSandboxResult {
	var autoPausePolicy *v1alpha1.AutoPausePolicy
	child, cloneErr := m.CloneSandbox(ctx, CloneSandboxOptions{
		Infra: infra.CloneSandboxOptions{
			Namespace:    opts.Namespace,
			User:         opts.User,
			CheckPointID: checkpointID,
			Modifier: func(sbx infra.Sandbox) error {
				if securityRules != "" {
					annotations := sbx.GetAnnotations()
					if annotations == nil {
						annotations = map[string]string{}
					}
					annotations[v1alpha1.AnnotationSecurityRules] = securityRules
					sbx.SetAnnotations(annotations)
				}
				if opts.AutoPause && opts.PausedRetentionAnnotation != "" {
					annotations := sbx.GetAnnotations()
					if annotations == nil {
						annotations = map[string]string{}
					}
					annotations[v1alpha1.AnnotationReservePausedSandboxDuration] = opts.PausedRetentionAnnotation
					sbx.SetAnnotations(annotations)
				}
				// The child boots under the preparation deadline with the
				// auto-pause policy withheld, so the only deadline during boot
				// is the bounded preparation timeout; saveForkChildTimeout
				// then applies the requested lifecycle in one update once the
				// child is Ready.
				autoPausePolicy = sbx.GetAutoPausePolicy()
				sbx.SetAutoPausePolicy(nil)
				sbx.SetTimeout(forkPreparationTimeout(time.Now()))
				return nil
			},
			ReserveFailedSandboxFor: ptr.To(consts.ReserveFailedSandboxNever),
			NetworkPolicy:           networkPolicy,
			AllowForkCheckpoint:     true,
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

// saveForkChildTimeout applies the requested lifecycle to a Ready child in a
// single update: the final deadlines (pause plus shutdown-after-retention, or
// plain shutdown) and the auto-pause policy withheld during boot. A child
// whose final lifecycle cannot be persisted would otherwise sit on the
// preparation deadline, so it is killed and its quota released, and the
// failure surfaces as that child's item error.
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
