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

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/pagination"
)

// preserveTypedError keeps an already-classified manager error (e.g. the
// ErrorBadRequest / ErrorInternal produced by the infra create-error classifier)
// so its ErrorCode survives up to the HTTP layer and maps to the right status.
// Only untyped errors are wrapped as ErrorInternal with the given context.
func preserveTypedError(err error, contextMsg string) error {
	if errors.GetErrCode(err) != errors.ErrorUnknown {
		return err
	}
	return errors.NewError(errors.ErrorInternal, "%s: %v", contextMsg, err)
}

// ClaimSandbox attempts to lock a Pod and assign it to the current caller.
//
// Two counters are recorded on failure paths and they have distinct semantics
// (so this is NOT double counting):
//   - sandboxClaimCreationResponses: API-level result counter (success/failure).
//   - sandboxClaimTotal: claim-operation counter broken down by lock_type.
func (m *SandboxManager) ClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
	log := klog.FromContext(ctx)
	if !m.infra.HasTemplate(ctx, infra.HasTemplateOptions{Namespace: opts.Namespace, Name: opts.Template}) {
		// Template lookup failed before any sandbox was picked, so lock_type is unknown.
		sandboxClaimCreationResponses.WithLabelValues(opts.Namespace, "failure").Inc()
		sandboxClaimTotal.WithLabelValues(opts.Namespace, "failure", "unknown").Inc()
		return nil, errors.NewError(errors.ErrorNotFound, "template %s not found", opts.Template)
	}
	sandbox, claimMetrics, err := m.infra.ClaimSandbox(ctx, opts)
	if err != nil {
		log.Error(err, "failed to claim sandbox", "metrics", claimMetrics.String())
		// claimMetrics may carry the actual lock_type even on failure; fall back to
		// "unknown" only when infra never reached the lock step.
		lockType := string(claimMetrics.LockType)
		if lockType == "" {
			lockType = "unknown"
		}
		sandboxClaimCreationResponses.WithLabelValues(opts.Namespace, "failure").Inc()
		sandboxClaimTotal.WithLabelValues(opts.Namespace, "failure", lockType).Inc()
		return nil, preserveTypedError(err, "failed to claim sandbox")
	}

	// Success: Record metrics
	sandboxClaimCreationResponses.WithLabelValues(sandbox.GetNamespace(), "success").Inc()

	// Claim-specific metrics
	sandboxClaimDuration.WithLabelValues(sandbox.GetNamespace()).Observe(claimMetrics.Total.Seconds())
	sandboxClaimTotal.WithLabelValues(sandbox.GetNamespace(), "success", string(claimMetrics.LockType)).Inc()
	sandboxClaimRetries.WithLabelValues(sandbox.GetNamespace()).Observe(float64(claimMetrics.Retries))

	state, reason := sandbox.GetState()
	log.Info("sandbox claimed", "sandbox", klog.KObj(sandbox), "metrics", claimMetrics.String(), "state", state, "reason", reason)

	// Sync route without refresh since sandbox was just claimed and state is already up-to-date
	if err = m.syncRoute(ctx, sandbox, false); err != nil {
		log.Error(err, "failed to sync route with peers after claim")
	}
	return sandbox, nil
}

func (m *SandboxManager) CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions) (infra.Sandbox, error) {
	log := klog.FromContext(ctx)
	sandbox, cloneMetrics, err := m.infra.CloneSandbox(ctx, opts)
	if err != nil {
		log.Error(err, "failed to clone sandbox", "metrics", cloneMetrics)
		sandboxCloneTotal.WithLabelValues(opts.Namespace, "failure").Inc()
		return nil, preserveTypedError(err, "failed to clone sandbox")
	}

	// Clone-specific metrics
	sandboxCloneDuration.WithLabelValues(sandbox.GetNamespace()).Observe(cloneMetrics.Total.Seconds())
	sandboxCloneTotal.WithLabelValues(sandbox.GetNamespace(), "success").Inc()

	state, reason := sandbox.GetState()
	log.Info("sandbox cloned", "sandbox", klog.KObj(sandbox), "metrics", cloneMetrics.String(), "state", state, "reason", reason)

	// Sync route without refresh since sandbox was just claimed and state is already up-to-date
	if err = m.syncRoute(ctx, sandbox, false); err != nil {
		log.Error(err, "failed to sync route with peers after claim")
	}
	return sandbox, nil
}

