package sandboxcr

import (
	"context"
	"fmt"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	"k8s.io/klog/v2"
)

func WaitForInplaceUpdate(ctx context.Context, sbx *Sandbox, opts infra.InplaceUpdateOptions, cache *Cache) (cost time.Duration, err error) {
	log := klog.FromContext(ctx).V(consts.DebugLogLevel).WithValues("sandbox", klog.KObj(sbx))
	start := time.Now()
	if opts.Timeout == 0 {
		opts.Timeout = consts.DefaultInplaceUpdateTimeout
	}
	log.Info("waiting for inplace-update to finish", "opts", opts)
	err = cache.WaitForSandboxSatisfied(ctx, sbx.Sandbox, WaitActionInplaceUpdate, func(sbx *v1alpha1.Sandbox) (bool, error) {
		return CheckSandboxInplaceUpdate(ctx, sbx)
	}, opts.Timeout)
	return time.Since(start), err
}

func CheckSandboxInplaceUpdate(ctx context.Context, sbx *v1alpha1.Sandbox) (bool, error) {
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx)).V(consts.DebugLogLevel)
	if sbx.Status.ObservedGeneration != sbx.Generation {
		log.Info("watched sandbox not updated", "generation", sbx.Generation, "observedGeneration", sbx.Status.ObservedGeneration)
		return false, nil
	}
	cond := GetSandboxCondition(sbx, v1alpha1.SandboxConditionReady)
	if cond.Reason == v1alpha1.SandboxReadyReasonStartContainerFailed {
		err := retriableError{Message: fmt.Sprintf("sandbox inplace update failed: %s", cond.Message)}
		log.Error(err, "sandbox inplace update failed")
		return false, err // stop early
	}
	state, reason := stateutils.GetSandboxState(sbx)
	log.Info("sandbox update watched", "state", state, "reason", reason)
	return state == v1alpha1.SandboxStateRunning, nil
}
