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
	"fmt"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	annotationutils "github.com/openkruise/agents/pkg/utils/annotations"
)

// projectSandbox renders an agents sandbox into the OpenSandbox `Sandbox`
// response shape shared by describe and list. It is the read-path counterpart
// of create.go's convertToOpenSandboxResponse, trimmed to the fields the
// OpenSandbox `Sandbox` schema carries on a read (the create response omits
// startup-source details per the upstream spec; the read response omits them
// because agents is template-backed — see SandboxResponse).
//
// The required keys (id, status, createdAt, entrypoint) are always populated so
// the generated SDKs can pop them unconditionally; entrypoint is an empty list
// because the create path echoes but does not persist it (compatibility limit).
func projectSandbox(sbx infra.Sandbox) (SandboxResponse, error) {
	state, err := MapSandboxState(sbx)
	if err != nil {
		return SandboxResponse{}, err
	}
	_, reason := sbx.GetState()

	resp := SandboxResponse{
		ID: sbx.GetSandboxID(),
		Status: SandboxStatus{
			State:  state,
			Reason: reason,
		},
		// Always non-nil so the key serializes as [] rather than being absent:
		// the SDK pops entrypoint unconditionally (Phase 1 lesson).
		Entrypoint: []string{},
		CreatedAt:  resolveCreatedAt(sbx).Format(time.RFC3339),
		Metadata:   extractUserMetadata(sbx),
	}
	if expiresAt := resolveExpiresAt(sbx); !expiresAt.IsZero() {
		resp.ExpiresAt = expiresAt.Format(time.RFC3339)
	}
	return resp, nil
}

// resolveCreatedAt mirrors create.go's createdAt fallback chain so a sandbox
// reads back with the same createdAt it reported at creation: prefer the
// claim-time annotation, fall back to the object creation timestamp, and
// finally to now as a defensive last resort (createdAt is schema-required and
// must never be empty).
func resolveCreatedAt(sbx infra.Sandbox) time.Time {
	if claimTime, err := sbx.GetClaimTime(); err == nil {
		return claimTime
	}
	if creationTimestamp := sbx.GetCreationTimestamp(); !creationTimestamp.IsZero() {
		return creationTimestamp.Time
	}
	return time.Now().UTC()
}

// resolveExpiresAt mirrors create.go's expiresAt derivation: the earlier of
// PauseTime and ShutdownTime, with PauseTime taking precedence when set (the
// E2B endAt convention). The OpenSandbox create path never sets PauseTime (no
// auto-pause), so this is ShutdownTime in practice; the branch is kept so create
// and read responses stay byte-consistent for the same sandbox.
func resolveExpiresAt(sbx infra.Sandbox) time.Time {
	timeoutOpts := sbx.GetTimeout()
	if !timeoutOpts.PauseTime.IsZero() {
		return timeoutOpts.PauseTime
	}
	return timeoutOpts.ShutdownTime
}

// extractUserMetadata recovers the caller-visible metadata from a sandbox's
// labels and annotations, dropping every key under an internal reserved prefix
// (agents.kruise.io/, e2b.agents.kruise.io/) so system-owned annotations never
// leak through the OpenSandbox surface. The protected resource-context key is
// written last so persisted user metadata cannot spoof it, matching create.go
// and the E2B layer's convention.
func extractUserMetadata(sbx infra.Sandbox) map[string]string {
	metadata := make(map[string]string)
	// Labels are read first for backward compatibility (the E2B layer stores
	// some user metadata as labels); annotations win on key collision because
	// the OpenSandbox create path persists user metadata as annotations.
	for k, v := range sbx.GetLabels() {
		if annotationutils.IsBlackListed(k) {
			continue
		}
		metadata[k] = v
	}
	for k, v := range sbx.GetAnnotations() {
		if annotationutils.IsBlackListed(k) {
			continue
		}
		metadata[k] = v
	}
	metadata[MetadataKeySandboxResource] = fmt.Sprintf("%s/%s", sbx.GetNamespace(), sbx.GetName())
	return metadata
}

// isSandboxViewable reports whether a loaded sandbox should be surfaced by the
// read routes or hidden behind a 404. It mirrors the E2B layer's viewability
// rule so both protocols agree on when a sandbox is "gone":
//   - creating sandboxes are hidden (not yet live);
//   - dead sandboxes with a terminal reason (Succeeded/Failed/Terminating/
//     Deleted) are hidden;
//   - failed sandboxes reserved for debugging are hidden.
//
// A dead sandbox with a non-terminal reason (e.g. RunningResourceClaimedButNot
// Ready, ShutdownTimeReached) stays viewable so describe can report its
// transitional state instead of pretending it never existed.
func isSandboxViewable(sbx infra.Sandbox) bool {
	if utils.IsReservedFailedSandbox(sbx.GetLabels()) {
		return false
	}
	state, reason := sbx.GetState()
	if state == agentsv1alpha1.SandboxStateCreating {
		return false
	}
	if state != agentsv1alpha1.SandboxStateDead {
		return true
	}
	switch reason {
	case reasonResourceSucceeded, reasonResourceFailed, reasonResourceTerminating, reasonResourceDeleted:
		return false
	default:
		return true
	}
}
