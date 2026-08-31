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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/pausedretention"
	"github.com/openkruise/agents/pkg/utils"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
)

const (
	maxCompiledMessageRegexes             = 128
	defaultAutoPauseRequeueInterval       = 30 * time.Second
	autoPauseStatusPersistRequeueInterval = time.Second
)

type compiledMessageRegexCache struct {
	mu       sync.Mutex
	patterns map[string]*regexp.Regexp
}

func (c *compiledMessageRegexCache) compile(pattern string) (*regexp.Regexp, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if re := c.patterns[pattern]; re != nil {
		return re, nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return nil, err
	}
	if c.patterns == nil || len(c.patterns) >= maxCompiledMessageRegexes {
		c.patterns = make(map[string]*regexp.Regexp)
	}
	c.patterns[pattern] = re
	return re, nil
}

// autoPauseTakesOver reports whether the probe-driven auto-pause loop is the
// component that decides when this sandbox pauses.
//
// Both the feature gate and the policy must hold. A configured policy alone is
// not enough: with the gate disabled handleAutoPause never runs, so any caller
// that steps aside for "an active policy" would be yielding to a loop that will
// never act. Every such caller must ask this question, not
// hasActiveAutoPausePolicy alone.
func autoPauseTakesOver(box *agentsv1alpha1.Sandbox) bool {
	return utilfeature.DefaultFeatureGate.Enabled(features.AutoPauseControllerGate) && hasActiveAutoPausePolicy(box)
}

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
	// Running is the only phase with a live Pod that can refresh probe results.
	// Persist the resume decision before pausing; once the Pod is deleted, the
	// recorded schedule becomes the source of truth for waking the Sandbox.
	switch newStatus.Phase {
	case agentsv1alpha1.SandboxRunning:
		resumeTime := r.evaluateResumeSchedule(ctx, box, newStatus)
		if !scheduleProbeHealthy(box, newStatus) {
			// Never pause when a configured resume probe is unavailable: after
			// pausing there would be no Pod left to repair the missing decision.
			return defaultAutoPauseRequeueInterval, nil
		}
		if shouldResume(resumeTime, now) {
			// The lead window has already started. Keep the Sandbox running and
			// periodically re-evaluate until the probe publishes its next task.
			return defaultAutoPauseRequeueInterval, nil
		}
		pauseTime := r.evaluatePauseSchedule(ctx, box, newStatus)
		if shouldPause(pauseTime, now) && resumeScheduleNeedsPersistence(&box.Status, resumeTime) {
			// Persist the live resume decision before deleting the Pod. The
			// Reconcile caller writes newStatus after this returns; a short requeue
			// then observes that status before Spec.Paused is patched.
			return autoPauseStatusPersistRequeueInterval, nil
		}
		return r.tryPause(ctx, box, newStatus, now, pauseTime)
	case agentsv1alpha1.SandboxPaused:
		recordPauseSchedule(nil, newStatus)
		if !hasResumeRule(box) {
			recordResumeSchedule(nil, newStatus)
			return 0, nil
		}
		var resumeTime *metav1.Time
		if sched := findSchedule(newStatus, agentsv1alpha1.ScheduleReasonProbedSchedule); sched != nil {
			resumeTime = sched.NextResumeTime
		}
		if resumeTime == nil {
			// Recover a schedule that was not persisted together with the pause
			// patch from the last mirrored probe result, if it is still present.
			resumeTime = r.calculateResumeTime(ctx, box, newStatus)
			if resumeTime != nil {
				recordResumeSchedule(resumeTime, newStatus)
			}
		}
		if resumeTime == nil {
			pausedCond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionPaused))
			if pausedCond == nil || pausedCond.Status != metav1.ConditionTrue {
				// Pause completion can remove the Pod before the Paused condition is
				// persisted. Poll until the state settles so a schedule written by the
				// preceding Running reconcile is not lost to that transition.
				return defaultAutoPauseRequeueInterval, nil
			}
			// A fully paused Sandbox with no recorded task has no internal signal
			// that polling could discover because its Pod no longer exists. Policy
			// or manual resume updates still enqueue reconciliation.
			return 0, nil
		}
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
	if err := r.pauseSandbox(ctx, box, now); err != nil {
		return 0, err
	}
	klog.FromContext(ctx).Info("auto-pause: pausing sandbox", "sandbox", klog.KObj(box))
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