// GetSandbox returns a sandbox owned by user and optionally filters it by state.
// When expectedStates is empty, ownership is still checked but any claimed state
// is accepted. When expectedStates is non-empty, the sandbox state must match one
// of the provided values.
func (m *SandboxManager) GetSandbox(ctx context.Context, user string, expectedStates []string, opts infra.GetSandboxOptions) (infra.Sandbox, error) {
	log := klog.FromContext(ctx).WithValues("sandboxID", opts.SandboxID)
	if user == "" {
		return nil, errors.NewError(errors.ErrorBadRequest, "user is required")
	}
	log.Info("try to get claimed sandbox")
	sbx, err := m.infra.GetSandbox(ctx, opts)
	if err != nil {
		log.Error(err, "failed to get sandbox from cache")
		return nil, errors.NewError(errors.ErrorNotFound, "sandbox %s not found", opts.SandboxID)
	}

	state, reason := sbx.GetState()

	if sbx.GetRoute().Owner != user {
		log.Error(nil, "sandbox is not owned by user")
		return nil, errors.NewError(errors.ErrorNotAllowed, "sandbox %s is not owned", opts.SandboxID)
	}

	if len(expectedStates) == 0 {
		return sbx, nil
	}
	for _, expectedState := range expectedStates {
		if state == expectedState {
			return sbx, nil
		}
	}
	log.Error(nil, "sandbox state is not expected", "state", state, "reason", reason, "expectedStates", expectedStates)
	return nil, errors.NewError(errors.ErrorBadRequest, "sandbox %s is not healthy (state %s, reason %s)", opts.SandboxID, state, reason)
}

func (m *SandboxManager) ListSandboxes(ctx context.Context, opts infra.SelectSandboxesOptions, p *pagination.Paginator[infra.Sandbox]) ([]infra.Sandbox, string, error) {
	sandboxes, err := m.infra.SelectSandboxes(ctx, opts)
	if err != nil {
		return nil, "", errors.NewError(errors.ErrorNotFound, "failed to list sandboxes: %v", err)
	}
	var nextToken string
	if p != nil {
		sandboxes, nextToken = p.Apply(sandboxes)
	}
	return sandboxes, nextToken, nil
}

func (m *SandboxManager) ListCheckpoints(ctx context.Context, opts infra.SelectSucceededCheckpointsOptions, p *pagination.Paginator[infra.CheckpointInfo]) ([]infra.CheckpointInfo, string, error) {
	checkpoints, err := m.infra.SelectSucceededCheckpoints(ctx, opts)
	if err != nil {
		return nil, "", errors.NewError(errors.ErrorNotFound, "failed to list checkpoints: %v", err)
	}
	var nextToken string
	if p != nil {
		checkpoints, nextToken = p.Apply(checkpoints)
	}
	return checkpoints, nextToken, nil
}

func (m *SandboxManager) DeleteCheckpoint(ctx context.Context, user string, opts infra.DeleteCheckpointOptions) error {
	log := klog.FromContext(ctx).WithValues("checkpointID", opts.CheckpointID)
	opts.User = user
	if err := m.infra.DeleteCheckpoint(ctx, opts); err != nil {
		log.Error(err, "failed to delete checkpoint")
		return err
	}
	log.Info("checkpoint deleted by infra")
	return nil
}

func (m *SandboxManager) GetOwnerOfSandbox(sandboxID string) (string, bool) {
	route, ok := m.proxy.LoadRoute(sandboxID)
	return route.Owner, ok
}

// syncRoute syncs the sandbox route with peers
// If refresh is true, it will refresh the sandbox state before syncing
// Returns error if route sync fails, but refresh failures are logged and ignored
func (m *SandboxManager) syncRoute(ctx context.Context, sbx infra.Sandbox, refresh bool) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	// Refresh sandbox to get the latest state if needed
	if refresh {
		if err := sbx.InplaceRefresh(ctx, false); err != nil {
			log.Error(err, "failed to refresh sandbox, route sync may use stale state")
			// Continue to sync route even if refresh fails, as the route might still be valid
		}
	}
	start := time.Now()
	route := sbx.GetRoute()
	m.proxy.SetRoute(ctx, route)
	err := m.proxy.SyncRouteWithPeers(route)
	duration := time.Since(start).Seconds()
	if err != nil {
		log.Error(err, "failed to sync route with peers")
		sandboxRouteSyncTotal.WithLabelValues(sbx.GetNamespace(), "sync_with_peers", "failure").Inc()
		return err
	}
	sandboxRouteSyncDuration.WithLabelValues(sbx.GetNamespace(), "sync_with_peers").Observe(duration)
	sandboxRouteSyncTotal.WithLabelValues(sbx.GetNamespace(), "sync_with_peers", "success").Inc()
	log.Info("route synced with peers", "cost", time.Since(start), "route", route)
	return nil
}

