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

package sandboxcr

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
)

func TestPropagateAnnotationsToCheckpoint(t *testing.T) {
	csiKey := v1alpha1.AnnotationCSIVolumeConfig

	tests := []struct {
		name               string
		sbx                *v1alpha1.Sandbox
		cp                 *v1alpha1.Checkpoint
		expectedAnnotation map[string]string
	}{
		{
			name: "sandbox has necessary annotation - propagated to checkpoint",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey: `[{"driver":"nfs"}]`,
					},
				},
			},
			cp: &v1alpha1.Checkpoint{},
			expectedAnnotation: map[string]string{
				csiKey: `[{"driver":"nfs"}]`,
			},
		},
		{
			name: "sandbox has no annotations - checkpoint annotations unchanged",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			cp:                 &v1alpha1.Checkpoint{},
			expectedAnnotation: map[string]string{},
		},
		{
			name: "sandbox annotation is empty string - not propagated",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey: "",
					},
				},
			},
			cp:                 &v1alpha1.Checkpoint{},
			expectedAnnotation: map[string]string{},
		},
		{
			name: "checkpoint already has annotations - necessary keys merged",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey: `[{"driver":"oss"}]`,
					},
				},
			},
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"existing-key": "existing-value",
					},
				},
			},
			expectedAnnotation: map[string]string{
				"existing-key": "existing-value",
				csiKey:         `[{"driver":"oss"}]`,
			},
		},
		{
			name: "sandbox has extra non-necessary annotations - only necessary keys propagated",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey:           `[{"driver":"nfs"}]`,
						"some-other-key": "should-not-propagate",
					},
				},
			},
			cp: &v1alpha1.Checkpoint{},
			expectedAnnotation: map[string]string{
				csiKey: `[{"driver":"nfs"}]`,
			},
		},
		{
			name: "sandbox with nil annotations - no panic and checkpoint annotations unchanged",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{},
			},
			cp:                 &v1alpha1.Checkpoint{},
			expectedAnnotation: nil,
		},
		{
			name: "sandbox with nil annotations - checkpoint existing annotations preserved",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{},
			},
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"existing-key": "existing-value",
					},
				},
			},
			expectedAnnotation: map[string]string{
				"existing-key": "existing-value",
			},
		},
		{
			name: "sandbox has security-prefixed annotations - propagated to checkpoint",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						identity.AnnotationAgentName:        "my-agent",
						identity.AgentKeyTokenRefreshStatus: `{"accessTokenExpiration":"2026-01-01T00:00:00Z"}`,
						"some-other-key":                    "should-not-propagate",
					},
				},
			},
			cp: &v1alpha1.Checkpoint{},
			expectedAnnotation: map[string]string{
				identity.AnnotationAgentName:        "my-agent",
				identity.AgentKeyTokenRefreshStatus: `{"accessTokenExpiration":"2026-01-01T00:00:00Z"}`,
			},
		},
		{
			name: "security-prefixed annotation with empty value - not propagated",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						identity.AnnotationAgentName: "",
					},
				},
			},
			cp:                 &v1alpha1.Checkpoint{},
			expectedAnnotation: map[string]string{},
		},
		{
			name: "sandbox has both necessary and security-prefixed annotations - both propagated",
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey:                       `[{"driver":"nfs"}]`,
						identity.AnnotationAgentName: "my-agent",
					},
				},
			},
			cp: &v1alpha1.Checkpoint{},
			expectedAnnotation: map[string]string{
				csiKey:                       `[{"driver":"nfs"}]`,
				identity.AnnotationAgentName: "my-agent",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			PropagateAnnotationsToCheckpoint(tt.sbx, tt.cp)

			cpAnnotations := tt.cp.GetAnnotations()
			if tt.expectedAnnotation == nil {
				assert.Nil(t, cpAnnotations, "checkpoint annotations should remain nil")
				return
			}
			for key, expectedVal := range tt.expectedAnnotation {
				assert.Equal(t, expectedVal, cpAnnotations[key], "annotation %s mismatch", key)
			}
			// Verify no extra necessary keys were added
			for _, key := range necessaryAnnotationKeys {
				if _, expected := tt.expectedAnnotation[key]; !expected {
					assert.Empty(t, cpAnnotations[key], "annotation %s should not be set", key)
				}
			}
		})
	}
}

