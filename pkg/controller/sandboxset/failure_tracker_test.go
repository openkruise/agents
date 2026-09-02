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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func TestFailureTracker(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	const key = "default/pool"

	type sighting struct {
		uid    types.UID
		reason string
		at     time.Duration // offset from base
	}
	tests := []struct {
		name      string
		sightings []sighting
		countAt   time.Duration
		window    time.Duration
		expect    int
	}{
		{
			name:      "distinct terminal failures are counted once each",
			sightings: []sighting{{"a", utils.ReasonResourceFailed, 0}, {"b", utils.ReasonResourceFailed, time.Minute}},
			countAt:   2 * time.Minute,
			window:    5 * time.Minute,
			expect:    2,
		},
		{
			name: "the same UID seen on many reconciles counts once",
			sightings: []sighting{
				{"a", utils.ReasonResourceFailed, 0},
				{"a", utils.ReasonResourceFailed, time.Second},
				{"a", utils.ReasonResourceFailed, 2 * time.Second},
			},
			countAt: time.Minute,
			window:  5 * time.Minute,
			expect:  1,
		},
		{
			name: "a later ResourceDeleted sighting does not erase the captured failure",
			sightings: []sighting{
				{"a", utils.ReasonResourceFailed, 0},
				{"a", "ResourceDeleted", time.Second},
			},
			countAt: time.Minute,
			window:  5 * time.Minute,
			expect:  1,
		},
		{
			name: "a sandbox first seen while terminating is not counted as a failure",
			sightings: []sighting{
				{"a", "ResourceDeleted", 0},
				{"a", utils.ReasonResourceFailed, time.Second},
			},
			countAt: time.Minute,
			window:  5 * time.Minute,
			expect:  0,
		},
		{
			name:      "non-failure terminal reasons are ignored",
			sightings: []sighting{{"a", "ResourceSucceeded", 0}, {"b", "ShutdownTimeReached", 0}},
			countAt:   time.Minute,
			window:    5 * time.Minute,
			expect:    0,
		},
		{
			name:      "records outside the window are dropped",
			sightings: []sighting{{"a", utils.ReasonResourceFailed, 0}, {"b", utils.ReasonResourceFailed, 9 * time.Minute}},
			countAt:   10 * time.Minute,
			window:    5 * time.Minute,
			expect:    1,
		},
		{
			name:      "an empty UID is ignored",
			sightings: []sighting{{"", utils.ReasonResourceFailed, 0}},
			countAt:   time.Minute,
			window:    5 * time.Minute,
			expect:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tracker := newFailureTracker()
			for _, s := range tt.sightings {
				tracker.observe(key, s.uid, s.reason, base.Add(s.at))
			}
			assert.Equal(t, tt.expect, tracker.terminalFailures(key, tt.window, base.Add(tt.countAt)))
		})
	}
}

func TestFailureTrackerForgetAndIsolation(t *testing.T) {
	base := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	tracker := newFailureTracker()

	tracker.observe("default/one", "a", utils.ReasonResourceFailed, base)
	tracker.observe("default/two", "b", utils.ReasonResourceFailed, base)

	assert.Equal(t, 1, tracker.terminalFailures("default/one", time.Minute, base))
	assert.Equal(t, 1, tracker.terminalFailures("default/two", time.Minute, base))

	tracker.forget("default/one")
	assert.Equal(t, 0, tracker.terminalFailures("default/one", time.Minute, base))
	assert.Equal(t, 1, tracker.terminalFailures("default/two", time.Minute, base),
		"forgetting one SandboxSet must not touch another")

	// A recreated pool starts from zero rather than inheriting the old count.
	tracker.observe("default/one", "c", utils.ReasonResourceFailed, base)
	assert.Equal(t, 1, tracker.terminalFailures("default/one", time.Minute, base))
}

func TestSetScaleUpBlockedCondition(t *testing.T) {
	restoreThreshold := scaleUpFailureThreshold
	restoreWindow := scaleUpFailureWindow
	t.Cleanup(func() {
		scaleUpFailureThreshold = restoreThreshold
		scaleUpFailureWindow = restoreWindow
	})
	scaleUpFailureThreshold = 5
	scaleUpFailureWindow = 5 * time.Minute

	status := &v1alpha1.SandboxSetStatus{}

	assert.True(t, setScaleUpBlockedCondition(status, true), "first write must report a change")
	cond := findCondition(status, string(v1alpha1.SandboxSetConditionScaleUpBlocked))
	if assert.NotNil(t, cond) {
		assert.Equal(t, metav1.ConditionTrue, cond.Status)
		assert.Equal(t, v1alpha1.SandboxSetReasonTerminalFailures, cond.Reason)
	}
	firstTransition := cond.LastTransitionTime

	// updateSandboxSetStatus compares the whole status with reflect.DeepEqual,
	// so a repeated write while still blocked has to be a no-op. Otherwise the
	// status write and the SandboxSet watch feed each other every reconcile.
	assert.False(t, setScaleUpBlockedCondition(status, true), "repeat write while blocked must not change status")
	assert.Equal(t, firstTransition, findCondition(status, string(v1alpha1.SandboxSetConditionScaleUpBlocked)).LastTransitionTime)

	assert.True(t, setScaleUpBlockedCondition(status, false), "recovery must report a change")
	cond = findCondition(status, string(v1alpha1.SandboxSetConditionScaleUpBlocked))
	if assert.NotNil(t, cond) {
		assert.Equal(t, metav1.ConditionFalse, cond.Status)
		assert.Equal(t, v1alpha1.SandboxSetReasonWithinBudget, cond.Reason)
	}
	assert.False(t, setScaleUpBlockedCondition(status, false), "repeat write while healthy must not change status")

	// Disabling the check clears a condition left by an earlier configuration.
	scaleUpFailureThreshold = 0
	assert.True(t, setScaleUpBlockedCondition(status, false))
	assert.Nil(t, findCondition(status, string(v1alpha1.SandboxSetConditionScaleUpBlocked)))
	assert.False(t, setScaleUpBlockedCondition(status, false), "nothing left to remove")
}

func findCondition(status *v1alpha1.SandboxSetStatus, condType string) *metav1.Condition {
	for i := range status.Conditions {
		if status.Conditions[i].Type == condType {
			return &status.Conditions[i]
		}
	}
	return nil
}

func TestFailureTracker_ReclaimsEmptyControllers(t *testing.T) {
	tracker := newFailureTracker()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	window := 5 * time.Minute

	tracker.observe("ns/pool", "uid-a", utils.ReasonResourceFailed, start)
	require.Equal(t, 1, tracker.terminalFailures("ns/pool", window, start))
	require.Len(t, tracker.controllers, 1)

	// Past the window, the count drops and the controller entry goes with it.
	require.Zero(t, tracker.terminalFailures("ns/pool", window, start.Add(window+time.Second)))
	assert.Empty(t, tracker.controllers, "an aged-out controller key must be reclaimed")

	// A pool that failed again after the reclaim starts from a clean count.
	later := start.Add(time.Hour)
	tracker.observe("ns/pool", "uid-b", utils.ReasonResourceFailed, later)
	assert.Equal(t, 1, tracker.terminalFailures("ns/pool", window, later))
}
