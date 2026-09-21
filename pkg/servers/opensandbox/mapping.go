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
	"strings"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

// ErrImageNotMapped is returned by ResolveTemplateID when the requested image
// URI has no entry in the static alias table. Phase 1 deliberately has no
// fallback: an unmapped image is a configuration gap, not a runtime condition,
// and the caller surfaces it as 500, mirroring the reference server's
// status for an unavailable creation source, so the operator sees the
// missing alias immediately. The formal "virtual template" direction
// (per-image auto-create or reuse of a SandboxSet) is tracked in issue
// #690 and belongs to a later PR.
type ErrImageNotMapped struct {
	ImageURI string
}

func (e *ErrImageNotMapped) Error() string {
	return fmt.Sprintf("image %q is not mapped to any SandboxTemplate; add %s=<template-id> to the %s environment variable", e.ImageURI, e.ImageURI, EnvImageAliases)
}

// ResolveTemplateID looks up the agents SandboxTemplate name that backs the
// given OpenSandbox image URI. The alias table is injected at startup
// (the OPENSANDBOX_IMAGE_ALIASES environment variable) and is read-only at
// request time. Lookup is
// exact-string: no normalization, no registry/tag inference. A miss returns an
// error wrapping *ErrImageNotMapped so the caller can classify it as 500 with
// errors.As instead of matching on the message.
//
// The error is returned as the interface type (not the concrete pointer) so a
// successful lookup yields a true nil error: returning a nil *ErrImageNotMapped
// through an error-typed variable would produce a non-nil interface and break
// `err != nil` checks at the call site.
func ResolveTemplateID(aliases map[string]string, imageURI string) (string, error) {
	if imageURI == "" {
		return "", &ErrImageNotMapped{ImageURI: imageURI}
	}
	if templateID, ok := aliases[imageURI]; ok && templateID != "" {
		return templateID, nil
	}
	return "", &ErrImageNotMapped{ImageURI: imageURI}
}

// MapState translates an agents sandbox state (plus the reason string that
// accompanies the "dead" state) into the OpenSandbox lifecycle vocabulary.
//
// The mapping mirrors the convention established by the E2B layer's
// convertToE2BSandbox: a sandbox in the Running phase that is claimed but not
// yet ready is reported as "dead" by GetState because the Ready condition is
// unsatisfied, but the underlying phase is Running, so both protocols surface
// it as running/Running to avoid returning an unparsable terminal state to
// SDK clients while the sandbox is still live.
//
// Phase 1 only emits StateRunning from the create path; the remaining branches
// are declared so later phases (describe/list/pause/resume) reuse the same
// translation instead of re-deriving it at each call site.
func MapState(agentsState, reason string) SandboxState {
	if agentsState == agentsv1alpha1.SandboxStateDead && reason == "RunningResourceClaimedButNotReady" {
		return SandboxStateRunning
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
		// Unknown states fall back to Running so a new agents-side state does
		// not silently become a terminal state on the OpenSandbox surface.
		// The reason string is preserved in SandboxStatus.Reason for
		// diagnosis.
		return SandboxStateRunning
	}
}

// EnvImageAliases is the environment variable carrying the comma-separated
// image alias table ("image_uri=template_id,..."). It is typically injected
// from a ConfigMap via envFrom and is read once at startup; there is no
// dynamic refresh, so changing it requires a pod restart.
const EnvImageAliases = "OPENSANDBOX_IMAGE_ALIASES"

// SplitImageAliasesEnv splits an EnvImageAliases value into individual
// "image_uri=template_id" entries. Entries are trimmed and empty items are
// dropped so a trailing comma or a formatted multi-line ConfigMap value stays
// parsable; malformed entries are left for ParseImageAliases to reject.
func SplitImageAliasesEnv(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	entries := make([]string, 0, len(parts))
	for _, part := range parts {
		if part = strings.TrimSpace(part); part != "" {
			entries = append(entries, part)
		}
	}
	return entries
}

// ParseImageAlias parses a single alias entry of the form
// "image_uri=template_id". It returns an error when the entry is malformed so
// startup fails loudly instead of registering a half-parsed alias.
func ParseImageAlias(entry string) (imageURI, templateID string, err error) {
	imageURI, templateID, found := strings.Cut(entry, "=")
	if !found {
		return "", "", fmt.Errorf("invalid image alias %q: expected image_uri=template_id", entry)
	}
	imageURI = strings.TrimSpace(imageURI)
	templateID = strings.TrimSpace(templateID)
	if imageURI == "" || templateID == "" {
		return "", "", fmt.Errorf("invalid image alias %q: image_uri and template_id must both be non-empty", entry)
	}
	return imageURI, templateID, nil
}

// ParseImageAliases parses a list of alias entries (see SplitImageAliasesEnv) into a
// read-only map. Duplicate image URIs are rejected: an operator typo that
// silently overwrites an earlier alias is worse than a startup failure.
func ParseImageAliases(entries []string) (map[string]string, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	aliases := make(map[string]string, len(entries))
	for _, entry := range entries {
		imageURI, templateID, err := ParseImageAlias(entry)
		if err != nil {
			return nil, err
		}
		if existing, ok := aliases[imageURI]; ok {
			return nil, fmt.Errorf("duplicate image alias for %q: already mapped to %q, cannot remap to %q", imageURI, existing, templateID)
		}
		aliases[imageURI] = templateID
	}
	return aliases, nil
}
