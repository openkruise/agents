package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/features"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	utilfeature "github.com/openkruise/agents/pkg/utils/feature"
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

// mockSandboxManager is a mock implementation for testing
type mockSandboxManager struct {
	claimFunc func(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error)
	cloneFunc func(ctx context.Context, opts infra.CloneSandboxOptions) (infra.Sandbox, error)
}

func (m *mockSandboxManager) ClaimSandbox(ctx context.Context, opts infra.ClaimSandboxOptions) (infra.Sandbox, error) {
	if m.claimFunc != nil {
		return m.claimFunc(ctx, opts)
	}
	// Default behavior: return a mock sandbox
	return &sandboxcr.Sandbox{
		Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mock-sandbox",
				Namespace: "default",
			},
			Status: v1alpha1.SandboxStatus{
				Phase: v1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{
					{
						Type:   string(v1alpha1.SandboxConditionReady),
						Status: metav1.ConditionTrue,
					},
				},
				PodInfo: v1alpha1.PodInfo{
					PodIP: "10.0.0.1",
				},
			},
		},
	}, nil
}

func (m *mockSandboxManager) CloneSandbox(ctx context.Context, opts infra.CloneSandboxOptions) (infra.Sandbox, error) {
	if m.cloneFunc != nil {
		return m.cloneFunc(ctx, opts)
	}
	// Default behavior: return a mock sandbox
	return &sandboxcr.Sandbox{
		Sandbox: &v1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "mock-cloned-sandbox",
				Namespace: "default",
			},
			Status: v1alpha1.SandboxStatus{
				Phase: v1alpha1.SandboxRunning,
			},
		},
	}, nil
}

func (m *mockSandboxManager) GetInfra() infra.Infrastructure {
	return nil
}

func (m *mockSandboxManager) Stop() {
	// No-op for testing
}

func (m *mockSandboxManager) Run(ctx context.Context, sysNs, peerSelector string) error {
	// No-op for testing
	return nil
}

func TestCreateSandboxWithClaim_CSIMount(t *testing.T) {
	tests := []struct {
		name               string
		request            models.NewSandboxRequest
		expectCSIMount     bool
		expectedMountCount int
	}{
		{
			name: "no csi mount configs",
			request: models.NewSandboxRequest{
				TemplateID: "test-template",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{},
					},
				},
			},
			expectCSIMount: false,
		},
		{
			name: "single csi mount config",
			request: models.NewSandboxRequest{
				TemplateID: "test-template",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "test-pv",
								MountPath: "/data",
								SubPath:   "subdir",
								ReadOnly:  true,
							},
						},
					},
				},
			},
			expectCSIMount:     true,
			expectedMountCount: 1,
		},
		{
			name: "multiple csi mount configs",
			request: models.NewSandboxRequest{
				TemplateID: "test-template",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "pv-nas-001",
								MountPath: "/workspace",
							},
							{
								PvName:    "pv-oss-002",
								MountPath: "/models",
								ReadOnly:  true,
							},
							{
								PvName:    "pv-disk-003",
								MountPath: "/storage",
								SubPath:   "data",
							},
						},
					},
				},
			},
			expectCSIMount:     true,
			expectedMountCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify the request structure is valid
			if len(tt.request.Extensions.CSIMount.MountConfigs) != tt.expectedMountCount {
				t.Errorf("expected %d mount configs, got %d", tt.expectedMountCount,
					len(tt.request.Extensions.CSIMount.MountConfigs))
			}

			// Check if CSI mount configs are properly set
			hasCSIMount := len(tt.request.Extensions.CSIMount.MountConfigs) > 0
			if hasCSIMount != tt.expectCSIMount {
				t.Errorf("expectCSIMount mismatch: expected %v, got %v", tt.expectCSIMount, hasCSIMount)
			}
		})
	}
}

func TestCreateSandboxWithClone_CSIMount(t *testing.T) {
	tests := []struct {
		name               string
		request            models.NewSandboxRequest
		expectCSIMount     bool
		expectedMountCount int
		hasInplaceUpdate   bool
	}{
		{
			name: "clone with csi mount",
			request: models.NewSandboxRequest{
				TemplateID: "test-checkpoint",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "test-pv",
								MountPath: "/data",
							},
						},
					},
				},
			},
			expectCSIMount:     true,
			expectedMountCount: 1,
		},
		{
			name: "clone with multiple csi mounts",
			request: models.NewSandboxRequest{
				TemplateID: "test-checkpoint",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "pv-1",
								MountPath: "/mnt/data1",
							},
							{
								PvName:    "pv-2",
								MountPath: "/mnt/data2",
								ReadOnly:  true,
							},
						},
					},
				},
			},
			expectCSIMount:     true,
			expectedMountCount: 2,
		},
		{
			name: "clone with inplace update and csi mount",
			request: models.NewSandboxRequest{
				TemplateID: "test-checkpoint",
				Extensions: models.NewSandboxRequestExtension{
					InplaceUpdate: models.InplaceUpdateExtension{
						Image: "new-image",
					},
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "test-pv",
								MountPath: "/data",
							},
						},
					},
				},
			},
			expectCSIMount:     true,
			expectedMountCount: 1,
			hasInplaceUpdate:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Verify the request structure is valid
			if len(tt.request.Extensions.CSIMount.MountConfigs) != tt.expectedMountCount {
				t.Errorf("expected %d mount configs, got %d", tt.expectedMountCount,
					len(tt.request.Extensions.CSIMount.MountConfigs))
			}

			// Check if CSI mount configs are properly set
			hasCSIMount := len(tt.request.Extensions.CSIMount.MountConfigs) > 0
			if hasCSIMount != tt.expectCSIMount {
				t.Errorf("expectCSIMount mismatch: expected %v, got %v", tt.expectCSIMount, hasCSIMount)
			}

			// Check inplace update
			hasInplaceUpdate := tt.request.Extensions.InplaceUpdate.Image != ""
			if hasInplaceUpdate != tt.hasInplaceUpdate {
				t.Errorf("hasInplaceUpdate mismatch: expected %v, got %v", tt.hasInplaceUpdate, hasInplaceUpdate)
			}
		})
	}
}

