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

package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// hasActiveAutoPausePolicy returns true when Spec.AutoPausePolicy is non-nil
// and at least one of Pause / Resume is configured.
func hasActiveAutoPausePolicy(box *agentsv1alpha1.Sandbox) bool {
	policy := box.Spec.AutoPausePolicy
	if policy == nil {
		return false
	}
	if policy.Pause != nil && policy.Pause.WhenProbedIdleState != nil {
		return true
	}
	if policy.Resume != nil && policy.Resume.WhenProbedScheduleTime != nil {
		return true
	}
	return false
}

// isUnclaimedPoolSandbox returns true when the sandbox is an unclaimed pool
// sandbox (managed by SandboxSet with claimed=false). These are excluded
// from auto-pause management.
func isUnclaimedPoolSandbox(box *agentsv1alpha1.Sandbox) bool {
	return box.Labels[agentsv1alpha1.LabelSandboxIsClaimed] == "false"
}

// handleAutoPause is the main entry point for auto-pause logic.
// It is called at the end of the Reconcile loop, after calculateStatus and Ensure*.
// Phase transitions triggered by patching Spec.Paused take effect in the next reconcile.
//
// The method evaluates AutoPausePolicy in two steps:
//  1. evaluatePauseSchedule / evaluateResumeSchedule: compute next pause/resume
//     times and record them in Status.Schedules for observability
//  2. tryPause / tryResume: check whether the time has been reached and
//     patch Spec.Paused accordingly
//
// Returns (requeueAfter, error). When error is non-nil, the caller should return immediately.
func (r *SandboxReconciler) handleAutoPause(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) (time.Duration, error) {
	// Skip if sandbox is being deleted
	if !box.DeletionTimestamp.IsZero() {
		return 0, nil
	}

	// Skip unclaimed pool sandboxes (managed by SandboxSet with claimed=false)
	if isUnclaimedPoolSandbox(box) {
		return 0, nil
	}

	now := metav1.Now()
	// Evaluate and record the next pause/resume times in Schedules.
	pauseTime := r.evaluatePauseSchedule(box, newStatus)
	resumeTime := r.evaluateResumeSchedule(box, newStatus)

	switch box.Status.Phase {
	case agentsv1alpha1.SandboxRunning:
		return r.tryPause(ctx, box, newStatus, now, pauseTime)
	case agentsv1alpha1.SandboxPaused:
		return r.tryResume(ctx, box, newStatus, now, resumeTime)
	default:
		// Other phases (Pending, Upgrading, Resuming, Succeeded, Failed,
		// Recycling, Terminating) need no pause/resume decision.
		return 0, nil
	}
}

// tryPause attempts to pause the sandbox when the pause time has been reached.
// When not yet reached, it returns a requeue for the pause time.
func (r *SandboxReconciler) tryPause(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
	now metav1.Time,
	pauseTime *metav1.Time,
) (time.Duration, error) {
	if !shouldPause(pauseTime, now) {
		return requeueAfter(now, pauseTime), nil
	}

	oldPaused := box.Spec.Paused
	if err := r.patchSandboxPaused(ctx, box, true); err != nil {
		return 0, err
	}
	klog.InfoS("auto-pause: pausing sandbox", "sandbox", klog.KObj(box))
	if !oldPaused {
		rule := box.Spec.AutoPausePolicy.Pause.WhenProbedIdleState
		r.recorder.Event(box, corev1.EventTypeNormal, "AutoPaused",
			fmt.Sprintf("probe %q reported idle state for %s, threshold reached", rule.Probe, rule.ThresholdDuration.Duration))
	}
	if sched := findSchedule(newStatus, agentsv1alpha1.ScheduleReasonProbedIdle); sched != nil {
		sched.NextPauseTime = nil
	}
	return 0, nil
}

