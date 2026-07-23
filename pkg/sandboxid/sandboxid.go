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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// LabelKey is the reserved Sandbox label containing an authoritative ID.
const LabelKey = agentsv1alpha1.LabelSandboxID

var shortEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Resolve returns the authoritative label value or the legacy ID when no value is set.
func Resolve(sandbox metav1.Object) string {
	if sandboxID := sandbox.GetLabels()[LabelKey]; sandboxID != "" {
		return sandboxID
	}
	return Legacy(sandbox.GetNamespace(), sandbox.GetName())
}

// Legacy returns the legacy namespace-and-name Sandbox ID.
func Legacy(namespace, name string) string {
	return utils.GetSandboxID(&agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace,
		Name:      name,
	}})
}

// GenerateShort encodes all 128 bits of a Kubernetes UID as lowercase unpadded Base32.
func GenerateShort(uid types.UID) (string, error) {
	parsed, err := uuid.Parse(string(uid))
	if err != nil {
		return "", fmt.Errorf("invalid sandbox UID %q: %w", uid, err)
	}
	return strings.ToLower(shortEncoding.EncodeToString(parsed[:])), nil
}

// AssignShort assigns a short ID only when the authoritative label value is empty.
func AssignShort(sandbox metav1.Object) (bool, error) {
	labels := sandbox.GetLabels()
	if labels[LabelKey] != "" {
		return false, nil
	}

	sandboxID, err := GenerateShort(sandbox.GetUID())
	if err != nil {
		return false, err
	}
	if labels == nil {
		labels = make(map[string]string, 1)
	}
	labels[LabelKey] = sandboxID
	sandbox.SetLabels(labels)
	return true, nil
}