// evaluatePauseSchedule computes the next expected pause time and records it in
// newStatus.Schedules for observability.
//
// Every path records its result, including the ones that decide against a
// pause. A time recorded by an earlier reconcile must not outlive the decision
// that produced it: once the probe stops reporting idle the decision is
// fail-closed, and a leftover NextPauseTime would keep advertising a pause that
// is no longer going to happen.
func (r *SandboxReconciler) evaluatePauseSchedule(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) *metav1.Time {
	pauseTime := r.calculatePauseTime(ctx, box, newStatus)
	recordPauseSchedule(pauseTime, newStatus)
	return pauseTime
}

// calculatePauseTime returns cond.LastTransitionTime + ThresholdDuration when the
// agent is currently idle (probe succeeded and message matches MessageRegex), or
// nil when the sandbox should not be paused: no/invalid rule, a rejected probe
// configuration, probe unavailable or not succeeded (fail-closed), or agent
// active.
func (r *SandboxReconciler) calculatePauseTime(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) *metav1.Time {
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
	policy := box.Spec.AutoPausePolicy
	if policy == nil || policy.Pause == nil || policy.Pause.WhenProbedIdleState == nil {
		// No pause rule configured
		return nil
	}

	rule := policy.Pause.WhenProbedIdleState
	// Fail-closed on a rejected probe configuration.
	//
	// The case this exists for is an invalid probe: validation then refuses to
	// apply the probes to the Pod, so every probe condition left in status is
	// frozen at its last value — and a frozen LastTransitionTime always looks
	// like the threshold has elapsed. Pausing on that would turn a rejected
	// configuration into a pause the user never asked for.
	//
	// ProbeValid is an aggregate, so a policy-only error trips it too even though
	// the probes keep syncing in that case. That is deliberately not narrowed:
	// the explicit checks below already reject most policy errors, and declining
	// to pause on a policy the user has not finished fixing is the conservative
	// answer. The cost is that an error confined to the resume rule also holds
	// off the pause rule. Resume is left alone — see handleAutoPause.
	if cond := utils.GetSandboxCondition(newStatus, string(agentsv1alpha1.SandboxConditionProbeValid)); cond != nil && cond.Status == metav1.ConditionFalse {
		logger.V(3).Info("auto-pause: probe configuration invalid, fail-closed", "reason", cond.Reason)
		return nil
	}
	// Validate required fields upfront. All three are required by the CRD, so
	// reaching this branch means the object predates the current schema or was
	// assembled in-process; there is no safe default to fall back on, because
	// pausing without a threshold would skip the smoothing the rule asks for.
	if rule.Probe == "" || rule.MessageRegex == "" || rule.ThresholdDuration == nil {
		r.recorder.Event(box, corev1.EventTypeWarning, "InvalidPauseRule",
			fmt.Sprintf("pause rule has missing required field(s): probe=%q, messageRegex=%q, thresholdDuration=%v",
				rule.Probe, rule.MessageRegex, rule.ThresholdDuration))
		return nil
	}
	// A negative threshold puts the pause time before the probe reported idle, so
	// it always reads as already elapsed. Admission rejects it, and honouring it
	// here would pause immediately on a value the user cannot have meant.
	if rule.ThresholdDuration.Duration < 0 {
		r.recorder.Event(box, corev1.EventTypeWarning, "InvalidPauseRule",
			fmt.Sprintf("pause rule thresholdDuration must not be negative: %s", rule.ThresholdDuration.Duration))
		return nil
	}

	// Compile each distinct MessageRegex once per reconciler. Policies can be
	// updated dynamically because the pattern itself is the cache key, and the
	// bounded cache prevents repeated updates from growing memory indefinitely.
	re, err := r.messageRegexCache.compile(rule.MessageRegex)
	if err != nil {
		logger.Error(err, "auto-pause: invalid messageRegex", "regex", rule.MessageRegex)
		r.recorder.Event(box, corev1.EventTypeWarning, "InvalidMessageRegex",
			fmt.Sprintf("Invalid messageRegex %q: %v", rule.MessageRegex, err))
		return nil
	}

	condType := agentsv1alpha1.ProbeConditionType(rule.Probe)
	cond := utils.GetSandboxCondition(newStatus, condType)
	if cond == nil {
		// Probe condition not yet available
		logger.V(3).Info("auto-pause: probe condition not found", "probe", rule.Probe)
		return nil
	}
	// Probe not succeeded (False or Unknown) — fail-closed, treat as active
	if cond.Status != metav1.ConditionTrue {
		logger.V(3).Info("auto-pause: probe not succeeded, fail-closed", "probe", rule.Probe, "status", cond.Status)
		return nil
	}
	// Message does not match — agent is active.
	if !re.MatchString(cond.Message) {
		logger.V(3).Info("auto-pause: agent active (message does not match)", "probe", rule.Probe)
		return nil
	}

	// Agent is idle — pause is expected once ThresholdDuration elapses since the
	// probe last transitioned to the idle state.
	calculatedPause := metav1.NewTime(cond.LastTransitionTime.Add(rule.ThresholdDuration.Duration))
	logger.V(3).Info("auto-pause: agent idle, pause scheduled", "probe", rule.Probe, "pauseTime", calculatedPause)
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

func hasResumeRule(box *agentsv1alpha1.Sandbox) bool {
	policy := box.Spec.AutoPausePolicy
	return policy != nil && policy.Resume != nil && policy.Resume.WhenProbedScheduleTime != nil
}

// scheduleProbeHealthy reports whether the probe a configured resume rule reads
// has produced a usable result. A Sandbox must not be paused while this is
// false, because its Pod—and therefore the probe's only opportunity to
// recover—will be removed.
func scheduleProbeHealthy(box *agentsv1alpha1.Sandbox, status *agentsv1alpha1.SandboxStatus) bool {
	if !hasResumeRule(box) {
		return true
	}
	rule := box.Spec.AutoPausePolicy.Resume.WhenProbedScheduleTime
	if rule.Probe == "" {
		return false
	}
	cond := utils.GetSandboxCondition(status, agentsv1alpha1.ProbeConditionType(rule.Probe))
	return cond != nil && cond.Status == metav1.ConditionTrue
}

func resumeScheduleNeedsPersistence(status *agentsv1alpha1.SandboxStatus, resumeTime *metav1.Time) bool {
	sched := findSchedule(status, agentsv1alpha1.ScheduleReasonProbedSchedule)
	if sched == nil || sched.NextResumeTime == nil {
		return resumeTime != nil
	}
	return resumeTime == nil || !sched.NextResumeTime.Equal(resumeTime)
}

// evaluateResumeSchedule calculates the next resume time from the live probe
// and records it in newStatus.Schedules for observability. As in
// evaluatePauseSchedule, every path records its result so a NextResumeTime
// cannot outlive the decision that produced it.
func (r *SandboxReconciler) evaluateResumeSchedule(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) *metav1.Time {
	resumeTime := r.calculateResumeTime(ctx, box, newStatus)
	recordResumeSchedule(resumeTime, newStatus)
	return resumeTime
}

// calculateResumeTime parses the resume probe's message as the schedule time the
// agent reported and subtracts the lead time. It returns nil when no resume time
// can be determined: no/invalid rule, probe unavailable or not succeeded, or an
// unparsable message.
func (r *SandboxReconciler) calculateResumeTime(
	ctx context.Context,
	box *agentsv1alpha1.Sandbox,
	newStatus *agentsv1alpha1.SandboxStatus,
) *metav1.Time {
	logger := klog.FromContext(ctx).WithValues("sandbox", klog.KObj(box))
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
	condType := agentsv1alpha1.ProbeConditionType(rule.Probe)
	cond := utils.GetSandboxCondition(newStatus, condType)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		logger.V(3).Info("auto-pause: resume probe condition not available or not true", "probe", rule.Probe)
		return nil
	}

	// Parse the message according to the rule's TimeFormat.
	//
	// An unparsable message is a steady state, not an incident: a resume probe
	// reports a sentinel such as "none" whenever the agent has no upcoming task,
	// on every probe period. Logging that at the default level would flood the
	// log for every sandbox that simply has nothing scheduled, so keep it at the
	// same verbosity as the other "no decision" branches. The outcome stays
	// observable without the log: recordResumeSchedule clears NextResumeTime and
	// the probe condition keeps the raw message.
	scheduledAt, err := parseProbedScheduleTime(rule.TimeFormat, cond.Message)
	if err != nil {
		logger.V(3).Info("auto-pause: failed to parse resume probe message",
			"timeFormat", rule.TimeFormat, "message", cond.Message, "err", err)
		return nil
	}

	calculatedResume := metav1.NewTime(scheduledAt.Add(-resumeLeadTime(rule)))
	return &calculatedResume
}

