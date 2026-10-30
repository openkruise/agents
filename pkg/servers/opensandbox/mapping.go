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

import agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"

// MapState translates an agents sandbox state (plus the reason string that
// accompanies the "dead" state) into the OpenSandbox lifecycle vocabulary.
//
// A claimed instance that is not ready remains Pending. Native E2B state
// conventions must not turn a missing readiness observation into Running on
// the OpenSandbox API. Unknown backend states also remain Pending, retaining
// the reason for diagnosis until a recognized observation arrives.
func MapState(agentsState, reason string) SandboxState {
	if agentsState == agentsv1alpha1.SandboxStateDead && reason == "RunningResourceClaimedButNotReady" {
		return SandboxStatePending
	}
	switch agentsState {
	case agentsv1alpha1.SandboxStateRunning:
		return SandboxStateRunning
	case agentsv1alpha1.SandboxStatePaused:
		return SandboxStatePaused
	case agentsv1alpha1.SandboxStateCreating:
		return SandboxStatePending
	case agentsv1alpha1.SandboxStateDead:
		return SandboxStateTerminated
	default:
		return SandboxStatePending
	}
}
