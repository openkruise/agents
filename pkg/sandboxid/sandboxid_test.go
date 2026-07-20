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
	"maps"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

func TestResolve(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expected string
		format   string
	}{
		{name: "non-empty label is authoritative", labels: map[string]string{LabelKey: "operator-assigned-value"}, expected: "operator-assigned-value", format: FormatShort},
		{name: "absent label uses legacy ID", labels: map[string]string{"app": "sandbox"}, expected: "team-a--sandbox-a", format: FormatLegacy},
		{name: "empty label uses legacy ID", labels: map[string]string{LabelKey: ""}, expected: "team-a--sandbox-a", format: FormatLegacy},
		{name: "nil labels use legacy ID", labels: nil, expected: "team-a--sandbox-a", format: FormatLegacy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a",
				Name:      "sandbox-a",
				Labels:    tt.labels,
			}}
			id, format := ResolveWithFormat(sandbox)
			assert.Equal(t, tt.expected, id)
			assert.Equal(t, tt.format, format)
			assert.Equal(t, id, Resolve(sandbox))
		})
	}
}

func TestLegacyCompatibility(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		sandbox   string
	}{
		{name: "standard names", namespace: "team-a", sandbox: "sandbox-a"},
		{name: "empty namespace remains compatible", namespace: "", sandbox: "sandbox-a"},
		{name: "empty name remains compatible", namespace: "team-a", sandbox: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Namespace: tt.namespace, Name: tt.sandbox}}
			assert.Equal(t, utils.GetSandboxID(sandbox), Legacy(tt.namespace, tt.sandbox))
			assert.Equal(t, agentsv1alpha1.LabelSandboxID, LabelKey)
		})
	}
}

func TestGenerateShort(t *testing.T) {
	tests := []struct {
		name        string
		uid         types.UID
		expected    string
		expectError string
	}{
		{name: "zero UUID encodes all bits deterministically", uid: types.UID("00000000-0000-0000-0000-000000000000"), expected: strings.Repeat("a", 26)},
		{name: "different UUID changes the encoded value", uid: types.UID("00000000-0000-0000-0000-000000000001"), expected: strings.Repeat("a", 25) + "e"},
		{name: "invalid UUID is rejected", uid: types.UID("not-a-uuid"), expectError: "invalid sandbox UID"},
		{name: "empty UUID is rejected", uid: types.UID(""), expectError: "invalid sandbox UID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			actual, err := GenerateShort(tt.uid)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Empty(t, actual)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expected, actual)
			assert.Len(t, actual, 26)
			assert.Regexp(t, `^[a-z2-7]{26}$`, actual)
		})
	}
}

func TestAssignShort(t *testing.T) {
	tests := []struct {
		name          string
		uid           types.UID
		labels        map[string]string
		expectChanged bool
		expectedID    string
		expectError   string
	}{
		{name: "missing label is assigned", uid: types.UID("00000000-0000-0000-0000-000000000000"), expectChanged: true, expectedID: strings.Repeat("a", 26)},
		{name: "empty label is assigned and other labels are preserved", uid: types.UID("00000000-0000-0000-0000-000000000001"), labels: map[string]string{LabelKey: "", "app": "sandbox"}, expectChanged: true, expectedID: strings.Repeat("a", 25) + "e"},
		{name: "non-empty label is preserved without UID validation", uid: types.UID("not-a-uuid"), labels: map[string]string{LabelKey: "operator-assigned-value"}, expectedID: "operator-assigned-value"},
		{name: "invalid UID leaves labels unchanged", uid: types.UID("not-a-uuid"), labels: map[string]string{"app": "sandbox"}, expectError: "invalid sandbox UID"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			initialLabels := maps.Clone(tt.labels)
			sandbox := &agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{
				Namespace: "team-a",
				Name:      "sandbox-a",
				UID:       tt.uid,
				Labels:    maps.Clone(tt.labels),
			}}

			changed, err := AssignShort(sandbox)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.False(t, changed)
				assert.Equal(t, initialLabels, sandbox.Labels)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectChanged, changed)
			assert.Equal(t, tt.expectedID, Resolve(sandbox))
			if initialLabels["app"] != "" {
				assert.Equal(t, initialLabels["app"], sandbox.Labels["app"])
			}

			changed, err = AssignShort(sandbox)
			require.NoError(t, err)
			assert.False(t, changed)
		})
	}
}
