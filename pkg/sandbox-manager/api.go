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
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/otel/attribute"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/quota"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/sandboxroute"
	"github.com/openkruise/agents/pkg/tracing"
	"github.com/openkruise/agents/pkg/utils/pagination"
)

// ClaimSandboxOptions wraps infra-level claim options with an optional quota spec.
// The manager builds the admission internally from Quota and the infra User field.
type ClaimSandboxOptions struct {
	Infra infra.ClaimSandboxOptions
	Quota *quotaspec.QuotaSpec
}

// CloneSandboxOptions wraps infra-level clone options with an optional quota spec.
type CloneSandboxOptions struct {
	Infra infra.CloneSandboxOptions
	Quota *quotaspec.QuotaSpec
}

// DeleteSandboxOptions carries the sandbox, user identity, and optional quota spec
// needed for delete and post-delete quota release.
type DeleteSandboxOptions struct {
	Sandbox infra.Sandbox
	User    string
	Quota   *quotaspec.QuotaSpec
}

// quotaAdmission builds a SandboxAdmission that enforces the given quota spec via
// the manager's QuotaEnforcer. Returns nil when enforcement is not applicable.
func (m *SandboxManager) quotaAdmission(user string, spec *quotaspec.QuotaSpec) *infra.SandboxAdmission {
	if m == nil || m.quota == nil || spec == nil || !spec.IsLimited() {
		return nil
	}
	quotaSpec := spec.DeepCopy()
	return &infra.SandboxAdmission{
		Acquire: func(ctx context.Context, lockString string, resource infra.SandboxResource) error {
			err := m.quota.Acquire(ctx, quota.AcquireRequest{
				User:       user,
				LockString: lockString,
				Quota:      quotaSpec,
				Footprint:  quota.FootprintFromResource(resource),
				Scopes:     []quotaspec.QuotaScope{quotaspec.ScopeRunning},
			})
			if errors.Is(err, quota.ErrQuotaExceeded) {
				return managererrors.NewError(managererrors.ErrorQuotaExceeded, "api-key quota exceeded")
			}
			return err
		},
		Release: func(ctx context.Context, lockString string) error {
			return m.quota.Release(ctx, quota.ReleaseRequest{User: user, LockString: lockString})
		},
	}
}

// releaseQuotaAfterDelete releases quota after a successful delete.
// It uses a bounded context derived from the caller's context to avoid blocking.
func (m *SandboxManager) releaseQuotaAfterDelete(ctx context.Context, opts DeleteSandboxOptions) {
	if m == nil || m.quota == nil || opts.Quota == nil || !opts.Quota.IsLimited() || opts.Sandbox == nil {
		return
	}
	annotations := opts.Sandbox.GetAnnotations()
	if annotations[v1alpha1.AnnotationOwner] != opts.User {
		return
	}
	lockString := annotations[v1alpha1.AnnotationLock]
	if lockString == "" {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), infra.SandboxAdmissionReleaseTimeout)
	defer cancel()
	if err := m.quota.Release(releaseCtx, quota.ReleaseRequest{User: opts.User, LockString: lockString}); err != nil {
		klog.FromContext(releaseCtx).Error(err, "failed to release quota after accepted sandbox delete", "owner", opts.User, "lockString", lockString)
	}
}

// preserveTypedError keeps an already-classified manager error (e.g. the
// ErrorBadRequest / ErrorInternal produced by the infra create-error classifier)
// so its ErrorCode survives up to the HTTP layer and maps to the right status.
// Only untyped errors are wrapped as ErrorInternal with the given context.
func preserveTypedError(err error, contextMsg string) error {
	if managererrors.GetErrCode(err) != managererrors.ErrorUnknown {
		return err
	}
	return managererrors.NewError(managererrors.ErrorInternal, "%s: %v", contextMsg, err)
}