// parseProbedScheduleTime parses a resume probe's message into the schedule time
// it reports, following the rule's TimeFormat.
//
// An empty format means unix: +kubebuilder:default is applied by the apiserver at
// write time only, so an object written before the field existed, or one
// assembled in-process, can reach the controller with TimeFormat unset.
func parseProbedScheduleTime(format, message string) (time.Time, error) {
	message = strings.TrimSpace(message)
	switch format {
	case agentsv1alpha1.ProbeTimeFormatDatetime:
		return time.Parse(time.RFC3339, message)
	case agentsv1alpha1.ProbeTimeFormatUnix, "":
		seconds, err := strconv.ParseInt(message, 10, 64)
		if err != nil {
			return time.Time{}, err
		}
		if seconds <= 0 {
			return time.Time{}, fmt.Errorf("unix timestamp %d is not positive", seconds)
		}
		return time.Unix(seconds, 0), nil
	default:
		return time.Time{}, fmt.Errorf("unsupported timeFormat %q", format)
	}
}

// defaultResumeLeadTime mirrors the CRD default for
// ProbedScheduleTimeRule.LeadTime.
const defaultResumeLeadTime = 5 * time.Minute

// resumeLeadTime returns the rule's usable lead time, falling back to the CRD default
// when the field is unset, for the same reason parseProbedScheduleTime accepts an
// empty TimeFormat.
//
// A negative lead time would resume *after* the task it exists to wake up for,
// which is never what the field means. Admission rejects it, so treat it as
// unset here rather than honouring it on an object written without admission.
func resumeLeadTime(rule *agentsv1alpha1.ProbedScheduleTimeRule) time.Duration {
	if rule.LeadTime == nil || rule.LeadTime.Duration < 0 {
		return defaultResumeLeadTime
	}
	return rule.LeadTime.Duration
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

// resetProbeDrivenState drops every probe condition and clears every
// probe-driven schedule time from the status. Pausing deletes the pod, which
// stops the probes, so after a resume these values describe a pod that no
// longer exists.
//
// Auto-pause measures its idle threshold from the probe condition's
// LastTransitionTime, so a leftover idle condition makes the threshold look
// long expired and pauses the sandbox again right after it resumes, before the
// new pod's probes have reported even once. A leftover resume timestamp stays
// in the past and keeps asking for a resume, so the two decisions fight each
// other every reconcile.
//
// Removing the conditions stops both: evaluatePauseSchedule and
// evaluateResumeSchedule treat a missing condition as "no decision" and leave
// the sandbox alone (fail-closed), while syncConditions re-adds each probe
// condition as Unknown until the new pod reports its first result.
func resetProbeDrivenState(newStatus *agentsv1alpha1.SandboxStatus) {
	var probeConds []string
	for _, cond := range newStatus.Conditions {
		if strings.HasPrefix(cond.Type, agentsv1alpha1.ProbeConditionPrefix) {
			probeConds = append(probeConds, cond.Type)
		}
	}
	for _, condType := range probeConds {
		utils.RemoveSandboxCondition(newStatus, condType)
	}
	// Every schedule time is derived from a probe condition, so no time outlives
	// the probe reset. Clear the times in place instead of dropping the whole
	// slice: a nil or empty slice is omitted by omitempty, and JSON Merge Patch
	// treats an absent field as unchanged, which would leave the stale times in
	// etcd.
	for i := range newStatus.Schedules {
		newStatus.Schedules[i].NextPauseTime = nil
		newStatus.Schedules[i].NextResumeTime = nil
	}
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

// tryResume attempts to resume the sandbox when the recorded resume time has been reached.
// When not yet reached, it returns a requeue for the resume time. The caller
// provides a bounded fallback requeue when no schedule is available.
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
		klog.FromContext(ctx).Info("auto-pause: resume time reached but sandbox not paused, waiting", "sandbox", klog.KObj(box))
		return defaultAutoPauseRequeueInterval, nil
	}

	oldPaused := box.Spec.Paused
	if err := r.patchSandboxPaused(ctx, box, false); err != nil {
		return 0, err
	}
	klog.FromContext(ctx).Info("auto-pause: resuming sandbox", "sandbox", klog.KObj(box))
	if oldPaused && hasResumeRule(box) {
		rule := box.Spec.AutoPausePolicy.Resume.WhenProbedScheduleTime
		r.recorder.Event(box, corev1.EventTypeNormal, "AutoResumed",
			fmt.Sprintf("probe %q schedule time reached (lead time %s)", rule.Probe, resumeLeadTime(rule)))
	}
	if sched := findSchedule(newStatus, agentsv1alpha1.ScheduleReasonProbedSchedule); sched != nil {
		sched.NextResumeTime = nil
	}
	return 0, nil
}

