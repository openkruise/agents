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

package poolautoscaler

import (
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// ReasonNoCronTriggered is the reason returned when no cron policy has triggered.
const ReasonNoCronTriggered = "no cron policy has triggered yet"

// cronPolicyResult holds the evaluation result of a single cron policy.
type cronPolicyResult struct {
	name           string
	targetReplicas int32
	triggerTime    time.Time
}

// evaluateCronPolicies determines the desired replicas from cron policies.
//
// Unlike the previous implementation which looked back 24 hours and retroactively
// applied past schedules, this version only reacts to NEW triggers — schedules that
// fired AFTER the last recorded execution time. This matches CronJob semantics:
// creating or editing a PoolAutoscaler does not retroactively change replicas.
//
// The baseline for each policy is its lastScheduleTime in status. For a newly created
// policy (no status yet), the PoolAutoscaler's creationTimestamp is used as baseline,
// so past schedules before creation are ignored.
func evaluateCronPolicies(
	policies []agentsv1alpha1.CronScalingPolicy,
	now time.Time,
	existingStatus []agentsv1alpha1.CronScalingPolicyStatus,
) (int32, string, []agentsv1alpha1.CronScalingPolicyStatus, error) {
	if len(policies) == 0 {
		return 0, "", nil, fmt.Errorf("no cron policies configured")
	}

	var mostRecent *cronPolicyResult
	appliedStatuses := make([]agentsv1alpha1.CronScalingPolicyStatus, 0, len(policies))

	for i := range policies {
		policy := &policies[i]

		loc := time.Local
		if policy.TimeZone != nil && *policy.TimeZone != "" {
			var err error
			loc, err = time.LoadLocation(*policy.TimeZone)
			if err != nil {
				return 0, "", nil, fmt.Errorf("invalid timezone %q in policy %q: %w", *policy.TimeZone, policy.Name, err)
			}
		}

		schedule, err := cronParser.Parse(policy.Schedule)
		if err != nil {
			return 0, "", nil, fmt.Errorf("invalid cron expression %q in policy %q: %w", policy.Schedule, policy.Name, err)
		}

		// Determine the baseline: only triggers AFTER this time are considered new.
		// For existing policies, use lastScheduleTime as baseline.
		// For newly added policies (no status record), use 'now - 1 minute' as baseline.
		// Cron has minute-level granularity, so subtracting 1 minute ensures the
		// current minute's trigger is captured on the first reconcile, while still
		// preventing retroactive triggers from hours/days ago.
		baseline := now.Add(-time.Minute)
		if existing := findExistingScheduleTime(policy.Name, existingStatus); existing != nil {
			baseline = existing.Time
		}

		nowInTZ := now.In(loc)
		newTrigger := findTriggerSince(schedule, baseline.In(loc), nowInTZ)

		status := agentsv1alpha1.CronScalingPolicyStatus{Name: policy.Name}
		if !newTrigger.IsZero() {
			t := metav1.NewTime(newTrigger)
			status.LastScheduleTime = &t
		} else {
			status.LastScheduleTime = findExistingScheduleTime(policy.Name, existingStatus)
		}
		appliedStatuses = append(appliedStatuses, status)

		if newTrigger.IsZero() {
			continue
		}

		if mostRecent == nil || newTrigger.After(mostRecent.triggerTime) {
			mostRecent = &cronPolicyResult{
				name:           policy.Name,
				targetReplicas: policy.TargetReplicas,
				triggerTime:    newTrigger,
			}
		}
	}

	if mostRecent == nil {
		return 0, ReasonNoCronTriggered, appliedStatuses, nil
	}

	reason := fmt.Sprintf("cron policy %q triggered at %s", mostRecent.name, mostRecent.triggerTime.Format(time.RFC3339))
	return mostRecent.targetReplicas, reason, appliedStatuses, nil
}

// findTriggerSince finds the most recent time the schedule fired in the window (since, now].
// Returns zero time if no trigger occurred in that window.
func findTriggerSince(schedule cron.Schedule, since, now time.Time) time.Time {
	var lastTrigger time.Time
	candidate := since
	for {
		next := schedule.Next(candidate)
		if next.After(now) {
			break
		}
		lastTrigger = next
		candidate = next
	}
	return lastTrigger
}

// findExistingScheduleTime looks up a policy's last schedule time from existing status.
func findExistingScheduleTime(name string, existing []agentsv1alpha1.CronScalingPolicyStatus) *metav1.Time {
	for i := range existing {
		if existing[i].Name == name {
			return existing[i].LastScheduleTime
		}
	}
	return nil
}
