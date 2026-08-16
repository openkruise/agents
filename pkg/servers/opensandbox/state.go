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
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
)

// convertPhaseToOpenSandboxState maps the Sandbox CRD's finer-grained
// SandboxPhase to the OpenSandbox lifecycle spec's SandboxState
// (Pending/Running/Pausing/Paused/Resuming/Stopping/Terminated/Failed).
//
// Two mappings are heuristic rather than exact, and are called out here so a
// reviewer can judge them rather than discover them by surprise:
//   - SandboxUpgrading (an in-place image/resource update, a KruiseAgents
//     concept with no OpenSandbox equivalent) maps to Running because the
//     sandbox keeps serving traffic during the update.
//   - "Pausing" (the async in-flight moment between a pause request and the
//     sandbox actually reaching Paused) is not currently observable through
//     the infra.Sandbox interface, and in practice is rarely observed here:
//     SandboxManager.PauseSandbox is synchronous, so by the time this
//     adapter's 202 response is written the phase has usually already
//     reached Paused. See the package AGENTS.md "Known Gaps" for the
//     asynchronous-pause follow-up.
func convertPhaseToOpenSandboxState(phase string) models.SandboxState {
	switch agentsv1alpha1.SandboxPhase(phase) {
	case agentsv1alpha1.SandboxPending:
		return models.SandboxStatePending
	case agentsv1alpha1.SandboxRunning, agentsv1alpha1.SandboxUpgrading:
		return models.SandboxStateRunning
	case agentsv1alpha1.SandboxPaused:
		return models.SandboxStatePaused
	case agentsv1alpha1.SandboxResuming:
		return models.SandboxStateResuming
	case agentsv1alpha1.SandboxRecycling, agentsv1alpha1.SandboxTerminating:
		return models.SandboxStateStopping
	case agentsv1alpha1.SandboxSucceeded:
		return models.SandboxStateTerminated
	case agentsv1alpha1.SandboxFailed:
		return models.SandboxStateFailed
	default:
		// An unrecognized phase is surfaced as Failed rather than silently
		// defaulting to a healthy-looking state, since a caller polling for
		// completion must never mistake "unknown" for "fine".
		return models.SandboxStateFailed
	}
}
