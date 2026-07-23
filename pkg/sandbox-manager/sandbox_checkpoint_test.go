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
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

type checkpointSandboxStub struct {
	infra.Sandbox
	namespace   string
	name        string
	labels      map[string]string
	annotations map[string]string
	received    []infra.CreateCheckpointOptions
}

func (s *checkpointSandboxStub) GetNamespace() string {
	return s.namespace
}

func (s *checkpointSandboxStub) GetName() string {
	return s.name
}

func (s *checkpointSandboxStub) GetLabels() map[string]string {
	return s.labels
}

func (s *checkpointSandboxStub) GetAnnotations() map[string]string {
	return s.annotations
}

func (s *checkpointSandboxStub) CreateCheckpoint(_ context.Context, opts infra.CreateCheckpointOptions) (string, error) {
	s.received = append(s.received, opts)
	return "checkpoint-id", nil
}

func TestSandboxManagerCreateCheckpoint(t *testing.T) {
	tests := []struct {
		name         string
		labels       map[string]string
		annotations  map[string]string
		transitionTo map[string]string
		expectedIDs  []string
	}{
		{name: "legacy fallback", expectedIDs: []string{"team-a--sandbox-a"}},
		{name: "empty label falls back", labels: map[string]string{agentsv1alpha1.LabelSandboxID: ""}, expectedIDs: []string{"team-a--sandbox-a"}},
		{name: "short label is preserved", labels: map[string]string{agentsv1alpha1.LabelSandboxID: "opaque-short-id"}, expectedIDs: []string{"opaque-short-id"}},
		{
			name:        "Sandbox annotation with shared key does not affect resolution",
			annotations: map[string]string{agentsv1alpha1.AnnotationSandboxID: "annotation-must-be-ignored"},
			expectedIDs: []string{"team-a--sandbox-a"},
		},
		{
			name:         "old Checkpoint identity stays legacy after Sandbox transitions",
			transitionTo: map[string]string{agentsv1alpha1.LabelSandboxID: "opaque-short-id"},
			expectedIDs:  []string{"team-a--sandbox-a", "opaque-short-id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := &checkpointSandboxStub{
				namespace:   "team-a",
				name:        "sandbox-a",
				labels:      tt.labels,
				annotations: tt.annotations,
			}
			manager := &SandboxManager{}
			checkpointID, err := manager.CreateCheckpoint(t.Context(), sandbox, infra.CreateCheckpointOptions{SandboxID: "caller-spoofed-id"})
			require.NoError(t, err)
			assert.Equal(t, "checkpoint-id", checkpointID)
			if tt.transitionTo != nil {
				sandbox.labels = tt.transitionTo
				checkpointID, err = manager.CreateCheckpoint(t.Context(), sandbox, infra.CreateCheckpointOptions{SandboxID: "second-spoofed-id"})
				require.NoError(t, err)
				assert.Equal(t, "checkpoint-id", checkpointID)
			}
			require.Len(t, sandbox.received, len(tt.expectedIDs))
			for i := range tt.expectedIDs {
				assert.Equal(t, tt.expectedIDs[i], sandbox.received[i].SandboxID)
			}
		})
	}
}
