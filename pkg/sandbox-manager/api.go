package sandbox_manager

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/proxy"
	"github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/klog/v2"
)

// ClaimSandbox attempts to lock a Pod and assign it to the current caller
func (m *SandboxManager) ClaimSandbox(ctx context.Context, user, template string, timeoutSeconds int) (infra.Sandbox, error) {
	log := klog.FromContext(ctx)
	start := time.Now()
	pool, ok := m.infra.GetPoolByTemplate(template)
	if !ok {
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("pool %s not found", template))
	}
	sandbox, err := pool.ClaimSandbox(ctx, user, nil)
	if err != nil {
		return nil, errors.NewError(errors.ErrorInternal, fmt.Sprintf("failed to claim sandbox: %v", err))
	}
	go func() {
		if timeoutSeconds > 0 {
			err := m.SetTimer(ctx, sandbox, timeoutSeconds, consts.SandboxKill)
			log.Info("timeout timer set", "sandbox", klog.KObj(sandbox), "seconds", timeoutSeconds, "error", err)
		}
	}()
	route := proxy.Route{
		ID:           sandbox.GetName(),
		IP:           sandbox.GetIP(),
		Owner:        user,
		ExtraHeaders: sandbox.GetRouteHeader(),
	}
	m.proxy.SetRoute(sandbox.GetName(), route)
	log.Info("sandbox claimed", "sandbox", klog.KObj(sandbox), "route", route.IP, "cost", time.Since(start))
	return sandbox, nil
}

// GetClaimedSandbox returns a claimed (running or paused) Pod by its ID
func (m *SandboxManager) GetClaimedSandbox(sandboxID string) (infra.Sandbox, error) {
	sbx, err := m.infra.GetSandbox(sandboxID)
	if err != nil {
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("sandbox %s not found", sandboxID))
	}
	if utils.SandboxClaimed(sbx) {
		return sbx, nil
	} else {
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("sandbox %s is not claimed", sandboxID))
	}
}

func (m *SandboxManager) GetRoute(sandboxID string) (proxy.Route, bool) {
	return m.proxy.LoadRoute(sandboxID)
}

// ListClaimedSandboxes returns a list of claimed with given state (running or paused, both are listed if `state` is not provided)
// Sandboxes by their template and custom label selector.
// NOTE: internal labels will be ignored
func (m *SandboxManager) ListClaimedSandboxes(state string, selector map[string]string) ([]infra.Sandbox, error) {
	options := infra.SandboxSelectorOptions{
		Labels: map[string]string{},
	}
	for k, v := range selector {
		if strings.HasPrefix(k, v1alpha1.InternalPrefix) {
			continue
		}
		options.Labels[k] = v
	}
	switch state {
	case v1alpha1.SandboxStatePaused:
		options.WantPaused = true
	case v1alpha1.SandboxStateRunning:
		options.WantRunning = true
	default:
		options.WantRunning = true
		options.WantPaused = true
	}
	sandboxes, err := m.infra.SelectSandboxes(options)
	if err != nil {
		return nil, errors.NewError(errors.ErrorNotFound, fmt.Sprintf("failed to list sandboxes: %v", err))
	}
	return sandboxes, nil
}

func (m *SandboxManager) DeleteClaimedSandbox(ctx context.Context, sandboxID string) error {
	sbx, err := m.GetClaimedSandbox(sandboxID)
	if err != nil {
		return err
	}
	return m.killSandbox(ctx, sbx)
}

func (m *SandboxManager) killSandbox(ctx context.Context, sbx infra.Sandbox) error {
	if sbx == nil {
		return nil
	}
	m.proxy.DeleteRoute(sbx.GetName())
	if err := sbx.Kill(ctx); err != nil {
		return errors.NewError(errors.ErrorInternal, fmt.Sprintf("failed to delete sandbox %s: %v", sbx.GetName(), err))
	}
	return nil
}

func (m *SandboxManager) SetSandboxTimeout(ctx context.Context, sbx infra.Sandbox, seconds int) error {
	if sbx.GetState() != v1alpha1.SandboxStateRunning {
		return errors.NewError(errors.ErrorConflict, fmt.Sprintf("sandbox %s is not running", sbx.GetName()))
	}
	return m.SetTimer(ctx, sbx, seconds, consts.SandboxKill)
}

func (m *SandboxManager) GetOwnerOfSandbox(sandboxID string) (string, bool) {
	route, ok := m.proxy.LoadRoute(sandboxID)
	return route.Owner, ok
}
