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
	"fmt"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// CalculateClaimStatus determines the next phase of a SandboxClaim and whether to skip business logic.
//
// Purpose:
//   - Compute the next status based on current state and conditions
//   - Decide if reconciliation should skip Ensure* methods (claim/release logic)
//
// Returns:
//   - newStatus: Updated status to be persisted
//   - skipBusinessLogic: true if should skip Ensure* methods and go directly to status update
//
// Handled scenarios (in order):
//  1. Already Completed                     → Completed, continue (for TTL cleanup)
//  2. SandboxSet not found                  → Completed, SKIP (terminal, fail-fast)
//  3. New claim (Phase == "")               → Claiming, continue
//  4. All replicas claimed                  → Completed, SKIP (terminal)
//  5. Timeout exceeded                      → Completed, SKIP (terminal)
//  6. Otherwise                             → Current phase, continue
//
// Note: ObservedGeneration is always updated to track spec changes
func CalculateClaimStatus(args ClaimArgs) (*agentsv1alpha1.SandboxClaimStatus, bool) {
	claim := args.Claim
	newStatus := args.NewStatus

	// Always update ObservedGeneration to track spec changes
	newStatus.ObservedGeneration = claim.Generation

	// 1. Handle terminal state
	if newStatus.Phase == agentsv1alpha1.SandboxClaimPhaseCompleted {
		klog.V(2).InfoS("SandboxClaim already completed, skipping state calculation",
			"claim", klog.KObj(claim),
			"completionTime", newStatus.CompletionTime)
		// If already Completed, skip state calculation but allow EnsureClaimCompleted to run
		// (for TTL cleanup logic)
		return newStatus, false
	}

	// 2. Check if SandboxSet exists
	// Transition: * → Completed (SandboxSet deleted)
	if args.SandboxSet == nil {
		klog.InfoS("SandboxSet not found, transitioning to Completed",
			"claim", klog.KObj(claim),
			"sandboxSet", claim.Spec.TemplateName)
		return TransitionToCompleted(newStatus,
			"SandboxSetNotFound",
			"SandboxSet not found or deleted"), true
	}

	// 3. Handle initial state
	// Transition: "" → Claiming
	if newStatus.Phase == "" {
		klog.InfoS("Initializing new SandboxClaim, starting claim process",
			"claim", klog.KObj(claim),
			"generation", claim.Generation,
			"desiredReplicas", getDesiredReplicas(claim))
		newStatus.Phase = agentsv1alpha1.SandboxClaimPhaseClaiming
		now := metav1.Now()
		newStatus.ClaimStartTime = &now
		return newStatus, false
	}

	// 4. Check if desired replicas already met
	// Transition: Claiming → Completed (All replicas claimed)
	if isReplicasMet(claim, newStatus) {
		klog.InfoS("All replicas claimed, transitioning to Completed",
			"claim", klog.KObj(claim),
			"claimedReplicas", newStatus.ClaimedReplicas,
			"desiredReplicas", getDesiredReplicas(claim))
		return transitionToCompletedWithSuccess(newStatus, claim), true
	}

	// 5. Early timeout detection
	// Transition: Claiming → Completed (Timeout)
	if isClaimTimeout(claim, newStatus) {
		elapsed := time.Since(newStatus.ClaimStartTime.Time)
		klog.InfoS("Claim timeout reached, transitioning to Completed",
			"claim", klog.KObj(claim),
			"timeout", claim.Spec.ClaimTimeout.Duration,
			"elapsed", elapsed,
			"claimedReplicas", newStatus.ClaimedReplicas,
			"desiredReplicas", getDesiredReplicas(claim))
		return transitionToCompletedWithTimeout(newStatus, elapsed, claim), true
	}

	// Continue with business logic
	klog.V(2).InfoS("Continuing with claim business logic",
		"claim", klog.KObj(claim),
		"phase", newStatus.Phase,
		"claimedReplicas", newStatus.ClaimedReplicas,
		"desiredReplicas", getDesiredReplicas(claim))

	return newStatus, false
}

// getDesiredReplicas returns the desired number of replicas for a claim.
// Returns DefaultReplicasCount if not specified.
func getDesiredReplicas(claim *agentsv1alpha1.SandboxClaim) int32 {
	if claim.Spec.Replicas != nil {
		return *claim.Spec.Replicas
	}
	return DefaultReplicasCount
}

// isClaimTimeout checks if the claim has exceeded its timeout
func isClaimTimeout(claim *agentsv1alpha1.SandboxClaim, status *agentsv1alpha1.SandboxClaimStatus) bool {
	if claim.Spec.ClaimTimeout == nil || status.ClaimStartTime == nil {
		return false
	}
	timeout := claim.Spec.ClaimTimeout.Duration
	elapsed := time.Since(status.ClaimStartTime.Time)

	return elapsed >= timeout
}

