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

package opensandbox

import (
	"testing"

	"github.com/stretchr/testify/assert"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

func TestMapState(t *testing.T) {
	tests := []struct {
		name        string
		agentsState string
		reason      string
		want        SandboxState
	}{
		{
			name:        "running maps to Running",
			agentsState: agentsv1alpha1.SandboxStateRunning,
			reason:      "RunningResourceClaimedAndReady",
			want:        SandboxStateRunning,
		},
		{
			name:        "claimed but not ready remains Pending",
			agentsState: agentsv1alpha1.SandboxStateDead,
			reason:      "RunningResourceClaimedButNotReady",
			want:        SandboxStatePending,
		},
		{
			name:        "paused maps to Paused",
			agentsState: agentsv1alpha1.SandboxStatePaused,
			reason:      "RunningResourceClaimedAndPaused",
			want:        SandboxStatePaused,
		},
		{
			name:        "creating maps to Pending",
			agentsState: agentsv1alpha1.SandboxStateCreating,
			reason:      agentsv1alpha1.SandboxStateReasonResourcePending,
			want:        SandboxStatePending,
		},
		{
			name:        "dead with terminal reason maps to Terminated",
			agentsState: agentsv1alpha1.SandboxStateDead,
			reason:      "ResourceTerminating",
			want:        SandboxStateTerminated,
		},
		{
			name:        "unknown state remains Pending",
			agentsState: "some-future-state",
			reason:      "Whatever",
			want:        SandboxStatePending,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, MapState(tt.agentsState, tt.reason))
		})
	}
}
