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

// Package resourceaccess defines the versioned resource-access policy shared by
// delivery admission and independent lifecycle controllers.
package resourceaccess

import (
	"errors"
	"fmt"
	"maps"
)

type Mode string

const (
	TrustedDomain Mode = "trusted-domain"
	Isolated      Mode = "isolated"

	AnnotationPolicy      = "agents.kruise.io/resource-access-policy"
	StorageAuthAnnotation = "security.agents.kruise.io/storage-auth"
)

var (
	ErrAccessDenied = errors.New("resource access denied")
	ErrBackendRead  = errors.New("resource lookup failed")
)

// Normalize preserves the zero-value configuration used by existing callers.
// Unknown nonempty values must never silently choose the compatible mode.
func Normalize(mode Mode) (Mode, error) {
	switch mode {
	case "", TrustedDomain:
		return TrustedDomain, nil
	case Isolated:
		return Isolated, nil
	default:
		return "", fmt.Errorf("unsupported resource access mode %q", mode)
	}
}

// FromAnnotations uses only the accepted delivery's policy, never a process's
// current default. An absent marker denotes a legacy delivery; an invalid or
// empty marker fails closed instead of downgrading an isolated delivery.
func FromAnnotations(annotations map[string]string) (Mode, error) {
	value, exists := annotations[AnnotationPolicy]
	if !exists {
		return TrustedDomain, nil
	}
	switch value {
	case "trusted-domain-v1":
		return TrustedDomain, nil
	case "isolated-v1":
		return Isolated, nil
	default:
		return "", fmt.Errorf("invalid resource access policy")
	}
}

// Stamp returns independent metadata for a new delivery. Call after all request
// modifiers and checkpoint restoration so neither can select its policy.
func Stamp(annotations map[string]string, mode Mode) (map[string]string, error) {
	mode, err := Normalize(mode)
	if err != nil {
		return nil, err
	}
	result := maps.Clone(annotations)
	if result == nil {
		result = make(map[string]string)
	}
	result[AnnotationPolicy] = string(mode) + "-v1"
	return result, nil
}
