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

// ErrImageNotMapped reports a missing transitional image alias. Automatic
// virtual-template preparation is still required for image create compatibility.
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
