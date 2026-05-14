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

package events

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEventReasonConstants_Unique(t *testing.T) {
	// Collect all event reason constants and verify no duplicate values exist.
	allReasons := []struct {
		name  string
		value string
	}{
		// Sandbox lifecycle events
		{"SandboxPodCreated", SandboxPodCreated},
		{"SandboxPodCreateFailed", SandboxPodCreateFailed},
		{"SandboxReady", SandboxReady},
		{"SandboxPausing", SandboxPausing},
		{"SandboxPaused", SandboxPaused},
		{"SandboxResuming", SandboxResuming},
		{"SandboxResumed", SandboxResumed},
		{"SandboxUpgrading", SandboxUpgrading},
		{"SandboxUpgraded", SandboxUpgraded},
		{"SandboxFailed", SandboxFailed},
		{"SandboxDeleting", SandboxDeleting},

		// SandboxSet lifecycle events
		{"SandboxCreated", SandboxCreated},
		{"CreateSandboxFailed", CreateSandboxFailed},
		{"SandboxScaledDown", SandboxScaledDown},
		{"FailedSandboxDeleted", FailedSandboxDeleted},
		{"SandboxSetScalingUp", SandboxSetScalingUp},
		{"SandboxSetScalingDown", SandboxSetScalingDown},
		{"SandboxSetRollingUpdate", SandboxSetRollingUpdate},
		{"SandboxSetRollingUpdateCompleted", SandboxSetRollingUpdateCompleted},

		// SandboxClaim lifecycle events
		{"SandboxClaimBinding", SandboxClaimBinding},
		{"SandboxClaimCompleted", SandboxClaimCompleted},
		{"SandboxClaimed", SandboxClaimed},
		{"NoAvailableSandboxes", NoAvailableSandboxes},
		{"SandboxClaimTTLDelete", SandboxClaimTTLDelete},
		{"FeatureGateDisabled", FeatureGateDisabled},
		{"UnknownPhase", UnknownPhase},

		// SandboxUpdateOps lifecycle events
		{"UpdateOpsStarted", UpdateOpsStarted},
		{"UpdateOpsProgressing", UpdateOpsProgressing},
		{"UpdateOpsCompleted", UpdateOpsCompleted},
		{"UpdateOpsFailed", UpdateOpsFailed},
	}

	tests := []struct {
		name string
	}{
		{name: "all event reason constants have unique values"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			seen := make(map[string]string) // value -> constant name
			for _, r := range allReasons {
				if existingName, exists := seen[r.value]; exists {
					t.Errorf("duplicate event reason value %q: used by both %q and %q",
						r.value, existingName, r.name)
				}
				seen[r.value] = r.name
			}
			assert.Equal(t, len(allReasons), len(seen),
				"expected all constants to have unique values")
		})
	}
}

func TestEventReasonConstants_NonEmpty(t *testing.T) {
	// Verify that no event reason constant is an empty string.
	allReasons := []struct {
		name  string
		value string
	}{
		{"SandboxPodCreated", SandboxPodCreated},
		{"SandboxPodCreateFailed", SandboxPodCreateFailed},
		{"SandboxReady", SandboxReady},
		{"SandboxPausing", SandboxPausing},
		{"SandboxPaused", SandboxPaused},
		{"SandboxResuming", SandboxResuming},
		{"SandboxResumed", SandboxResumed},
		{"SandboxUpgrading", SandboxUpgrading},
		{"SandboxUpgraded", SandboxUpgraded},
		{"SandboxFailed", SandboxFailed},
		{"SandboxDeleting", SandboxDeleting},
		{"SandboxCreated", SandboxCreated},
		{"CreateSandboxFailed", CreateSandboxFailed},
		{"SandboxScaledDown", SandboxScaledDown},
		{"FailedSandboxDeleted", FailedSandboxDeleted},
		{"SandboxSetScalingUp", SandboxSetScalingUp},
		{"SandboxSetScalingDown", SandboxSetScalingDown},
		{"SandboxSetRollingUpdate", SandboxSetRollingUpdate},
		{"SandboxSetRollingUpdateCompleted", SandboxSetRollingUpdateCompleted},
		{"SandboxClaimBinding", SandboxClaimBinding},
		{"SandboxClaimCompleted", SandboxClaimCompleted},
		{"SandboxClaimed", SandboxClaimed},
		{"NoAvailableSandboxes", NoAvailableSandboxes},
		{"SandboxClaimTTLDelete", SandboxClaimTTLDelete},
		{"FeatureGateDisabled", FeatureGateDisabled},
		{"UnknownPhase", UnknownPhase},
		{"UpdateOpsStarted", UpdateOpsStarted},
		{"UpdateOpsProgressing", UpdateOpsProgressing},
		{"UpdateOpsCompleted", UpdateOpsCompleted},
		{"UpdateOpsFailed", UpdateOpsFailed},
	}

	tests := []struct {
		name string
	}{
		{name: "all event reason constants are non-empty"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, r := range allReasons {
				assert.NotEmpty(t, r.value, "event reason constant %q should not be empty", r.name)
			}
		})
	}
}