// ClaimSandbox attempts to lock a Pod and assign it to the current caller.
//
// Two counters are recorded on failure paths and they have distinct semantics
// (so this is NOT double counting):
//   - sandboxClaimCreationResponses: API-level result counter (success/failure).
//   - sandboxClaimTotal: claim-operation counter broken down by lock_type.
func (m *SandboxManager) ClaimSandbox(ctx context.Context, opts ClaimSandboxOptions) (sandbox infra.Sandbox, err error) {
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanManagerClaimSandbox)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()
	log := klog.FromContext(ctx)
	infraOpts := opts.Infra
	infraOpts.Admission = m.quotaAdmission(infraOpts.User, opts.Quota)
	infraOpts.Modifier = withSandboxIDAssignment(infraOpts.Modifier, m.enableShortID, m.shortIDPrefix, m.generateSandboxID)
	infraOpts.TrafficAccessTokenValidity = m.trafficTokenOptions.RequestedValidity

	if !m.infra.HasTemplate(ctx, infra.HasTemplateOptions{Namespace: infraOpts.Namespace, Name: infraOpts.Template}) {
		// Template lookup failed before any sandbox was picked, so lock_type is unknown.
		sandboxClaimCreationResponses.WithLabelValues(infraOpts.Namespace, "failure").Inc()
		sandboxClaimTotal.WithLabelValues(infraOpts.Namespace, "failure", "unknown").Inc()
		return nil, managererrors.NewError(managererrors.ErrorNotFound, "template %s not found", infraOpts.Template)
	}
	sandbox, claimMetrics, err := m.infra.ClaimSandbox(ctx, infraOpts)
	span.SetAttributes(
		attribute.String(tracing.AttrClaimLockType, string(claimMetrics.LockType)),
		attribute.Int(tracing.AttrClaimRetries, claimMetrics.Retries),
		attribute.Float64(tracing.AttrClaimDuration, claimMetrics.Total.Seconds()),
	)
	if err != nil {
		log.Error(err, "failed to claim sandbox", "metrics", claimMetrics.String())
		// claimMetrics may carry the actual lock_type even on failure; fall back to
		// "unknown" only when infra never reached the lock step.
		lockType := string(claimMetrics.LockType)
		if lockType == "" {
			lockType = "unknown"
		}
		sandboxClaimCreationResponses.WithLabelValues(infraOpts.Namespace, "failure").Inc()
		sandboxClaimTotal.WithLabelValues(infraOpts.Namespace, "failure", lockType).Inc()
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

func (m *SandboxManager) CloneSandbox(ctx context.Context, opts CloneSandboxOptions) (sandbox infra.Sandbox, err error) {
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanManagerCloneSandbox)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()
	log := klog.FromContext(ctx)
	infraOpts := opts.Infra
	infraOpts.Admission = m.quotaAdmission(infraOpts.User, opts.Quota)
	infraOpts.Modifier = withSandboxIDAssignment(infraOpts.Modifier, m.enableShortID, m.shortIDPrefix, m.generateSandboxID)
	infraOpts.TrafficAccessTokenValidity = m.trafficTokenOptions.RequestedValidity

	sandbox, cloneMetrics, err := m.infra.CloneSandbox(ctx, infraOpts)
	if err != nil {
		log.Error(err, "failed to clone sandbox", "metrics", cloneMetrics)
		sandboxCloneTotal.WithLabelValues(infraOpts.Namespace, "failure").Inc()
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
//
// The infra lookup may wait while ctx is live, so pass a context with a
// deadline (e.g. context.WithTimeout).
func (m *SandboxManager) GetSandbox(ctx context.Context, user string, expectedStates []string, opts infra.GetSandboxOptions) (infra.Sandbox, error) {
	log := klog.FromContext(ctx).WithValues("sandboxID", opts.SandboxID)
	if user == "" {
		return nil, managererrors.NewError(managererrors.ErrorBadRequest, "user is required")
	}
	log.Info("try to get claimed sandbox")
	sbx, err := m.infra.GetSandbox(ctx, opts)
	if err != nil {
		// A definitive miss is a normal client outcome; only an inconclusive
		// lookup indicates an infra failure worth an error-level record.
		if errors.Is(err, infra.ErrSandboxNotFound) {
			log.Info("sandbox not found", "err", err)
			return nil, managererrors.NewError(managererrors.ErrorNotFound, "sandbox %s not found", opts.SandboxID)
		}
		if errors.Is(err, infra.ErrSandboxIDAmbiguous) {
			log.Info("sandbox ID is ambiguous", "err", err)
			return nil, managererrors.WrapError(managererrors.ErrorNotFound, err, "sandbox %s not found: %v", opts.SandboxID, err)
		}
		log.Error(err, "failed to get sandbox from cache")
		return nil, managererrors.NewError(managererrors.ErrorInternal, "failed to get sandbox %s: %v", opts.SandboxID, err)
	}

	state, reason := sbx.GetState()

	if sbx.GetAnnotations()[v1alpha1.AnnotationOwner] != user {
		log.Error(nil, "sandbox is not owned by user")
		return nil, managererrors.NewError(managererrors.ErrorNotAllowed, "sandbox %s is not owned", opts.SandboxID)
	}

	if len(expectedStates) == 0 {
		return sbx, nil
	}
	if slices.Contains(expectedStates, state) {
		return sbx, nil
	}
	log.Error(nil, "sandbox state is not expected", "state", state, "reason", reason, "expectedStates", expectedStates)
	return nil, managererrors.NewError(managererrors.ErrorBadRequest, "sandbox %s is not healthy (state %s, reason %s)", opts.SandboxID, state, reason)
}

func (m *SandboxManager) ListSandboxes(ctx context.Context, opts infra.SelectSandboxesOptions, p *pagination.Paginator[infra.Sandbox]) ([]infra.Sandbox, string, error) {
	sandboxes, err := m.infra.SelectSandboxes(ctx, opts)
	if err != nil {
		return nil, "", managererrors.NewError(managererrors.ErrorNotFound, "failed to list sandboxes: %v", err)
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
		return nil, "", managererrors.NewError(managererrors.ErrorNotFound, "failed to list checkpoints: %v", err)
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

// GetOwnerOfVolume returns the owner (UserID) of the volume identified by volumeID (PV Name)
// in the given namespace. Returns ("", false) if the volume is not found.
func (m *SandboxManager) GetOwnerOfVolume(ctx context.Context, namespace, volumeID string) (string, bool) {
	log := klog.FromContext(ctx)
	pvcList := &corev1.PersistentVolumeClaimList{}
	err := m.infra.GetCache().GetClient().List(ctx, pvcList,
		client.InNamespace(namespace),
		client.MatchingFields{cache.IndexVolumeName: volumeID},
	)
	if err != nil {
		log.Error(err, "failed to list PVCs for volume ownership check", "namespace", namespace, "volumeID", volumeID)
		return "", false
	}
	if len(pvcList.Items) == 0 {
		log.Info("no PVC found for volume ownership check", "namespace", namespace, "volumeID", volumeID)
		return "", false
	}
	return pvcList.Items[0].GetAnnotations()[v1alpha1.AnnotationOwner], true
}

// syncRoute syncs the sandbox route with peers.
// If refresh is true, it will refresh the sandbox state before syncing.
// Peer fanout ignores client cancellation because bounded retries and Sandbox
// informers make it a short, best-effort operation. Returns error if route sync
// fails, but refresh failures are logged and ignored.
func (m *SandboxManager) syncRoute(ctx context.Context, sbx infra.Sandbox, refresh bool) (err error) {
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanProxySyncRoute)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	// Refresh sandbox to get the latest state if needed
	if refresh {
		if err := sbx.InplaceRefresh(ctx, false); err != nil {
			log.Error(err, "failed to refresh sandbox, route sync may use stale state")
			// Continue to sync route even if refresh fails, as the route might still be valid
		}
	}
	start := time.Now()
	route, err := sbx.GetRoute()
	if err != nil {
		return err
	}
	span.SetAttributes(attribute.String(tracing.AttrRouteID, route.ID))
	result := m.proxy.SetRoute(route)
	sandboxroute.LogMutation(log, "upsert", route, result)
	err = m.proxy.SyncRouteWithPeers(context.WithoutCancel(ctx), route)
	duration := time.Since(start).Seconds()
	span.SetAttributes(attribute.Bool(tracing.AttrPeersSynced, err == nil))
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

// pauseResumeConfig carries the operation-specific parts of the shared
// pause/resume orchestration in runPauseResume: span name, log verb,
// metrics vectors, and the underlying sandbox operation.
type pauseResumeConfig struct {
	spanName  string
	opName    string
	responses *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	op        func(ctx context.Context) error
}

// runPauseResume owns the orchestration shared by PauseSandbox and
// ResumeSandbox: span lifecycle, timing/result metrics, and route sync.
// Operation-specific behavior is provided via pauseResumeConfig.
func (m *SandboxManager) runPauseResume(ctx context.Context, sbx infra.Sandbox, cfg pauseResumeConfig) (err error) {
	ctx, span := tracing.StartManagerSpan(ctx, cfg.spanName)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	if err := cfg.op(ctx); err != nil {
		log.Error(err, fmt.Sprintf("failed to %s sandbox", cfg.opName))
		cfg.responses.WithLabelValues(sbx.GetNamespace(), "failure").Inc()
		return err
	}
	cfg.responses.WithLabelValues(sbx.GetNamespace(), "success").Inc()
	cfg.duration.WithLabelValues(sbx.GetNamespace()).Observe(time.Since(start).Seconds())
	if err := m.syncRoute(ctx, sbx, true); err != nil {
		log.Error(err, fmt.Sprintf("failed to sync route with peers after %s", cfg.opName))
	}
	return nil
}

// PauseSandbox pauses a sandbox and syncs route with peers
func (m *SandboxManager) PauseSandbox(ctx context.Context, sbx infra.Sandbox, opts infra.PauseOptions) error {
	return m.runPauseResume(ctx, sbx, pauseResumeConfig{
		spanName:  tracing.SpanManagerPauseSandbox,
		opName:    "pause",
		responses: sandboxPauseResponses,
		duration:  sandboxPauseDuration,
		op:        func(ctx context.Context) error { return sbx.Pause(ctx, opts) },
	})
}

// ResumeSandbox resumes a sandbox and syncs route with peers
func (m *SandboxManager) ResumeSandbox(ctx context.Context, sbx infra.Sandbox, opts infra.ResumeOptions) error {
	return m.runPauseResume(ctx, sbx, pauseResumeConfig{
		spanName:  tracing.SpanManagerResumeSandbox,
		opName:    "resume",
		responses: sandboxResumeResponses,
		duration:  sandboxResumeDuration,
		op:        func(ctx context.Context) error { return sbx.Resume(ctx, opts) },
	})
}

// deleteRouteAndSync attempts to remove the current local route and syncs the deletion with peers.
func (m *SandboxManager) deleteRouteAndSync(ctx context.Context, sbx infra.Sandbox) {
	log := klog.FromContext(ctx)
	key := types.NamespacedName{Namespace: sbx.GetNamespace(), Name: sbx.GetName()}
	deletion := sandboxroute.Route{
		Namespace:       key.Namespace,
		Name:            key.Name,
		ResourceVersion: sbx.GetResourceVersion(),
	}
	result := m.proxy.Delete(deletion)
	sandboxroute.LogMutation(log, "delete", deletion, result)
	route, err := sbx.GetRoute()
	if err != nil {
		log.Error(err, "failed to project deleted sandbox route")
		return
	}
	route.State = v1alpha1.SandboxStateDead
	// Peer fanout is detached from the request ctx so client cancellation does
	// not abort this bounded, best-effort dead-route push.
	if err := m.proxy.SyncRouteWithPeers(context.WithoutCancel(ctx), route); err != nil {
		log.Error(err, "failed to sync route with peers")
	}
}

// DeleteSandbox deletes a sandbox and syncs route with peers.
// If the sandbox is cleanup-enabled and in Running phase, it triggers cleanup instead of deletion.
// On both accepted-delete return paths (reuse trigger success and Kill success), quota is released.
func (m *SandboxManager) DeleteSandbox(ctx context.Context, opts DeleteSandboxOptions) (err error) {
	ctx, span := tracing.StartManagerSpan(ctx, tracing.SpanManagerDeleteSandbox)
	// Keep the closure: a direct defer tracing.EndSpan(ctx, span, err) would
	// evaluate err while still nil and record every failure as success.
	defer func() { tracing.EndSpan(ctx, span, err) }()
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(opts.Sandbox))
	sbx := opts.Sandbox

	if sbx.IsRecycleEnabled() && sbx.Phase() == string(v1alpha1.SandboxRunning) {
		log.Info("sandbox is recycle-enabled, triggering recycle instead of deletion")
		start := time.Now()
		if err := sbx.TriggerRecycle(ctx); err != nil {
			log.Error(err, "failed to trigger recycle, falling back to delete")
			sandboxRecycleResponses.WithLabelValues(sbx.GetNamespace(), "failure").Inc()
		} else {
			sandboxRecycleResponses.WithLabelValues(sbx.GetNamespace(), "success").Inc()
			sandboxRecycleDuration.WithLabelValues(sbx.GetNamespace()).Observe(time.Since(start).Seconds())
			span.SetAttributes(attribute.Bool(tracing.AttrReuseTriggered, true))
			m.deleteRouteAndSync(ctx, sbx)
			m.releaseQuotaAfterDelete(ctx, opts)
			return nil
		}
	}
	span.SetAttributes(attribute.Bool(tracing.AttrReuseTriggered, false))

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
	m.releaseQuotaAfterDelete(ctx, opts)
	return nil
}
