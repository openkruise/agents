package checkpoint

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

func TestPropagateAnnotationsToCheckpoint(t *testing.T) {
	csiKey := models.ExtensionKeyClaimWithCSIMount_MountConfig

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
	csiKey := models.ExtensionKeyClaimWithCSIMount_MountConfig

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
