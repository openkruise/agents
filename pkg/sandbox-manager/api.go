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
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("pool %s not found", template))
	}
	sandbox, err := pool.ClaimSandbox(ctx, user, consts.DefaultPoolingCandidateCounts, opts)
	if err != nil {
		return nil, errors.NewError(errors.ErrorInternal, fmt.Sprintf("failed to claim sandbox: %v", err))
	}
	log.Info("sandbox claimed", "sandbox", klog.KObj(sandbox), "cost", time.Since(start))
	start = time.Now()
	route := sandbox.GetRoute()
	err = m.proxy.SyncRouteWithPeers(route)
	if err != nil {
		log.Error(err, "failed to sync route with peers", "cost", time.Since(start))
	} else {
		log.Info("route synced with peers", "cost", time.Since(start), "route", route)
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
