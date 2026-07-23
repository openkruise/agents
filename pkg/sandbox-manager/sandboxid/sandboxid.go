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
	// UUID is 16 bytes
	bytes := parsedUUID[:]

	// RFC 4648 Base32 encoding with padding removed
	encoder := base32.StdEncoding.WithPadding(base32.NoPadding)
	encoded := encoder.EncodeToString(bytes)

	return strings.ToLower(encoded), nil
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