// evaluatePauseSchedule computes the next expected pause time from the pause policy's
// probed idle state and records it in newStatus.Schedules for observability. It
// returns cond.LastTransitionTime + ThresholdDuration when the agent is currently
// idle (probe succeeded and message matches MessageRegex), or nil when the sandbox
// should not be paused: no/invalid rule, probe unavailable or not succeeded
// (fail-closed), or agent active. When nil, it clears any existing NextPauseTime.
func (r *SandboxReconciler) evaluatePauseSchedule(
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) *metav1.Time {
	policy := box.Spec.AutoPausePolicy
	if policy == nil || policy.Pause == nil || policy.Pause.WhenProbedIdleState == nil {
		// No pause rule configured
		return nil
	}

	rule := policy.Pause.WhenProbedIdleState
	// Validate required fields upfront
	if rule.Probe == "" || rule.MessageRegex == "" || rule.ThresholdDuration == nil {
		r.recorder.Event(box, corev1.EventTypeWarning, "InvalidPauseRule",
			fmt.Sprintf("pause rule has missing required field(s): probe=%q, messageRegex=%q, thresholdDuration=%v",
				rule.Probe, rule.MessageRegex, rule.ThresholdDuration))
		return nil
	}

	// Compile MessageRegex upfront to catch invalid patterns early
	re, err := regexp.Compile(rule.MessageRegex)
	if err != nil {
		klog.ErrorS(err, "auto-pause: invalid messageRegex", "sandbox", klog.KObj(box), "regex", rule.MessageRegex)
		r.recorder.Event(box, corev1.EventTypeWarning, "InvalidMessageRegex",
			fmt.Sprintf("Invalid messageRegex %q: %v", rule.MessageRegex, err))
		return nil
	}

	condType := agentsv1alpha1.ProbeConditionPrefix + rule.Probe
	cond := utils.GetSandboxCondition(newStatus, condType)
	if cond == nil {
		// Probe condition not yet available
		klog.V(3).InfoS("auto-pause: probe condition not found", "sandbox", klog.KObj(box), "probe", rule.Probe)
		return nil
	}
	// Probe not succeeded (False or Unknown) — fail-closed, treat as active
	if cond.Status != metav1.ConditionTrue {
		klog.V(3).InfoS("auto-pause: probe not succeeded, fail-closed", "sandbox", klog.KObj(box), "probe", rule.Probe, "status", cond.Status)
		return nil
	}
	// Message does not match — agent is active, clear any stale NextPauseTime.
	if !re.MatchString(cond.Message) {
		klog.V(3).InfoS("auto-pause: agent active (message does not match)", "sandbox", klog.KObj(box), "probe", rule.Probe)
		recordPauseSchedule(nil, newStatus)
		return nil
	}

	// Agent is idle — pause is expected once ThresholdDuration elapses since the
	// probe last transitioned to the idle state.
	calculatedPause := metav1.NewTime(cond.LastTransitionTime.Add(rule.ThresholdDuration.Duration))
	klog.V(3).InfoS("auto-pause: agent idle, pause scheduled", "sandbox", klog.KObj(box), "probe", rule.Probe, "pauseTime", calculatedPause)
	recordPauseSchedule(&calculatedPause, newStatus)
	return &calculatedPause
}

// recordPauseSchedule records the next expected pause time in newStatus.Schedules.
// When pauseTime is nil it clears an existing NextPauseTime on the probedIdle schedule
// without creating a new entry.
func recordPauseSchedule(pauseTime *metav1.Time, newStatus *agentsv1alpha1.SandboxStatus) {
	if pauseTime == nil {
		if sched := findSchedule(newStatus, agentsv1alpha1.ScheduleReasonProbedIdle); sched != nil {
			sched.NextPauseTime = nil
		}
		return
	}
	sched := ensureSchedule(newStatus, agentsv1alpha1.ScheduleReasonProbedIdle)
	sched.NextPauseTime = pauseTime
}

// ensureSchedule returns a pointer to the schedule entry in newStatus whose
// Reason matches the given reason, creating one if none exists. This allows
// callers to update individual fields (e.g., NextPauseTime, NextResumeTime)
// without overwriting entries belonging to a different reason.
func ensureSchedule(newStatus *agentsv1alpha1.SandboxStatus, reason string) *agentsv1alpha1.Schedule {
	for i := range newStatus.Schedules {
		if newStatus.Schedules[i].Reason == reason {
			return &newStatus.Schedules[i]
		}
	}
	newStatus.Schedules = append(newStatus.Schedules, agentsv1alpha1.Schedule{Reason: reason})
	return &newStatus.Schedules[len(newStatus.Schedules)-1]
}

// findSchedule returns a pointer to the schedule entry whose Reason matches,
// or nil when no such entry exists. Unlike ensureSchedule it does not create
// a new entry.
func findSchedule(newStatus *agentsv1alpha1.SandboxStatus, reason string) *agentsv1alpha1.Schedule {
	for i := range newStatus.Schedules {
		if newStatus.Schedules[i].Reason == reason {
			return &newStatus.Schedules[i]
		}
	}
	return nil
}

// evaluateResumeSchedule calculates the next resume time from the resume policy's
// probe condition and records it in newStatus.Schedules for observability. It
// parses the probe message as a Unix timestamp and applies lead time. Returns nil
// if no resume time can be determined. When the probe is True but the message is
// not a valid timestamp, it clears any existing NextResumeTime.
func (r *SandboxReconciler) evaluateResumeSchedule(
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) *metav1.Time {
	policy := box.Spec.AutoPausePolicy
	if policy == nil || policy.Resume == nil || policy.Resume.WhenProbedScheduleTime == nil {
		return nil
	}

	rule := policy.Resume.WhenProbedScheduleTime
	// Validate required fields upfront
	if rule.Probe == "" {
		r.recorder.Event(box, corev1.EventTypeWarning, "InvalidResumeRule",
			fmt.Sprintf("resume rule has missing required field: probe=%q", rule.Probe))
		return nil
	}
	condType := agentsv1alpha1.ProbeConditionPrefix + rule.Probe
	cond := utils.GetSandboxCondition(newStatus, condType)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		klog.V(3).InfoS("auto-pause: resume probe condition not available or not true",
			"sandbox", klog.KObj(box), "probe", rule.Probe)
		return nil
	}

	// Parse message as Unix timestamp
	timestamp, err := strconv.ParseInt(strings.TrimSpace(cond.Message), 10, 64)
	if err != nil || timestamp <= 0 {
		klog.InfoS("auto-pause: failed to parse resume probe message as unix timestamp",
			"sandbox", klog.KObj(box), "message", cond.Message)
		// Probe is True but message is not a valid timestamp — clear any stale
		// NextResumeTime so the status does not retain an obsolete schedule.
		recordResumeSchedule(nil, newStatus)
		return nil
	}

	resumedAt := time.Unix(timestamp, 0)
	// LeadTime defaults to 5m via CRD defaulter (+kubebuilder:default="5m").
	resumedAt = resumedAt.Add(-rule.LeadTime.Duration)

	calculatedResume := metav1.NewTime(resumedAt)
	recordResumeSchedule(&calculatedResume, newStatus)
	return &calculatedResume
}