func TestRestoreAnnotationsFromCheckpoint(t *testing.T) {
	csiKey := v1alpha1.AnnotationCSIVolumeConfig

	tests := []struct {
		name               string
		cp                 *v1alpha1.Checkpoint
		sbx                *v1alpha1.Sandbox
		expectedAnnotation map[string]string
	}{
		{
			name: "checkpoint has necessary annotation - restored to sandbox",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey: `[{"driver":"nfs"}]`,
					},
				},
			},
			sbx: &v1alpha1.Sandbox{},
			expectedAnnotation: map[string]string{
				csiKey: `[{"driver":"nfs"}]`,
			},
		},
		{
			name: "checkpoint has no annotations - sandbox annotations unchanged",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			sbx:                &v1alpha1.Sandbox{},
			expectedAnnotation: map[string]string{},
		},
		{
			name: "checkpoint annotation is empty string - not restored",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey: "",
					},
				},
			},
			sbx:                &v1alpha1.Sandbox{},
			expectedAnnotation: map[string]string{},
		},
		{
			name: "sandbox already has annotations - necessary keys merged",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey: `[{"driver":"oss"}]`,
					},
				},
			},
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"existing-key": "existing-value",
					},
				},
			},
			expectedAnnotation: map[string]string{
				"existing-key": "existing-value",
				csiKey:         `[{"driver":"oss"}]`,
			},
		},
		{
			name: "checkpoint has extra non-necessary annotations - only necessary keys restored",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey:           `[{"driver":"nfs"}]`,
						"some-other-key": "should-not-restore",
					},
				},
			},
			sbx: &v1alpha1.Sandbox{},
			expectedAnnotation: map[string]string{
				csiKey: `[{"driver":"nfs"}]`,
			},
		},
		{
			name: "checkpoint with nil annotations - no panic and sandbox annotations unchanged",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{},
			},
			sbx:                &v1alpha1.Sandbox{},
			expectedAnnotation: nil,
		},
		{
			name: "checkpoint with nil annotations - sandbox existing annotations preserved",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{},
			},
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"existing-key": "existing-value",
					},
				},
			},
			expectedAnnotation: map[string]string{
				"existing-key": "existing-value",
			},
		},
		{
			name: "sandbox with nil annotations map - annotations created and restored",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						csiKey: `[{"pvName":"pv-1","mountPath":"/data"}]`,
					},
				},
			},
			sbx: &v1alpha1.Sandbox{
				ObjectMeta: metav1.ObjectMeta{},
			},
			expectedAnnotation: map[string]string{
				csiKey: `[{"pvName":"pv-1","mountPath":"/data"}]`,
			},
		},
		{
			name: "checkpoint has security-prefixed annotations - restored to sandbox",
			cp: &v1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						identity.AnnotationAgentName: "my-agent",
						"some-other-key":             "should-not-restore",
					},
				},
			},
			sbx: &v1alpha1.Sandbox{},
			expectedAnnotation: map[string]string{
				identity.AnnotationAgentName: "my-agent",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			RestoreAnnotationsFromCheckpoint(tt.cp, tt.sbx)

			sbxAnnotations := tt.sbx.GetAnnotations()
			if tt.expectedAnnotation == nil {
				assert.Nil(t, sbxAnnotations, "sandbox annotations should remain nil")
				return
			}
			for key, expectedVal := range tt.expectedAnnotation {
				assert.Equal(t, expectedVal, sbxAnnotations[key], "annotation %s mismatch", key)
			}
			// Verify no extra necessary keys were added
			for _, key := range necessaryAnnotationKeys {
				if _, expected := tt.expectedAnnotation[key]; !expected {
					assert.Empty(t, sbxAnnotations[key], "annotation %s should not be set", key)
				}
			}
		})
	}
}

func TestShouldPreserveAnnotation(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		expected bool
	}{
		{
			name:     "exact necessary key - preserved",
			key:      v1alpha1.AnnotationCSIVolumeConfig,
			expected: true,
		},
		{
			name:     "security prefix agent-name key - preserved",
			key:      identity.AnnotationAgentName,
			expected: true,
		},
		{
			name:     "security prefix token-status key - preserved",
			key:      identity.AgentKeyTokenRefreshStatus,
			expected: true,
		},
		{
			name:     "key equals security prefix exactly - preserved",
			key:      identity.SecurityMetadataPrefix,
			expected: true,
		},
		{
			name:     "arbitrary key under security prefix - preserved",
			key:      identity.SecurityMetadataPrefix + "future-key",
			expected: true,
		},
		{
			name:     "unrelated key - not preserved",
			key:      "some-other-key",
			expected: false,
		},
		{
			name:     "empty key - not preserved",
			key:      "",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, shouldPreserveAnnotation(tt.key))
		})
	}
}

func TestCopyPreservedAnnotations(t *testing.T) {
	csiKey := v1alpha1.AnnotationCSIVolumeConfig

	tests := []struct {
		name     string
		src      map[string]string
		dst      map[string]string
		expected map[string]string
	}{
		{
			name: "nil dst allocated and preserved keys copied",
			src: map[string]string{
				csiKey:                       `[{"driver":"nfs"}]`,
				identity.AnnotationAgentName: "my-agent",
				"some-other-key":             "ignored",
			},
			dst: nil,
			expected: map[string]string{
				csiKey:                       `[{"driver":"nfs"}]`,
				identity.AnnotationAgentName: "my-agent",
			},
		},
		{
			name: "empty value preserved key not copied",
			src: map[string]string{
				csiKey:                       "",
				identity.AnnotationAgentName: "",
			},
			dst:      nil,
			expected: map[string]string{},
		},
		{
			name: "existing dst entries retained and merged",
			src: map[string]string{
				identity.AnnotationAgentName: "my-agent",
			},
			dst: map[string]string{
				"existing-key": "existing-value",
			},
			expected: map[string]string{
				"existing-key":               "existing-value",
				identity.AnnotationAgentName: "my-agent",
			},
		},
		{
			name:     "empty src leaves dst unchanged",
			src:      map[string]string{},
			dst:      map[string]string{"existing-key": "existing-value"},
			expected: map[string]string{"existing-key": "existing-value"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := copyPreservedAnnotations(tt.src, tt.dst)
			assert.Equal(t, tt.expected, got)
		})
	}
}
