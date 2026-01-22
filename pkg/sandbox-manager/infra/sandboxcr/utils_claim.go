package sandboxcr

import (
	"context"
	"fmt"
	"math/rand"
	"sync"

	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	"k8s.io/klog/v2"
)

func ValidateAndInitClaimOptions(opts infra.ClaimSandboxOptions) (infra.ClaimSandboxOptions, error) {
	if opts.User == "" {
		return infra.ClaimSandboxOptions{}, fmt.Errorf("user is required")
	}
	if opts.Template == "" {
		return infra.ClaimSandboxOptions{}, fmt.Errorf("template is required")
	}
	if opts.CSIMount != nil {
		if opts.InitRuntime == nil {
			return infra.ClaimSandboxOptions{}, fmt.Errorf("init runtime is required when csi mount is specified")
		}
	}
	if opts.CandidateCounts <= 0 {
		opts.CandidateCounts = consts.DefaultPoolingCandidateCounts
	}
	if opts.LockString == "" {
		opts.LockString = utils.NewLockString()
	}
	return opts, nil
}

// TryClaimSandbox attempts to claim a sandbox based on the provided Options.
// The returned sandbox is valid only when nil error is returned. Once a non-nil sandbox is returned,
// the sandbox object should not be used anymore and needs appropriate handling.
//
// ValidateAndInitClaimOptions must be called before this function.
func TryClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions, r *rand.Rand, pickCache *sync.Map,
	cache *Cache, client clients.SandboxClient) (claimed infra.Sandbox, metrics infra.ClaimMetrics, err error) {
	log := klog.FromContext(ctx)
	defer ClearFailedSandbox(ctx, claimed, err, opts.ReserveFailedSandbox)
	// Step 1: Pick an available sandbox
	sbx, err := PickAnAvailableSandbox(ctx, opts.Template, opts.CandidateCounts, r, pickCache, cache, client)
	if err != nil {
		log.Error(err, "failed to select available sandbox")
		return
	}
	defer pickCache.Delete(GetPickKey(sbx.Sandbox))

	log = log.WithValues("sandbox", klog.KObj(sbx.Sandbox))
	log.Info("sandbox picked")

	// Step 2: Modify and lock sandbox. All modifications to be applied to the Sandbox should be performed here.
	if err = ModifyPickedSandbox(ctx, sbx, opts); err != nil {
		log.Error(err, "failed to modify picked sandbox")
		err = retriableError{Message: fmt.Sprintf("failed to modify picked sandbox: %s", err)}
		return
	}

	metrics.PickAndLock, err = PerformLockSandbox(ctx, sbx, opts.LockString, opts.User, client)
	if err != nil {
		log.Error(err, "failed to lock sandbox")
		return
	}
	metrics.Total += metrics.PickAndLock
	utils.ResourceVersionExpectationExpect(sbx)
	log.Info("sandbox locked", "cost", metrics.PickAndLock)
	claimed = sbx

	// Step 3: Built-in post processes. The locked sandbox must be always returned to be cleared properly.
	if opts.InplaceUpdate != nil {
		log.Info("starting to wait for inplace update to complete")
		metrics.InplaceUpdate, err = WaitForInplaceUpdate(ctx, sbx, *opts.InplaceUpdate, cache)
		if err != nil {
			log.Error(err, "failed to wait for inplace update")
			return
		}
		metrics.Total += metrics.InplaceUpdate
		log.Info("inplace update completed", "cost", metrics.InplaceUpdate)
	}

	if opts.InitRuntime != nil {
		log.Info("starting to init runtime", "opts", opts.InitRuntime)
		metrics.InitRuntime, err = InitRuntime(ctx, sbx, *opts.InitRuntime)
		if err != nil {
			log.Error(err, "failed to init runtime")
			return
		}
		metrics.Total += metrics.InitRuntime
		log.Info("runtime inited", "cost", metrics.InitRuntime)
	}

	if opts.CSIMount != nil {
		log.Info("starting to perform csi mount")
		metrics.CSIMount, err = CSIMount(ctx, sbx, *opts.CSIMount)
		if err != nil {
			log.Error(err, "failed to perform csi mount")
			return
		}
		metrics.Total += metrics.CSIMount
		log.Info("csi mount completed", "cost", metrics.CSIMount)
	}

	return
}

func ClearFailedSandbox(ctx context.Context, sbx infra.Sandbox, err error, reserve bool) {
	if err == nil {
		return // success, no need to clear
	}
	log := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(sbx))
	if reserve {
		log.Info("the locked sandbox is reserved for debugging")
	} else {
		log.Info("the locked sandbox will be deleted")
		if err := sbx.Kill(ctx); err != nil {
			log.Error(err, "failed to delete locked sandbox")
		} else {
			log.Info("sandbox deleted")
		}
	}
}
