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

package sandboxset

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	intstrutil "k8s.io/apimachinery/pkg/util/intstr"

	logf "sigs.k8s.io/controller-runtime/pkg/log"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

const (
	scalingLimitedReasonBudgetAvailable = "StartupBudgetAvailable"
	scalingLimitedReasonBudgetExhausted = "StartupBudgetExhausted"
	eventScalingLimited                 = "ScalingLimited"
)

// calculateScalingLimited updates the ScalingLimited condition on newStatus
// and returns the earliest next-deadline requeue. maxUnavailable is validated
// by the admission webhook, so intstr resolution errors are not expected here
// and callers do not need to handle them.
func (r *Reconciler) calculateScalingLimited(
	ctx context.Context,
	sbs *agentsv1alpha1.SandboxSet,
	status *agentsv1alpha1.SandboxSetStatus,
	groups GroupedSandboxes,
	now time.Time,
) time.Duration {
	failed, timedOut, nextDeadline := countStartupBlocked(groups, r.sbxMaxPendingTimeout, now)

	startupBudget := resolveStartupBudget(ctx, sbs.Spec.ScaleStrategy.MaxUnavailable, status.Replicas)
	blocked := failed + timedOut
	limited := blocked >= startupBudget
	reason := scalingLimitedReasonBudgetAvailable
	conditionStatus := metav1.ConditionFalse
	if limited {
		reason = scalingLimitedReasonBudgetExhausted
		conditionStatus = metav1.ConditionTrue
	}

	previous := apiMeta.FindStatusCondition(sbs.Status.Conditions, string(agentsv1alpha1.SandboxSetConditionScalingLimited))
	apiMeta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               string(agentsv1alpha1.SandboxSetConditionScalingLimited),
		Status:             conditionStatus,
		ObservedGeneration: sbs.Generation,
		Reason:             reason,
		Message:            fmt.Sprintf("%d of %d startup slots are blocked: Timeout=%d, Failed=%d", blocked, startupBudget, timedOut, failed),
	})
	if limited && (previous == nil || previous.Status != metav1.ConditionTrue) {
		r.Recorder.Eventf(sbs, corev1.EventTypeWarning, eventScalingLimited,
			"SandboxSet startup budget is exhausted: Timeout=%d, Failed=%d, Budget=%d", timedOut, failed, startupBudget)
	}

	if nextDeadline.IsZero() {
		return 0
	}
	return max(nextDeadline.Sub(now), 0)
}

// countStartupBlocked returns the number of sandboxes that occupy the startup
// budget: those whose Ready condition reports a definitive startup failure,
// and those still stuck in Creating/ResourcePending past sbxMaxPendingTimeout. It
// also reports the earliest future deadline among still-pending sandboxes so
// the caller can requeue at the right time. This drives the ScalingLimited
// condition; scale-up delta uses replicas-minus-available separately.
func countStartupBlocked(groups GroupedSandboxes, sbxMaxPendingTimeout time.Duration, now time.Time) (failed, timedOut int, nextDeadline time.Time) {
	for _, sandbox := range groups.Creating {
		if isStartupFailure(sandbox) {
			failed++
			continue
		}
		state, reason := utils.GetSandboxState(sandbox)
		if state != agentsv1alpha1.SandboxStateCreating || reason != agentsv1alpha1.SandboxStateReasonResourcePending {
			continue
		}
		deadline := sandbox.CreationTimestamp.Add(sbxMaxPendingTimeout)
		if !now.Before(deadline) {
			timedOut++
		} else if nextDeadline.IsZero() || deadline.Before(nextDeadline) {
			nextDeadline = deadline
		}
	}
	return failed, timedOut, nextDeadline
}

func isStartupFailure(sandbox *agentsv1alpha1.Sandbox) bool {
	condition := apiMeta.FindStatusCondition(sandbox.Status.Conditions, string(agentsv1alpha1.SandboxConditionReady))
	if condition == nil || condition.Status != metav1.ConditionFalse {
		return false
	}
	return condition.Reason == agentsv1alpha1.SandboxReadyReasonStartContainerFailed ||
		condition.Reason == agentsv1alpha1.SandboxReadyReasonPodCreateFailed
}

// resolveStartupBudget computes the startup budget used by the
// ScalingLimited condition. Admission has already vetted maxUnavailable, so
// intstr resolution is not expected to fail here; if it ever does the error
// is logged (rather than swallowed) and we fall back to a budget of one so
// the pool can still make forward progress.
func resolveStartupBudget(ctx context.Context, maxUnavailable *intstrutil.IntOrString, observedReplicas int32) int {
	executionBase := max(int(observedReplicas), 1)
	if maxUnavailable == nil {
		return executionBase
	}
	resolved, err := intstrutil.GetScaledValueFromIntOrPercent(maxUnavailable, executionBase, true)
	if err != nil {
		logf.FromContext(ctx).Error(err, "unexpected error resolving maxUnavailable for startup budget; admission webhook should have rejected this value",
			"maxUnavailable", maxUnavailable.String(), "observedReplicas", observedReplicas)
		return 1
	}
	return max(resolved, 1)
}

// resolveMaxUnavailable resolves MaxUnavailable against the supplied base.
// Callers pass Spec.Replicas when sizing physical scale-up steps. An absent or
// unresolvable value falls back to the base itself (no cap), which is
// equivalent to a default of 100%.
func resolveMaxUnavailable(ctx context.Context, maxUnavailable *intstrutil.IntOrString, base int32) int {
	fallback := max(int(base), 1)
	if maxUnavailable == nil {
		return fallback
	}
	resolved, err := intstrutil.GetScaledValueFromIntOrPercent(maxUnavailable, fallback, true)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to resolve maxUnavailable for scale delta; using default",
			"maxUnavailable", maxUnavailable.String(), "base", base)
		return fallback
	}
	return max(resolved, 0)
}

func minimumPositiveDuration(durations ...time.Duration) time.Duration {
	var earliest time.Duration
	for _, duration := range durations {
		if duration > 0 && (earliest == 0 || duration < earliest) {
			earliest = duration
		}
	}
	return earliest
}