// PauseSandbox pauses a sandbox and syncs route with peers
func (m *SandboxManager) PauseSandbox(ctx context.Context, sbx infra.Sandbox, opts infra.PauseOptions) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	if err := sbx.Pause(ctx, opts); err != nil {
		log.Error(err, "failed to pause sandbox")
		sandboxPauseResponses.WithLabelValues(sbx.GetNamespace(), "failure").Inc()
		return err
	}
	sandboxPauseResponses.WithLabelValues(sbx.GetNamespace(), "success").Inc()
	sandboxPauseDuration.WithLabelValues(sbx.GetNamespace()).Observe(time.Since(start).Seconds())
	if err := m.syncRoute(ctx, sbx, true); err != nil {
		log.Error(err, "failed to sync route with peers after pause")
	}
	return nil
}

// ResumeSandbox resumes a sandbox and syncs route with peers
func (m *SandboxManager) ResumeSandbox(ctx context.Context, sbx infra.Sandbox, opts infra.ResumeOptions) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	if err := sbx.Resume(ctx, opts); err != nil {
		log.Error(err, "failed to resume sandbox")
		sandboxResumeResponses.WithLabelValues(sbx.GetNamespace(), "failure").Inc()
		return err
	}
	sandboxResumeResponses.WithLabelValues(sbx.GetNamespace(), "success").Inc()
	sandboxResumeDuration.WithLabelValues(sbx.GetNamespace()).Observe(time.Since(start).Seconds())
	if err := m.syncRoute(ctx, sbx, true); err != nil {
		log.Error(err, "failed to sync route with peers after resume")
	}
	return nil
}

// deleteRouteAndSync removes the route locally and syncs the deletion with peers.
func (m *SandboxManager) deleteRouteAndSync(ctx context.Context, sbx infra.Sandbox) {
	log := klog.FromContext(ctx)
	route := sbx.GetRoute()
	route.State = v1alpha1.SandboxStateDead
	m.proxy.DeleteRoute(route.ID)
	if err := m.proxy.SyncRouteWithPeers(route); err != nil {
		log.Error(err, "failed to sync route with peers")
	}
}

// DeleteSandbox deletes a sandbox and syncs route with peers.
// If the sandbox is reuse-enabled and in Running phase, it triggers reuse instead of deletion.
func (m *SandboxManager) DeleteSandbox(ctx context.Context, sbx infra.Sandbox) error {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))

	if sbx.IsReuseEnabled() && sbx.Phase() == string(v1alpha1.SandboxRunning) {
		log.Info("sandbox is reuse-enabled, triggering reuse instead of deletion")
		start := time.Now()
		if err := sbx.TriggerReuse(ctx); err != nil {
			log.Error(err, "failed to trigger reuse, falling back to delete")
			sandboxReuseResponses.WithLabelValues(sbx.GetNamespace(), "failure").Inc()
		} else {
			sandboxReuseResponses.WithLabelValues(sbx.GetNamespace(), "success").Inc()
			sandboxReuseDuration.WithLabelValues(sbx.GetNamespace()).Observe(time.Since(start).Seconds())
			m.deleteRouteAndSync(ctx, sbx)
			return nil
		}
	}

	start := time.Now()
	if err := sbx.Kill(ctx); err != nil {
		log.Error(err, "failed to delete sandbox")
		sandboxDeleteResponses.WithLabelValues(sbx.GetNamespace(), "failure").Inc()
		return err
	}
	sandboxDeleteResponses.WithLabelValues(sbx.GetNamespace(), "success").Inc()
	sandboxDeleteDuration.WithLabelValues(sbx.GetNamespace()).Observe(time.Since(start).Seconds())
	log.Info("sandbox deleted")

	m.deleteRouteAndSync(ctx, sbx)
	return nil
}
