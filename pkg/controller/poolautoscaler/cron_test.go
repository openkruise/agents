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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestEvaluateCronPolicies(t *testing.T) {
	utc := "UTC"

	tests := []struct {
		name            string
		policies        []agentsv1alpha1.CronScalingPolicy
		now             time.Time
		existingStatus  []agentsv1alpha1.CronScalingPolicyStatus
		expectedTarget  int32
		expectedReason  string
		expectNoTrigger bool
		expectError     string
	}{
		{
			name: "newly created PA — no retroactive trigger",
			policies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "scale-up", Schedule: "0 8 * * *", TargetReplicas: 100, TimeZone: &utc},
			},
			now:             time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
			expectNoTrigger: true,
		},
		{
			name: "schedule fires after baseline",
			policies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "scale-up", Schedule: "0 8 * * *", TargetReplicas: 100, TimeZone: &utc},
			},
			now: time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
			existingStatus: []agentsv1alpha1.CronScalingPolicyStatus{
				{Name: "scale-up", LastScheduleTime: timePtr(2026, 7, 3, 7, 0, 0)},
			},
			expectedTarget: 100,
			expectedReason: "cron policy \"scale-up\" triggered",
		},
		{
			name: "two policies — only the one that fired since last check wins",
			policies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "scale-up", Schedule: "0 8 * * *", TargetReplicas: 100, TimeZone: &utc},
				{Name: "scale-down", Schedule: "0 20 * * *", TargetReplicas: 20, TimeZone: &utc},
			},
			now:            time.Date(2026, 7, 3, 21, 0, 0, 0, time.UTC),
			existingStatus: []agentsv1alpha1.CronScalingPolicyStatus{
				{Name: "scale-up", LastScheduleTime: timePtr(2026, 7, 3, 8, 0, 0)},
				{Name: "scale-down", LastScheduleTime: timePtr(2026, 7, 2, 20, 0, 0)}, // yesterday
			},
			expectedTarget: 20,
			expectedReason: "cron policy \"scale-down\" triggered",
		},
		{
			name: "no new trigger since last check — keep current",
			policies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "scale-up", Schedule: "0 8 * * *", TargetReplicas: 100, TimeZone: &utc},
			},
			now:            time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
			existingStatus: []agentsv1alpha1.CronScalingPolicyStatus{
				{Name: "scale-up", LastScheduleTime: timePtr(2026, 7, 3, 8, 0, 0)}, // already applied
			},
			expectNoTrigger: true,
		},
		{
			name: "every-minute policy fires on each reconcile",
			policies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "frequent", Schedule: "* * * * *", TargetReplicas: 5, TimeZone: &utc},
			},
			now:            time.Date(2026, 7, 3, 10, 30, 15, 0, time.UTC),
			existingStatus: []agentsv1alpha1.CronScalingPolicyStatus{
				{Name: "frequent", LastScheduleTime: timePtr(2026, 7, 3, 10, 29, 0)},
			},
			expectedTarget: 5,
			expectedReason: "cron policy \"frequent\" triggered",
		},
		{
			name: "timezone support",
			policies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "morning", Schedule: "0 8 * * *", TargetReplicas: 50, TimeZone: strPtr("Asia/Shanghai")},
			},
			now:            time.Date(2026, 7, 3, 1, 0, 0, 0, time.UTC), // Beijing 9am
			existingStatus: []agentsv1alpha1.CronScalingPolicyStatus{
				{Name: "morning", LastScheduleTime: timePtr(2026, 7, 2, 0, 0, 0)},
			},
			expectedTarget: 50,
			expectedReason: "cron policy \"morning\" triggered",
		},
		{
			name: "invalid cron expression",
			policies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "bad", Schedule: "NOT VALID", TargetReplicas: 10, TimeZone: &utc},
			},
			now:            time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
			expectError:    "invalid cron expression",
		},
		{
			name: "invalid timezone",
			policies: []agentsv1alpha1.CronScalingPolicy{
				{Name: "bad-tz", Schedule: "0 8 * * *", TargetReplicas: 10, TimeZone: strPtr("Invalid/Zone")},
			},
			now:            time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
			expectError:    "invalid timezone",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			target, reason, statuses, err := evaluateCronPolicies(
				tt.policies, tt.now, tt.existingStatus,
			)

			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)
			assert.Len(t, statuses, len(tt.policies))

			if tt.expectNoTrigger {
				assert.Contains(t, reason, "no cron policy has triggered yet")
				return
			}

			assert.Equal(t, tt.expectedTarget, target)
			assert.Contains(t, reason, tt.expectedReason)
		})
	}
}

func TestFindTriggerSince(t *testing.T) {
	schedule, err := cronParser.Parse("0 8 * * *")
	require.NoError(t, err)

	tests := []struct {
		name     string
		since    time.Time
		now      time.Time
		hasMatch bool
	}{
		{
			name:     "trigger within window",
			since:    time.Date(2026, 7, 3, 7, 0, 0, 0, time.UTC),
			now:      time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
			hasMatch: true,
		},
		{
			name:     "no trigger — both before schedule",
			since:    time.Date(2026, 7, 3, 6, 0, 0, 0, time.UTC),
			now:      time.Date(2026, 7, 3, 7, 0, 0, 0, time.UTC),
			hasMatch: false,
		},
		{
			name:     "no trigger — since is after the schedule",
			since:    time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
			now:      time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
			hasMatch: false,
		},
		{
			name:     "trigger exactly at since boundary — not included (since is exclusive)",
			since:    time.Date(2026, 7, 3, 8, 0, 0, 0, time.UTC),
			now:      time.Date(2026, 7, 3, 9, 0, 0, 0, time.UTC),
			hasMatch: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := findTriggerSince(schedule, tt.since, tt.now)
			if tt.hasMatch {
				assert.False(t, result.IsZero())
			} else {
				assert.True(t, result.IsZero())
			}
		})
	}
}

func timePtr(year int, month time.Month, day, hour, min, sec int) *metav1.Time {
	t := metav1.NewTime(time.Date(year, month, day, hour, min, sec, 0, time.UTC))
	return &t
}

func strPtr(s string) *string {
	return &s
}
