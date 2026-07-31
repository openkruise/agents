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

package sandboxid

import (
	"encoding/base32"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	types "k8s.io/apimachinery/pkg/types"
)

// LabelKey is the reserved Sandbox label key containing the resolved short sandbox ID.
const LabelKey = v1alpha1.LabelSandboxID

// ShortIDLength is the exact length of a generated short sandbox ID (26 Base32 characters).
const ShortIDLength = 26

// base32Encoder is a thread-safe, pre-compiled unpadded Base32 encoder using lowercase characters.
var base32Encoder = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Resolve returns the authoritative sandbox ID.
// If the sandbox-id label is present and non-empty, returns the label value.
// Otherwise, falls back to the legacy "<namespace>--<name>" format.
func Resolve(sandbox metav1.Object) string {
	if sandbox == nil {
		return ""
	}
	labels := sandbox.GetLabels()
	if val, ok := labels[LabelKey]; ok && val != "" {
		return val
	}
	return Legacy(sandbox.GetNamespace(), sandbox.GetName())
}

// Legacy returns the legacy deterministic format "<namespace>--<name>".
func Legacy(namespace, name string) string {
	return fmt.Sprintf("%s--%s", namespace, name)
}

// GenerateShortID decodes a Kubernetes UID as a 16-byte UUID and encodes it into 26 lowercase Base32 characters.
func GenerateShortID(uid types.UID) (string, error) {
	parsedUUID, err := uuid.Parse(string(uid))
	if err != nil {
		return "", fmt.Errorf("failed to parse UID as UUID: %w", err)
	}
	return base32Encoder.EncodeToString(parsedUUID[:]), nil
}

// ParseShortID decodes a 26-character lowercase Base32 short ID back into a Kubernetes UID (UUID format).
func ParseShortID(shortID string) (types.UID, error) {
	if len(shortID) != ShortIDLength {
		return "", fmt.Errorf("invalid short ID length: expected %d, got %d", ShortIDLength, len(shortID))
	}
	decoded, err := base32Encoder.DecodeString(strings.ToLower(shortID))
	if err != nil {
		return "", fmt.Errorf("failed to decode short ID: %w", err)
	}
	parsedUUID, err := uuid.FromBytes(decoded)
	if err != nil {
		return "", fmt.Errorf("failed to construct UUID from bytes: %w", err)
	}
	return types.UID(parsedUUID.String()), nil
}

// IsValidShortID checks whether a string is a valid 26-character lowercase Base32 short sandbox ID.
func IsValidShortID(id string) bool {
	if len(id) != ShortIDLength {
		return false
	}
	for i := 0; i < len(id); i++ {
		ch := id[i]
		if !((ch >= 'a' && ch <= 'z') || (ch >= '2' && ch <= '7')) {
			return false
		}
	}
	return true
}

// AssignShortID checks if the sandbox already has a sandbox-id label.
// If not, generates a short ID from its UID and sets it as the label.
func AssignShortID(sandbox metav1.Object) (changed bool, err error) {
	if sandbox == nil {
		return false, fmt.Errorf("sandbox is nil")
	}
	labels := sandbox.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	if val, ok := labels[LabelKey]; ok && val != "" {
		return false, nil
	}
	shortID, err := GenerateShortID(sandbox.GetUID())
	if err != nil {
		return false, err
	}
	labels[LabelKey] = shortID
	sandbox.SetLabels(labels)
	return true, nil
}
