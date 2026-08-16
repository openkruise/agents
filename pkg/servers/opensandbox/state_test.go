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
	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
)

func TestConvertPhaseToOpenSandboxState(t *testing.T) {
	tests := []struct {
		phase agentsv1alpha1.SandboxPhase
		want  models.SandboxState
	}{
		{agentsv1alpha1.SandboxPending, models.SandboxStatePending},
		{agentsv1alpha1.SandboxRunning, models.SandboxStateRunning},
		{agentsv1alpha1.SandboxUpgrading, models.SandboxStateRunning},
		{agentsv1alpha1.SandboxPaused, models.SandboxStatePaused},
		{agentsv1alpha1.SandboxResuming, models.SandboxStateResuming},
		{agentsv1alpha1.SandboxRecycling, models.SandboxStateStopping},
		{agentsv1alpha1.SandboxTerminating, models.SandboxStateStopping},
		{agentsv1alpha1.SandboxSucceeded, models.SandboxStateTerminated},
		{agentsv1alpha1.SandboxFailed, models.SandboxStateFailed},
		{agentsv1alpha1.SandboxPhase("SomeUnknownPhase"), models.SandboxStateFailed},
	}
	for _, tt := range tests {
		t.Run(string(tt.phase), func(t *testing.T) {
			assert.Equal(t, tt.want, convertPhaseToOpenSandboxState(string(tt.phase)))
		})
	}
}