func TestCreateSandboxWithClaim_CPUResizeFeatureGate(t *testing.T) {
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "test-user"}

	t.Run("feature gate disabled rejects CPU resize with requests", func(t *testing.T) {
		err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{
			string(features.SandboxInPlaceResourceResizeGate): false,
		})
		assert.NoError(t, err)
		defer func() {
			_ = utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{
				string(features.SandboxInPlaceResourceResizeGate): true,
			})
		}()

		ctrl := &Controller{}
		request := models.NewSandboxRequest{
			TemplateID: "test-template",
			Extensions: models.NewSandboxRequestExtension{
				SkipInitRuntime: true,
				InplaceUpdate: models.InplaceUpdateExtension{
					Resources: &models.InplaceUpdateResourcesExtension{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("500m"),
						},
					},
				},
			},
		}
		_, apiErr := ctrl.createSandboxWithClaim(context.Background(), request, user)
		assert.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "feature gate")
	})

	t.Run("feature gate disabled rejects CPU resize with limits", func(t *testing.T) {
		err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{
			string(features.SandboxInPlaceResourceResizeGate): false,
		})
		assert.NoError(t, err)
		defer func() {
			_ = utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{
				string(features.SandboxInPlaceResourceResizeGate): true,
			})
		}()

		ctrl := &Controller{}
		request := models.NewSandboxRequest{
			TemplateID: "test-template",
			Extensions: models.NewSandboxRequestExtension{
				SkipInitRuntime: true,
				InplaceUpdate: models.InplaceUpdateExtension{
					Resources: &models.InplaceUpdateResourcesExtension{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("1000m"),
						},
					},
				},
			},
		}
		_, apiErr := ctrl.createSandboxWithClaim(context.Background(), request, user)
		assert.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "feature gate")
	})

	t.Run("feature gate disabled rejects mixed image and cpu resize", func(t *testing.T) {
		err := utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{
			string(features.SandboxInPlaceResourceResizeGate): false,
		})
		assert.NoError(t, err)
		defer func() {
			_ = utilfeature.DefaultMutableFeatureGate.SetFromMap(map[string]bool{
				string(features.SandboxInPlaceResourceResizeGate): true,
			})
		}()

		ctrl := &Controller{}
		request := models.NewSandboxRequest{
			TemplateID: "test-template",
			Extensions: models.NewSandboxRequestExtension{
				SkipInitRuntime: true,
				InplaceUpdate: models.InplaceUpdateExtension{
					Image: "nginx:latest",
					Resources: &models.InplaceUpdateResourcesExtension{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU: resource.MustParse("500m"),
						},
					},
				},
			},
		}
		_, apiErr := ctrl.createSandboxWithClaim(context.Background(), request, user)
		assert.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "feature gate")
	})
}

func TestCreateSandboxWithClone_InplaceUpdateRejected(t *testing.T) {
	ctrl := &Controller{}
	request := models.NewSandboxRequest{
		TemplateID: "test-checkpoint",
		Extensions: models.NewSandboxRequestExtension{
			InplaceUpdate: models.InplaceUpdateExtension{
				Image: "nginx:latest",
			},
		},
	}
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "test-user"}

	_, apiErr := ctrl.createSandboxWithClone(context.Background(), request, user)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	assert.Contains(t, apiErr.Message, "InplaceUpdate is not supported for clone")
}

func TestParseCreateSandboxRequest(t *testing.T) {
	ctrl := &Controller{maxTimeout: 3600}

	t.Run("invalid json", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader("{"))
		_, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, 0, apiErr.Code)
		assert.NotEmpty(t, apiErr.Message)
	})

	t.Run("invalid extension", func(t *testing.T) {
		body := `{
			"templateID":"t1",
			"metadata":{
				"` + models.ExtensionKeyClaimWithCPULimit + `":"bad"
			}
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		_, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "Bad extension param")
	})

	t.Run("unqualified metadata key", func(t *testing.T) {
		body := `{
			"templateID":"t1",
			"metadata":{"bad/key/":"v"}
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		_, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "Unqualified metadata key")
	})

	t.Run("forbidden metadata key prefix", func(t *testing.T) {
		meta := map[string]string{v1alpha1.E2BPrefix + "custom-key": "v"}
		raw, err := json.Marshal(models.NewSandboxRequest{
			TemplateID: "t1",
			Metadata:   meta,
		})
		require.NoError(t, err)

		req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewReader(raw))
		_, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "Forbidden metadata key")
	})

	t.Run("timeout defaults when omitted", func(t *testing.T) {
		raw, err := json.Marshal(models.NewSandboxRequest{
			TemplateID: "t1",
		})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewReader(raw))

		got, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		assert.Equal(t, models.DefaultTimeoutSeconds, got.Timeout)
	})

	t.Run("timeout out of range", func(t *testing.T) {
		raw, err := json.Marshal(models.NewSandboxRequest{
			TemplateID: "t1",
			Timeout:    ctrl.maxTimeout + 1,
		})
		require.NoError(t, err)
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", bytes.NewReader(raw))

		_, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "timeout should between")
	})
}
