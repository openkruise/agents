/*
Copyright 2025.

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

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/common"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/controller/events"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/csiutils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

type commonControl struct {
	client.Client
	recorder        record.EventRecorder
	cache           cache.Provider
	storageRegistry storages.VolumeMountProviderRegistry
	pickCache       sync.Map
}

func NewCommonControl(c client.Client, recorder record.EventRecorder, cache cache.Provider) ClaimControl {
	// Note: sandboxClient and cache can be nil for unit tests
	// In production, SetupWithManager always provides these dependencies

	control := &commonControl{
		Client:          c,
		recorder:        recorder,
		cache:           cache,
		storageRegistry: storages.NewStorageProvider(),
		pickCache:       sync.Map{},
	}

	return control
}

// EnsureClaimClaiming handles the logic for claiming sandboxes
func (c *commonControl) EnsureClaimClaiming(ctx context.Context, args ClaimArgs) (RequeueStrategy, error) {
	claim, sandboxSet := args.Claim, args.SandboxSet

	// Only emit binding event when claim first enters Claiming phase
	if claim.Status.Phase != agentsv1alpha1.SandboxClaimPhaseClaiming {
		c.recorder.Eventf(claim, corev1.EventTypeNormal, events.SandboxClaimBinding,
			"SandboxClaim %s starts binding sandboxes from pool %s", claim.Name, sandboxSet.Name)
		klog.InfoS("SandboxClaim starts binding sandboxes", "sandboxClaim", klog.KObj(claim), "pool", sandboxSet.Name)
	}

	// Step 1: Get desired replicas
	desiredReplicas := getDesiredReplicas(claim)

	// Step 2: Get current count from status
	statusCount := claim.Status.ClaimedReplicas

	// Step 3: Recovery logic - query actual count to prevent loss
	// This handles edge cases:
	// - Controller crashes after claiming but before status update
	// - Status update fails due to network issues
	// TODO: Known edge case - if the following sequence happens:
	//   1. Sandboxes are successfully claimed
	//   2. Controller crashes before status update
	//   3. User manually deletes some claimed sandboxes
	//   4. Controller restarts
	//   Then the controller will create new sandboxes to reach the desired replicas,
	//   even though the user intentionally deleted them, it's an extremely rare case.
	actualCount, err := c.countClaimedSandboxes(ctx, claim)
	if err != nil {
		return NoRequeue(), fmt.Errorf("failed to count claimed sandboxes: %w", err)
	}

	// Step 4: Use max(statusCount, actualCount) to get current count
	currentCount := statusCount
	if actualCount > currentCount {
		klog.InfoS("Status count mismatch, using actual count",
			"sandboxClaim", klog.KObj(claim),
			"statusCount", statusCount,
			"actualCount", actualCount)
		currentCount = actualCount
	}

	// Step 5: Update status with current count
	args.NewStatus.ClaimedReplicas = currentCount

	// Step 6: Check if already completed
	if currentCount >= desiredReplicas {
		klog.InfoS("All replicas claimed",
			"sandboxClaim", klog.KObj(claim),
			"claimed", currentCount,
			"desired", desiredReplicas)
		c.recorder.Event(claim, corev1.EventTypeNormal, events.SandboxClaimCompleted,
			fmt.Sprintf("Successfully claimed %d/%d sandboxes", currentCount, desiredReplicas))
		args.NewStatus.Message = fmt.Sprintf("Completed: %d/%d claimed", currentCount, desiredReplicas)
		// Requeue immediately to transition to Completed phase
		return RequeueImmediately(), nil
	}

	// Step 7: Precondition
	if claim.Spec.InplaceUpdate != nil {
		if res := claim.Spec.InplaceUpdate.Resources; res != nil && (len(res.Requests) > 0 || len(res.Limits) > 0) {
			if !utilfeature.DefaultFeatureGate.Enabled(features.SandboxInPlaceResourceResizeGate) {
				msg := fmt.Sprintf("in-place resource resize is disabled by feature gate %s", features.SandboxInPlaceResourceResizeGate)
				klog.InfoS("Feature gate disabled for in-place resource resize", "sandboxClaim", klog.KObj(claim), "featureGate", features.SandboxInPlaceResourceResizeGate)
				c.recorder.Event(claim, corev1.EventTypeWarning, events.FeatureGateDisabled, msg)
				TransitionToCompleted(args.NewStatus, "FeatureGateDisabled", msg)
				return NoRequeue(), nil
			}
		}
	}

	// Step 8: Calculate batch size
	remaining := desiredReplicas - currentCount
	batchSize := min(int(remaining), MaxClaimBatchSize)

	// Step 8: Perform claim
	claimed, err := c.claimSandboxes(ctx, claim, sandboxSet, batchSize)
	if err != nil {
		klog.ErrorS(err, "Claim attempts completed with errors",
			"sandboxClaim", klog.KObj(claim),
			"claimed", claimed, "attempted", batchSize)
	}

	// Step 9: Update final count and status
	finalCount := currentCount + int32(claimed)
	args.NewStatus.ClaimedReplicas = finalCount
	args.NewStatus.Message = fmt.Sprintf("Claiming sandboxes: %d/%d claimed", finalCount, desiredReplicas)

	// Step 10: Record results and determine requeue strategy
	if claimed > 0 {
		klog.InfoS("Claimed sandboxes in this cycle",
			"sandboxClaim", klog.KObj(claim),
			"claimed", claimed,
			"total", finalCount,
			"desired", desiredReplicas)
		c.recorder.Event(claim, corev1.EventTypeNormal, events.SandboxClaimed,
			fmt.Sprintf("Claimed %d sandbox(es), total: %d/%d", claimed, finalCount, desiredReplicas))
		// Made progress, requeue immediately to continue claiming
		return RequeueImmediately(), nil
	}

	// No progress - no available sandboxes
	klog.InfoS("No available sandboxes, will retry",
		"sandboxClaim", klog.KObj(claim),
		"retryInterval", ClaimRetryInterval)
	c.recorder.Event(claim, corev1.EventTypeWarning, events.NoAvailableSandboxes,
		fmt.Sprintf("No available sandboxes in pool %s", sandboxSet.Name))
	// Retry after interval to avoid busy loop
	return RequeueAfter(ClaimRetryInterval), nil
}

// EnsureClaimCompleted handles claim in Completed phase
func (c *commonControl) EnsureClaimCompleted(ctx context.Context, args ClaimArgs) (RequeueStrategy, error) {
	claim := args.Claim

	klog.V(1).InfoS("EnsureClaimCompleted called", "sandboxClaim", klog.KObj(claim), "phase", args.NewStatus.Phase)

	// Check if TTL cleanup is needed
	if claim.Spec.TTLAfterCompleted != nil && args.NewStatus.CompletionTime != nil {
		ttl := claim.Spec.TTLAfterCompleted.Duration
		// Negative TTL means never delete - skip TTL cleanup
		if ttl < 0 {
			klog.V(1).InfoS("TTL is negative, skipping automatic deletion (never delete)", "sandboxClaim", klog.KObj(claim), "ttl", ttl)
			return NoRequeue(), nil
		}
		elapsed := time.Since(args.NewStatus.CompletionTime.Time)

		klog.InfoS("Checking TTL for cleanup", "sandboxClaim", klog.KObj(claim), "ttl", ttl, "elapsed", elapsed, "completionTime", args.NewStatus.CompletionTime.Time)

		// Check if TTL expired
		if elapsed >= ttl {
			klog.InfoS("TTL expired, deleting SandboxClaim", "sandboxClaim", klog.KObj(claim), "ttl", ttl, "elapsed", elapsed)
			c.recorder.Event(claim, corev1.EventTypeNormal, events.SandboxClaimTTLDelete, fmt.Sprintf("Deleting SandboxClaim after TTL of %v", ttl))
			if err := c.Delete(ctx, claim); err != nil {
				klog.ErrorS(err, "Failed to delete SandboxClaim", "sandboxClaim", klog.KObj(claim))
				// Return error to trigger exponential backoff retry
				return NoRequeue(), err
			}

			klog.InfoS("SandboxClaim deleted successfully due to TTL expiration", "sandboxClaim", klog.KObj(claim))
			return NoRequeue(), nil
		}

		// TTL not yet expired, calculate remaining time
		remaining := ttl - elapsed
		klog.V(1).InfoS("TTL not yet expired, will requeue", "sandboxClaim", klog.KObj(claim), "remaining", remaining)
		return RequeueAfter(remaining), nil
	}

	// No TTL configured, no need to requeue
	klog.V(1).InfoS("No TTL cleanup configured", "sandboxClaim", klog.KObj(claim), "hasTTL", claim.Spec.TTLAfterCompleted != nil, "hasCompletionTime", args.NewStatus.CompletionTime != nil)
	return NoRequeue(), nil
}

// claimSandboxes attempts to claim up to batchSize sandboxes from the pool
func (c *commonControl) claimSandboxes(ctx context.Context, claim *agentsv1alpha1.SandboxClaim, sandboxSet *agentsv1alpha1.SandboxSet, batchSize int) (int, error) {
	// Validate and build claim options
	opts, err := c.buildClaimOptions(ctx, claim, sandboxSet)
	if err != nil {
		return 0, fmt.Errorf("failed to build claim options: %w", err)
	}

	claimLockChannel := make(chan struct{}, batchSize) // set to max batch size, not controlled
	limiter := rate.NewLimiter(rate.Inf, batchSize)
	// Attempt to claim sandboxes concurrently using DoItSlowly
	claimedCount, err := utils.DoItSlowly(batchSize, InitialClaimBatchSize, func() error {
		// Pass nil for rand so sandboxcr uses global rand (concurrent-safe).
		sbx, metrics, claimErr := sandboxcr.TryClaimSandbox(ctx, opts, &c.pickCache, c.cache, claimLockChannel, limiter)
		if claimErr != nil {
			klog.ErrorS(claimErr, "Failed to claim sandbox", "sandboxClaim", klog.KObj(claim))
			return claimErr
		}

		klog.InfoS("Successfully claimed sandbox",
			"sandboxClaim", klog.KObj(claim),
			"sandbox", sbx.GetName(),
			"totalCost", metrics.Total,
			"pickAndLock", metrics.PickAndLock,
			"initRuntime", metrics.InitRuntime)
		return nil
	})

	if claimedCount > 0 {
		klog.InfoS("Claimed sandboxes successfully", "sandboxClaim", klog.KObj(claim), "count", claimedCount, "attempted", batchSize)
	}

	return claimedCount, err
}

// buildClaimOptions constructs ClaimSandboxOptions for TryClaimSandbox
func (c *commonControl) buildClaimOptions(ctx context.Context, claim *agentsv1alpha1.SandboxClaim, sandboxSet *agentsv1alpha1.SandboxSet) (infra.ClaimSandboxOptions, error) {
	opts := infra.ClaimSandboxOptions{
		User:     string(claim.UID), // Use UID to ensure uniqueness across claim recreations
		Template: sandboxSet.Name,
		Modifier: func(sbx infra.Sandbox) {
			// propagate annotations to sandbox
			if len(claim.Spec.Annotations) > 0 {
				annotations := sbx.GetAnnotations()
				if annotations == nil {
					annotations = make(map[string]string)
				}
				for k, v := range claim.Spec.Annotations {
					annotations[k] = v
				}
				sbx.SetAnnotations(annotations)
			}

			// propagate labels to sandbox
			labels := sbx.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}
			labels[agentsv1alpha1.LabelSandboxClaimName] = claim.Name

			for k, v := range claim.Spec.Labels {
				labels[k] = v
			}
			sbx.SetLabels(labels)

			// propagate labels to podtemplate
			labels = sbx.GetPodLabels()
			if labels == nil {
				labels = make(map[string]string)
			}

			for k, v := range claim.Spec.Labels {
				labels[k] = v
			}
			sbx.SetPodLabels(labels)

			// apply shutdownTime
			if claim.Spec.ShutdownTime != nil {
				sbx.SetTimeout(timeout.Options{
					ShutdownTime: claim.Spec.ShutdownTime.Time,
				})
			}
		},
		ReserveFailedSandbox: claim.Spec.ReserveFailedSandbox,
		CreateOnNoStock:      claim.Spec.CreateOnNoStock,
	}

	if claim.Spec.InplaceUpdate != nil {
		opts.InplaceUpdate = &config.InplaceUpdateOptions{
			Image: claim.Spec.InplaceUpdate.Image,
		}
		if res := claim.Spec.InplaceUpdate.Resources; res != nil && (len(res.Requests) > 0 || len(res.Limits) > 0) {
			opts.InplaceUpdate.Resources = &config.InplaceUpdateResourcesOptions{
				Requests: res.Requests,
				Limits:   res.Limits,
			}
		}
	}

	if claim.Spec.WaitReadyTimeout != nil {
		opts.WaitReadyTimeout = claim.Spec.WaitReadyTimeout.Duration
	}

	if !claim.Spec.SkipInitRuntime {
		hasAgentRuntime := false
		// Check condition A: Runtimes field contains agent-runtime
		for _, rt := range sandboxSet.Spec.Runtimes {
			if rt.Name == agentsv1alpha1.RuntimeConfigForInjectAgentRuntime {
				hasAgentRuntime = true
				break
			}
		}
		// Check condition B: initContainer named "runtime"
		// TODO support sandboxTemplateRef
		if !hasAgentRuntime && sandboxSet.Spec.Template != nil {
			for _, c := range sandboxSet.Spec.Template.Spec.InitContainers {
				if c.Name == common.RuntimeInitContainerName {
					hasAgentRuntime = true
					break
				}
			}
		}

		if hasAgentRuntime {
			opts.InitRuntime = &config.InitRuntimeOptions{
				EnvVars:     claim.Spec.EnvVars,
				AccessToken: uuid.NewString(),
			}
		} else {
			klog.ErrorS(fmt.Errorf("agent-runtime not configured in SandboxSet"), "SkipInitRuntime is false but no agent-runtime found, skip InitRuntime",
				"sandboxSet", klog.KObj(sandboxSet), "sandboxClaim", klog.KObj(claim))
		}
	}
	if len(claim.Spec.DynamicVolumesMount) > 0 {
		csiMountOptions := make([]config.MountConfig, 0, len(claim.Spec.DynamicVolumesMount))
		csiClient := csiutils.NewCSIMountHandler(c.cache.GetClient(), c.cache.GetAPIReader(), c.storageRegistry, utils.DefaultSandboxDeployNamespace)
		for _, mountConfig := range claim.Spec.DynamicVolumesMount {
			driverName, csiReqConfigRaw, genErr := csiClient.CSIMountOptionsConfig(ctx, mountConfig)
			if genErr != nil {
				errMsg := "failed to generate csi mount options config for sandbox"
				klog.ErrorS(genErr, errMsg, "sandboxClaim", klog.KObj(claim), "mountConfigRequest", mountConfig)
				return opts, fmt.Errorf("%s, err: %v", errMsg, genErr)
			}
			csiMountOptions = append(csiMountOptions, config.MountConfig{
				Driver:     driverName,
				RequestRaw: csiReqConfigRaw,
			})
		}
		opts.CSIMount = &config.CSIMountOptions{
			MountOptionList: csiMountOptions,
		}

		// json marshal csi mount config to raw string
		csiMountOptionsRaw, err := json.Marshal(claim.Spec.DynamicVolumesMount)
		if err != nil {
			klog.ErrorS(err, "Failed to marshal csi mount config", "sandboxClaim", klog.KObj(claim))
			return opts, fmt.Errorf("failed to marshal csi mount config, err: %v", err)
		}
		opts.CSIMount.MountOptionListRaw = string(csiMountOptionsRaw)
	}

	if len(claim.Spec.Runtimes) > 0 {
		opts.RuntimeConfig = claim.Spec.Runtimes
	}

	// Validate and initialize
	return sandboxcr.ValidateAndInitClaimOptions(opts)
}

// countClaimedSandboxes counts sandboxes that are claimed by this claim
func (c *commonControl) countClaimedSandboxes(ctx context.Context, claim *agentsv1alpha1.SandboxClaim) (int32, error) {
	sandboxes, err := c.cache.ListSandboxes(ctx, cache.ListSandboxesOptions{
		User:      string(claim.UID),
		Namespace: claim.Namespace,
	})
	if err != nil {
		return 0, err
	}
	var cnt int32
	for _, sbx := range sandboxes {
		state, reason := stateutils.GetSandboxState(sbx)
		if state == agentsv1alpha1.SandboxStateDead {
			klog.InfoS("Skip counting dead sandbox", "sandboxClaim", klog.KObj(claim), "reason", reason)
			continue
		}
		cnt++
	}
	return cnt, nil
}
