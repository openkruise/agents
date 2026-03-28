package e2b

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// TestCsiMountOptionsConfigRecord tests the csiMountOptionsConfigRecord function
func TestCsiMountOptionsConfigRecord(t *testing.T) {
	tests := []struct {
		name                  string
		request               models.NewSandboxRequest
		initialAnnotations    map[string]string
		expectedAnnotationKey string
		expectedAnnotationVal string
		shouldSet             bool
	}{
		{
			name: "empty mount configs",
			request: models.NewSandboxRequest{
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{},
					},
				},
			},
			shouldSet: false,
		},
		{
			name: "single mount config with all fields",
			request: models.NewSandboxRequest{
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								MountID:   "mount-123",
								PvName:    "pv-nas-001",
								MountPath: "/data",
								SubPath:   "subdir",
								ReadOnly:  true,
							},
						},
					},
				},
				Metadata: map[string]string{
					"user-id": "user-456",
				},
			},
			initialAnnotations:    map[string]string{},
			expectedAnnotationKey: models.ExtensionKeyClaimWithCSIMount_MountConfig,
			expectedAnnotationVal: `[{"mountID":"mount-123","pvName":"pv-nas-001","mountPath":"/data","subPath":"subdir","readOnly":true}]`,
			shouldSet:             true,
		},
		{
			name: "multiple mount configs with optional fields omitted",
			request: models.NewSandboxRequest{
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "pv-nas-001",
								MountPath: "/data",
							},
							{
								PvName:    "pv-oss-002",
								MountPath: "/models",
								ReadOnly:  true,
							},
						},
					},
				},
			},
			initialAnnotations:    map[string]string{"existing-key": "existing-val"},
			expectedAnnotationKey: models.ExtensionKeyClaimWithCSIMount_MountConfig,
			expectedAnnotationVal: `[{"pvName":"pv-nas-001","mountPath":"/data"},{"pvName":"pv-oss-002","mountPath":"/models","readOnly":true}]`,
			shouldSet:             true,
		},
		{
			name: "with metadata merging",
			request: models.NewSandboxRequest{
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "pv-test",
								MountPath: "/workspace",
							},
						},
					},
				},
			},
			initialAnnotations: map[string]string{
				"old-key": "old-val",
			},
			expectedAnnotationKey: models.ExtensionKeyClaimWithCSIMount_MountConfig,
			expectedAnnotationVal: `[{"pvName":"pv-test","mountPath":"/workspace"}]`,
			shouldSet:             true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create mock sandbox
			mockSbx := &sandboxcr.Sandbox{
				Sandbox: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test-sandbox",
						Namespace:   "default",
						Annotations: tt.initialAnnotations,
					},
				},
			}

			// Create controller instance
			ctrl := &Controller{}

			// Call the function
			ctx := context.Background()
			ctrl.csiMountOptionsConfigRecord(ctx, mockSbx, tt.request)

			// Verify results
			annotations := mockSbx.GetAnnotations()

			if !tt.shouldSet {
				// Should not set any annotation when mount configs are empty
				if len(annotations) != len(tt.initialAnnotations) {
					t.Errorf("expected no annotations to be added, got %d", len(annotations))
				}
				return
			}

			// Check if expected annotation is set
			val, exists := annotations[tt.expectedAnnotationKey]
			if !exists {
				t.Errorf("expected annotation %q to exist", tt.expectedAnnotationKey)
				return
			}

			// Verify the annotation value (parse JSON for comparison to avoid ordering issues)
			var expectedConfigs, actualConfigs []v1alpha1.CSIMountConfig
			if err := json.Unmarshal([]byte(tt.expectedAnnotationVal), &expectedConfigs); err != nil {
				t.Fatalf("failed to unmarshal expected value: %v", err)
			}
			if err := json.Unmarshal([]byte(val), &actualConfigs); err != nil {
				t.Fatalf("failed to unmarshal actual value: %v", err)
			}

			if !reflect.DeepEqual(expectedConfigs, actualConfigs) {
				t.Errorf("csi mount config mismatch:\nexpected: %#v\ngot:      %#v", expectedConfigs, actualConfigs)
			}

			if !reflect.DeepEqual(expectedConfigs, actualConfigs) {
				t.Errorf("csi mount config mismatch:\nexpected: %#v\ngot:      %#v", expectedConfigs, actualConfigs)
			}

			// Verify existing annotations are preserved
			if tt.initialAnnotations != nil {
				for k, v := range tt.initialAnnotations {
					if annotations[k] != v {
						t.Errorf("expected existing annotation %q=%q, got %q", k, v, annotations[k])
					}
				}
			}
		})
	}
}