// isReplicasMet checks if the desired number of replicas has been claimed
func isReplicasMet(claim *agentsv1alpha1.SandboxClaim, status *agentsv1alpha1.SandboxClaimStatus) bool {
	return status.ClaimedReplicas >= getDesiredReplicas(claim)
}

// TransitionToCompleted transitions the claim to Completed state with a generic reason
func TransitionToCompleted(status *agentsv1alpha1.SandboxClaimStatus, reason, message string) *agentsv1alpha1.SandboxClaimStatus {
	status.Phase = agentsv1alpha1.SandboxClaimPhaseCompleted
	status.Message = message
	now := metav1.Now()
	status.CompletionTime = &now

	condition := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxClaimConditionCompleted),
		Status:             metav1.ConditionTrue,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: now,
	}
	SetClaimCondition(status, condition)

	return status
}

// transitionToCompletedWithTimeout transitions to Completed due to timeout
func transitionToCompletedWithTimeout(status *agentsv1alpha1.SandboxClaimStatus, elapsed time.Duration, claim *agentsv1alpha1.SandboxClaim) *agentsv1alpha1.SandboxClaimStatus {
	desiredReplicas := getDesiredReplicas(claim)

	status.Phase = agentsv1alpha1.SandboxClaimPhaseCompleted
	status.Message = fmt.Sprintf("Timeout reached after %v, claimed %d/%d sandboxes",
		elapsed, status.ClaimedReplicas, desiredReplicas)
	now := metav1.Now()
	status.CompletionTime = &now

	// Set TimedOut condition
	condition := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxClaimConditionTimedOut),
		Status:             metav1.ConditionTrue,
		Reason:             "ClaimTimeoutReached",
		Message:            fmt.Sprintf("Timeout after %v, claimed %d/%d", elapsed, status.ClaimedReplicas, desiredReplicas),
		LastTransitionTime: now,
	}
	SetClaimCondition(status, condition)

	// Also set Completed condition
	completedCondition := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxClaimConditionCompleted),
		Status:             metav1.ConditionTrue,
		Reason:             "TimeoutReached",
		Message:            status.Message,
		LastTransitionTime: now,
	}
	SetClaimCondition(status, completedCondition)

	return status
}

// transitionToCompletedWithSuccess transitions to Completed after successfully claiming all replicas
func transitionToCompletedWithSuccess(status *agentsv1alpha1.SandboxClaimStatus, claim *agentsv1alpha1.SandboxClaim) *agentsv1alpha1.SandboxClaimStatus {
	desiredReplicas := getDesiredReplicas(claim)

	status.Phase = agentsv1alpha1.SandboxClaimPhaseCompleted
	status.Message = fmt.Sprintf("Successfully claimed %d/%d sandboxes", status.ClaimedReplicas, desiredReplicas)
	now := metav1.Now()
	status.CompletionTime = &now

	condition := metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxClaimConditionCompleted),
		Status:             metav1.ConditionTrue,
		Reason:             "AllReplicasClaimed",
		Message:            fmt.Sprintf("Successfully claimed all %d sandboxes", status.ClaimedReplicas),
		LastTransitionTime: now,
	}
	SetClaimCondition(status, condition)

	return status
}

// SetClaimCondition sets or updates a condition in the SandboxClaim status.
func SetClaimCondition(status *agentsv1alpha1.SandboxClaimStatus, condition metav1.Condition) {
	currentCond := GetClaimCondition(status, condition.Type)

	// Check if there's any substantive change
	if currentCond != nil && currentCond.Status == condition.Status &&
		currentCond.Reason == condition.Reason &&
		currentCond.Message == condition.Message {
		// No change needed, avoid unnecessary update
		return
	} else if currentCond == nil {
		// Add new condition
		status.Conditions = append(status.Conditions, condition)
		return
	}

	// Only update LastTransitionTime when Status changes
	if currentCond.Status != condition.Status {
		currentCond.LastTransitionTime = condition.LastTransitionTime
	}

	// Update other fields
	currentCond.Status = condition.Status
	currentCond.Reason = condition.Reason
	currentCond.Message = condition.Message
}

// GetClaimCondition returns the condition with the provided type from SandboxClaim status
func GetClaimCondition(status *agentsv1alpha1.SandboxClaimStatus, condType string) *metav1.Condition {
	for i := range status.Conditions {
		c := &status.Conditions[i]
		if c.Type == condType {
			return c
		}
	}
	return nil
}