// recordResumeSchedule records the next expected resume time in newStatus.Schedules.
// When resumeTime is nil it clears an existing NextResumeTime on the probedSchedule schedule
// without creating a new entry.
func recordResumeSchedule(resumeTime *metav1.Time, newStatus *agentsv1alpha1.SandboxStatus) {
	if resumeTime == nil {
		if sched := findSchedule(newStatus, agentsv1alpha1.ScheduleReasonProbedSchedule); sched != nil {
			sched.NextResumeTime = nil
		}
		return
	}
	sched := ensureSchedule(newStatus, agentsv1alpha1.ScheduleReasonProbedSchedule)
	sched.NextResumeTime = resumeTime
}

// shouldPause returns true when the sandbox should be paused, i.e., when
// pauseTime is non-nil and the current time has reached or passed it.
func shouldPause(pauseTime *metav1.Time, now metav1.Time) bool {
	return pauseTime != nil && !now.Before(pauseTime)
}

// shouldResume returns true when the sandbox should be resumed, i.e., when
// resumeTime is non-nil and the current time has reached or passed it.
func shouldResume(resumeTime *metav1.Time, now metav1.Time) bool {
	return resumeTime != nil && !now.Before(resumeTime)
}

// requeueAfter returns the duration until the earliest non-nil future time.
// Returns 0 when all times are nil or already in the past (no requeue needed).
func requeueAfter(now metav1.Time, times ...*metav1.Time) time.Duration {
	var result time.Duration
	for _, t := range times {
		if t == nil {
			continue
		}
		remaining := t.Sub(now.Time)
		if remaining <= 0 {
			continue
		}
		if result == 0 || remaining < result {
			result = remaining
		}
	}
	return result
}

// tryResume attempts to resume the sandbox when the resume time has been reached.
// When not yet reached, it returns a requeue for the resume time.
func (r *SandboxReconciler) tryResume(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
	now metav1.Time,
	resumeTime *metav1.Time,
) (time.Duration, error) {
	if !shouldResume(resumeTime, now) {
		return requeueAfter(now, resumeTime), nil
	}
	// Only resume when the sandbox is actually paused (condition True), not
	// just when Spec.Paused or Phase says Paused — those reflect intent, not
	// the actual pause state. Otherwise the resume time is stale and no
	// state transition is needed.
	pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
	if pausedCond == nil || pausedCond.Status != metav1.ConditionTrue {
		klog.InfoS("auto-pause: resume time reached but sandbox not paused, skipping", "sandbox", klog.KObj(box))
		return 0, nil
	}

	oldPaused := box.Spec.Paused
	if err := r.patchSandboxPaused(ctx, box, false); err != nil {
		return 0, err
	}
	klog.InfoS("auto-pause: resuming sandbox", "sandbox", klog.KObj(box))
	if oldPaused {
		rule := box.Spec.AutoPausePolicy.Resume.WhenProbedScheduleTime
		r.recorder.Event(box, corev1.EventTypeNormal, "AutoResumed",
			fmt.Sprintf("probe %q schedule time reached (lead time %s)", rule.Probe, rule.LeadTime.Duration))
	}
	if sched := findSchedule(newStatus, agentsv1alpha1.ScheduleReasonProbedSchedule); sched != nil {
		sched.NextResumeTime = nil
	}
	return 0, nil
}

// patchSandboxPaused patches Spec.Paused via a strategic merge patch.
func (r *SandboxReconciler) patchSandboxPaused(ctx context.Context, box *agentsv1alpha1.Sandbox, paused bool) error {
	if box.Spec.Paused == paused {
		return nil
	}

	patchData := fmt.Sprintf(`{"spec":{"paused":%v}}`, paused)
	rcvObject := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: box.Namespace, Name: box.Name}}
	if err := r.Patch(ctx, rcvObject, client.RawPatch(types.MergePatchType, []byte(patchData))); err != nil {
		return fmt.Errorf("failed to patch sandbox paused=%v: %w", paused, err)
	}

	klog.InfoS("auto-pause: patched sandbox paused", "sandbox", klog.KObj(box), "paused", paused)
	// Update the local copy so subsequent logic sees the change
	box.Spec.Paused = paused
	return nil
}
