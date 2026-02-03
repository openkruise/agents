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
	"fmt"
	"sync"
	"time"

	"github.com/openkruise/agents/pkg/utils"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
)

type commonControl struct {
	client.Client
	recorder      record.EventRecorder
	sandboxClient clients.SandboxClient
	cache         *sandboxcr.Cache
	pickCache     sync.Map
}

func NewCommonControl(c client.Client, recorder record.EventRecorder, sandboxClient clients.SandboxClient, cache *sandboxcr.Cache) ClaimControl {
	// Note: sandboxClient and cache can be nil for unit tests
	// In production, SetupWithManager always provides these dependencies

	control := &commonControl{
		Client:        c,
		recorder:      recorder,
		sandboxClient: sandboxClient,
		cache:         cache,
		pickCache:     sync.Map{},
	}

	return control
}

// EnsureClaimClaiming handles the logic for claiming sandboxes
func (c *commonControl) EnsureClaimClaiming(ctx context.Context, args ClaimArgs) (RequeueStrategy, error) {
	log := logf.FromContext(ctx)
	claim, sandboxSet := args.Claim, args.SandboxSet

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
		log.Info("Status count mismatch, using actual count",
			"statusCount", statusCount,
			"actualCount", actualCount)
		currentCount = actualCount
	}

	// Step 5: Update status with current count
	args.NewStatus.ClaimedReplicas = currentCount

	// Step 6: Check if already completed
	if currentCount >= desiredReplicas {
		log.Info("All replicas claimed",
			"claimed", currentCount,
			"desired", desiredReplicas)
		c.recorder.Event(claim, "Normal", "ClaimCompleted",
			fmt.Sprintf("Successfully claimed %d/%d sandboxes", currentCount, desiredReplicas))
		args.NewStatus.Message = fmt.Sprintf("Completed: %d/%d claimed", currentCount, desiredReplicas)
		// Requeue immediately to transition to Completed phase
		return RequeueImmediately(), nil
	}

	// Step 7: Calculate batch size
	remaining := desiredReplicas - currentCount
	batchSize := min(int(remaining), MaxClaimBatchSize)

	// Step 8: Perform claim
	claimed, err := c.claimSandboxes(ctx, claim, sandboxSet, batchSize)
	if err != nil {
		log.Error(err, "Claim attempts completed with errors",
			"claimed", claimed, "attempted", batchSize)
	}

	// Step 9: Update final count and status
	finalCount := currentCount + int32(claimed)
	args.NewStatus.ClaimedReplicas = finalCount
	args.NewStatus.Message = fmt.Sprintf("Claiming sandboxes: %d/%d claimed", finalCount, desiredReplicas)

	// Step 10: Record results and determine requeue strategy
	if claimed > 0 {
		log.Info("Claimed sandboxes in this cycle",
			"claimed", claimed,
			"total", finalCount,
			"desired", desiredReplicas)
		c.recorder.Event(claim, "Normal", "SandboxClaimed",
			fmt.Sprintf("Claimed %d sandbox(es), total: %d/%d", claimed, finalCount, desiredReplicas))
		// Made progress, requeue immediately to continue claiming
		return RequeueImmediately(), nil
	}

	// No progress - no available sandboxes
	log.Info("No available sandboxes, will retry",
		"retryInterval", ClaimRetryInterval)
	c.recorder.Event(claim, "Warning", "NoAvailableSandboxes",
		fmt.Sprintf("No available sandboxes in pool %s", sandboxSet.Name))
	// Retry after interval to avoid busy loop
	return RequeueAfter(ClaimRetryInterval), nil
}

