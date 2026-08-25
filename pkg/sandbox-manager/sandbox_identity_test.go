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

package sandbox_manager

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxid"
)

func TestWithSandboxIDAssignment(t *testing.T) {
	callerErr := errors.New("caller failed")
	generateErr := errors.New("generate failed")
	tests := []struct {
		name             string
		labels           map[string]string
		modifier         func(infra.Sandbox) error
		enabled          bool
		prefix           string
		generate         func() (string, error)
		expectError      string
		expectID         string
		expectIDOnError  string
		expectAnnotation string
	}{
		{name: "disabled without caller leaves unmarked delivery legacy"},
		{
			name: "disabled caller mutation is preserved",
			modifier: func(sandbox infra.Sandbox) error {
				sandbox.SetAnnotations(map[string]string{"example": "value"})
				return nil
			},
			expectAnnotation: "value",
		},
		{
			name: "caller reserved addition is discarded when assignment is disabled",
			modifier: func(sandbox infra.Sandbox) error {
				sandbox.SetLabels(map[string]string{sandboxid.LabelKey: "spoofed"})
				return nil
			},
		},
		{
			name:   "disabled assignment retires existing delivery ID",
			labels: map[string]string{sandboxid.LabelKey: "existing"},
		},
		{
			name:   "caller reserved value is discarded when assignment is disabled",
			labels: map[string]string{sandboxid.LabelKey: "existing"},
			modifier: func(sandbox infra.Sandbox) error {
				sandbox.GetLabels()[sandboxid.LabelKey] = "changed"
				return nil
			},
		},
		{
			name: "caller reserved addition is overwritten by assignment",
			modifier: func(sandbox infra.Sandbox) error {
				sandbox.SetLabels(map[string]string{sandboxid.LabelKey: "spoofed"})
				return nil
			},
			enabled:  true,
			generate: func() (string, error) { return "aaaaaaaaaaaac", nil },
			expectID: "aaaaaaaaaaaac",
		},
		{
			name: "caller runs before assignment",
			modifier: func(sandbox infra.Sandbox) error {
				if _, present := sandbox.GetLabels()[sandboxid.LabelKey]; present {
					return errors.New("core assignment ran before caller")
				}
				sandbox.SetAnnotations(map[string]string{"example": "caller-first"})
				return nil
			},
			enabled:          true,
			generate:         func() (string, error) { return "aaaaaaaaaaaac", nil },
			expectID:         "aaaaaaaaaaaac",
			expectAnnotation: "caller-first",
		},
		{
			name:     "assignment prepends configured prefix",
			enabled:  true,
			prefix:   "prod-",
			generate: func() (string, error) { return "aaaaaaaaaaaae", nil },
			expectID: "prod-aaaaaaaaaaaae",
		},
		{
			name:     "existing delivery ID is replaced",
			labels:   map[string]string{sandboxid.LabelKey: "existing"},
			enabled:  true,
			generate: func() (string, error) { return "aaaaaaaaaaaag", nil },
			expectID: "aaaaaaaaaaaag",
		},
		{
			name: "caller failure stops assignment",
			modifier: func(infra.Sandbox) error {
				return callerErr
			},
			enabled:     true,
			generate:    func() (string, error) { return "aaaaaaaaaaaac", nil },
			expectError: callerErr.Error(),
		},
		{
			name:        "generator failure stops persistence path",
			enabled:     true,
			generate:    func() (string, error) { return "", generateErr },
			expectError: generateErr.Error(),
		},
		{
			name:   "enabled assignment requires initialized generator",
			labels: map[string]string{sandboxid.LabelKey: "existing"},
			modifier: func(sandbox infra.Sandbox) error {
				sandbox.GetLabels()[sandboxid.LabelKey] = "changed"
				return nil
			},
			enabled:         true,
			expectError:     "short sandbox ID generator is not initialized",
			expectIDOnError: "existing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			modifier := withSandboxIDAssignment(tt.modifier, tt.enabled, tt.prefix, tt.generate)
			require.NotNil(t, modifier)
			sandbox := sandboxcr.AsSandbox(&agentsv1alpha1.Sandbox{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}, nil)
			err := modifier(sandbox)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				assert.Equal(t, tt.expectIDOnError, sandbox.GetLabels()[sandboxid.LabelKey])
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectID, sandbox.GetLabels()[sandboxid.LabelKey])
			assert.Equal(t, tt.expectAnnotation, sandbox.GetAnnotations()["example"])
		})
	}
}

func TestWithSandboxIDAssignmentGeneratesIDForEachDelivery(t *testing.T) {
	generateCalls := 0
	modifier := withSandboxIDAssignment(nil, true, "", func() (string, error) {
		generateCalls++
		if generateCalls == 1 {
			return "aaaaaaaaaaaac", nil
		}
		return "aaaaaaaaaaaae", nil
	})

	for _, expected := range []string{"aaaaaaaaaaaac", "aaaaaaaaaaaae"} {
		sandbox := sandboxcr.AsSandbox(&agentsv1alpha1.Sandbox{}, nil)
		require.NoError(t, modifier(sandbox))
		assert.Equal(t, expected, sandbox.GetLabels()[sandboxid.LabelKey])
	}
	assert.Equal(t, 2, generateCalls)
}
