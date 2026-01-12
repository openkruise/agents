package sandbox_manager

import (
	"context"
	"fmt"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"k8s.io/klog/v2"
)

// ClaimSandbox attempts to lock a Pod and assign it to the current caller
func (m *SandboxManager) ClaimSandbox(ctx context.Context, user, template string, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
	log := klog.FromContext(ctx)
	start := time.Now()
	pool, ok := m.infra.GetPoolByTemplate(template)
	if !ok {
		// Requirement: Track failure in API layer
		SandboxCreationResponses.WithLabelValues("failure").Inc()
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("pool %s not found", template))
	}
	sandbox, err := pool.ClaimSandbox(ctx, user, consts.DefaultPoolingCandidateCounts, opts)
	if err != nil {
		// Requirement: Track failure in API layer
		SandboxCreationResponses.WithLabelValues("failure").Inc()
		return nil, errors.NewError(errors.ErrorInternal, fmt.Sprintf("failed to claim sandbox: %v", err))
	}

	// Success: Record metrics
	SandboxCreationResponses.WithLabelValues("success").Inc()
	// Requirement: Only measure the latency when no error exists
	SandboxCreationLatency.Observe(float64(time.Since(start).Milliseconds()))

	log.Info("sandbox claimed", "sandbox", klog.KObj(sandbox), "cost", time.Since(start))

	// Sync route without refresh since sandbox was just claimed and state is already up-to-date
	if err = m.syncRoute(ctx, sandbox, false); err != nil {
		log.Error(err, "failed to sync route with peers after claim")
	}
	return sandbox, nil
}

// GetClaimedSandbox returns a claimed (running or paused) Pod by its ID
func (m *SandboxManager) GetClaimedSandbox(ctx context.Context, user, sandboxID string) (infra.Sandbox, error) {
	sbx, err := m.infra.GetSandbox(ctx, sandboxID)
	if err != nil {
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("sandbox %s not found", sandboxID))
	}

	state, reason := sbx.GetState()
	if state != v1alpha1.SandboxStatePaused && state != v1alpha1.SandboxStateRunning {
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("sandbox %s is not claimed (state %s, reason %s)", sandboxID, state, reason))
	}
	if sbx.GetRoute().Owner != user {
		return nil, errors.NewError(errors.ErrorNotAllowed, fmt.Sprintf("sandbox %s is not owned", sandboxID))
	}
	return sbx, nil
}

func (m *SandboxManager) ListSandboxes(user string, limit int, filter func(infra.Sandbox) bool) ([]infra.Sandbox, error) {
	sandboxes, err := m.infra.SelectSandboxes(user, limit, filter)
	if err != nil {
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("failed to list sandboxes: %v", err))
	}
	return sandboxes, nil
}

func (m *SandboxManager) GetOwnerOfSandbox(sandboxID string) (string, bool) {
	route, ok := m.proxy.LoadRoute(sandboxID)
	return route.Owner, ok
}

// syncRoute syncs the sandbox route with peers
// If refresh is true, it will refresh the sandbox state before syncing
// Returns error if route sync fails, but refresh failures are logged and ignored
func (m *SandboxManager) syncRoute(ctx context.Context, sbx infra.Sandbox, refresh bool) error {
	log := klog.FromContext(ctx)
	// Refresh sandbox to get the latest state if needed
	if refresh {
		if err := sbx.InplaceRefresh(false); err != nil {
			log.Error(err, "failed to refresh sandbox, route sync may use stale state")
			// Continue to sync route even if refresh fails, as the route might still be valid
		}
	}
	start := time.Now()
	route := sbx.GetRoute()
	m.proxy.SetRoute(route)
	err := m.proxy.SyncRouteWithPeers(route)
	if err != nil {
		return err
	}
	log.Info("route synced with peers", "cost", time.Since(start), "route", route)
	return nil
}

// PauseSandbox pauses a sandbox and syncs route with peers
func (m *SandboxManager) PauseSandbox(ctx context.Context, sbx infra.Sandbox) error {
	log := klog.FromContext(ctx)
	if err := sbx.Pause(ctx); err != nil {
		log.Error(err, "failed to pause sandbox")
		return err
	}
	if err := m.syncRoute(ctx, sbx, true); err != nil {
		log.Error(err, "failed to sync route with peers after pause")
	}
	return nil
}

// ResumeSandbox resumes a sandbox and syncs route with peers
func (m *SandboxManager) ResumeSandbox(ctx context.Context, sbx infra.Sandbox) error {
	log := klog.FromContext(ctx)
	if err := sbx.Resume(ctx); err != nil {
		log.Error(err, "failed to resume sandbox")
		return err
	}
	if err := m.syncRoute(ctx, sbx, true); err != nil {
		log.Error(err, "failed to sync route with peers after resume")
	}
	return nil
}