// EnsureClaimCompleted handles claim in Completed phase
func (c *commonControl) EnsureClaimCompleted(ctx context.Context, args ClaimArgs) (RequeueStrategy, error) {
	log := logf.FromContext(ctx)
	claim := args.Claim

	log.V(1).Info("EnsureClaimCompleted called", "phase", args.NewStatus.Phase)

	// Check if TTL cleanup is needed
	if claim.Spec.TTLAfterCompleted != nil && args.NewStatus.CompletionTime != nil {
		ttl := claim.Spec.TTLAfterCompleted.Duration
		elapsed := time.Since(args.NewStatus.CompletionTime.Time)

		log.Info("Checking TTL for cleanup", "ttl", ttl, "elapsed", elapsed, "completionTime", args.NewStatus.CompletionTime.Time)

		// Check if TTL expired
		if elapsed >= ttl {
			log.Info("TTL expired, deleting SandboxClaim", "ttl", ttl, "elapsed", elapsed)
			c.recorder.Event(claim, "Normal", "SandboxClaimTTLDelete", fmt.Sprintf("Deleting SandboxClaim after TTL of %v", ttl))
			if err := c.Delete(ctx, claim); err != nil {
				log.Error(err, "failed to delete SandboxClaim")
				// Return error to trigger exponential backoff retry
				return NoRequeue(), err
			}

			log.Info("SandboxClaim deleted successfully due to TTL expiration")
			return NoRequeue(), nil
		}

		// TTL not yet expired, calculate remaining time
		remaining := ttl - elapsed
		log.V(1).Info("TTL not yet expired, will requeue", "remaining", remaining)
		return RequeueAfter(remaining), nil
	}

	// No TTL configured, no need to requeue
	log.V(1).Info("No TTL cleanup configured", "hasTTL", claim.Spec.TTLAfterCompleted != nil, "hasCompletionTime", args.NewStatus.CompletionTime != nil)
	return NoRequeue(), nil
}

// claimSandboxes attempts to claim up to batchSize sandboxes from the pool
func (c *commonControl) claimSandboxes(ctx context.Context, claim *agentsv1alpha1.SandboxClaim, sandboxSet *agentsv1alpha1.SandboxSet, batchSize int) (int, error) {
	log := logf.FromContext(ctx)

	// Validate and build claim options
	opts, err := c.buildClaimOptions(ctx, claim, sandboxSet)
	if err != nil {
		return 0, fmt.Errorf("failed to build claim options: %w", err)
	}

	// Attempt to claim sandboxes concurrently using DoItSlowly
	claimedCount, err := utils.DoItSlowly(batchSize, InitialClaimBatchSize, func() error {
		// Pass nil for rand so sandboxcr uses global rand (concurrent-safe).
		sbx, metrics, claimErr := sandboxcr.TryClaimSandbox(ctx, opts, &c.pickCache, c.cache, c.sandboxClient)
		if claimErr != nil {
			log.Error(claimErr, "Failed to claim sandbox")
			return claimErr
		}

		log.Info("Successfully claimed sandbox",
			"sandbox", sbx.GetName(),
			"totalCost", metrics.Total,
			"pickAndLock", metrics.PickAndLock,
			"initRuntime", metrics.InitRuntime)
		return nil
	})

	if claimedCount > 0 {
		log.Info("Claimed sandboxes successfully", "count", claimedCount, "attempted", batchSize)
	}

	return claimedCount, err
}

// buildClaimOptions constructs ClaimSandboxOptions for TryClaimSandbox
func (c *commonControl) buildClaimOptions(ctx context.Context, claim *agentsv1alpha1.SandboxClaim, sandboxSet *agentsv1alpha1.SandboxSet) (infra.ClaimSandboxOptions, error) {
	opts := infra.ClaimSandboxOptions{
		User:     string(claim.UID), // Use UID to ensure uniqueness across claim recreations
		Template: sandboxSet.Name,
		Modifier: func(sbx infra.Sandbox) {
			// 1. apply labels
			labels := sbx.GetLabels()
			if labels == nil {
				labels = make(map[string]string)
			}
			labels[agentsv1alpha1.LabelSandboxClaimName] = claim.Name

			for k, v := range claim.Spec.Labels {
				labels[k] = v
			}
			sbx.SetLabels(labels)

			// 2. apply annotations
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

			// 3. apply shutdownTime
			if claim.Spec.ShutdownTime != nil {
				sbx.SetTimeout(infra.TimeoutOptions{
					ShutdownTime: claim.Spec.ShutdownTime.Time,
				})
			}
		},
	}

	// todo support other options (like envvars, inplace update...)

	// Validate and initialize
	return sandboxcr.ValidateAndInitClaimOptions(opts)
}

// countClaimedSandboxes counts sandboxes that are claimed by this claim
func (c *commonControl) countClaimedSandboxes(ctx context.Context, claim *agentsv1alpha1.SandboxClaim) (int32, error) {
	sandboxes, err := c.cache.ListSandboxWithUser(string(claim.UID))
	if err != nil {
		return 0, err
	}

	return int32(len(sandboxes)), nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