// pauseSandbox sets Spec.Paused and, for a sandbox under a paused-retention
// policy, extends ShutdownTime so the sandbox is preserved for the configured
// duration after being paused.
//
// Probe-driven auto-pause owns the pause decision and therefore extends a
// managed ShutdownTime itself when the pause actually fires. Until then,
// ShutdownTime remains the hard lifetime bound; handleShutdownTimeout does not
// defer deletion waiting for a probe that may never report idle.
func (r *SandboxReconciler) pauseSandbox(ctx context.Context, box *agentsv1alpha1.Sandbox, now metav1.Time) error {
	if box.Spec.Paused {
		return nil
	}

	modified := box.DeepCopy()
	modified.Spec.Paused = true
	var newShutdown *metav1.Time
	if retention, managed := r.resolveRetentionAnnotationOrDefault(ctx, box); managed && box.Spec.ShutdownTime != nil {
		extended := metav1.NewTime(pausedretention.PausedShutdownTime(now.Time, retention))
		newShutdown = &extended
		modified.Spec.ShutdownTime = &extended
		// Keep PauseTime aligned so the next connect/resume can preserve auto-pause mode.
		modified.Spec.PauseTime = &extended
	}

	patch := client.MergeFromWithOptions(box, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, modified, patch); err != nil {
		return fmt.Errorf("failed to patch sandbox paused=true: %w", err)
	}

	logger := klog.FromContext(ctx)
	logger.Info("auto-pause: patched sandbox paused", "sandbox", klog.KObj(box), "paused", true)
	// Update the local copy so subsequent logic sees the change.
	box.Spec.Paused = true
	if newShutdown != nil {
		logger.Info("auto-pause: extended shutdown time for paused retention",
			"sandbox", klog.KObj(box), "shutdownTime", *newShutdown)
		box.Spec.ShutdownTime = newShutdown
		box.Spec.PauseTime = newShutdown
	}
	return nil
}

// patchSandboxPaused patches Spec.Paused with optimistic locking.
func (r *SandboxReconciler) patchSandboxPaused(ctx context.Context, box *agentsv1alpha1.Sandbox, paused bool) error {
	if box.Spec.Paused == paused {
		return nil
	}

	modified := box.DeepCopy()
	modified.Spec.Paused = paused
	patch := client.MergeFromWithOptions(box, client.MergeFromWithOptimisticLock{})
	if err := r.Patch(ctx, modified, patch); err != nil {
		return fmt.Errorf("failed to patch sandbox paused=%v: %w", paused, err)
	}

	klog.FromContext(ctx).Info("auto-pause: patched sandbox paused", "sandbox", klog.KObj(box), "paused", paused)
	// Update the local copy so subsequent logic sees the change
	box.Spec.Paused = paused
	return nil
}
