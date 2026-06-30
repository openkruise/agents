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

package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// TestResolveServerTimeout verifies that a positive seconds value yields a
// finite timeout, while an absent (zero) or non-positive value yields
// noServerTimeout, leaving the operation bounded only by the client context.
func TestResolveServerTimeout(t *testing.T) {
	tests := []struct {
		name     string
		seconds  int
		expected time.Duration
	}{
		{
			name:     "absent or non-positive yields no server timeout",
			seconds:  0,
			expected: noServerTimeout,
		},
		{
			name:     "positive yields finite timeout",
			seconds:  30,
			expected: 30 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, resolveServerTimeout(tt.seconds))
		})
	}
}

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

func TestCreateSandboxWithClaim_NamingExtensionRejected(t *testing.T) {
	user := &models.CreatedTeamAPIKey{ID: uuid.New(), Name: "test-user"}
	tests := []struct {
		name      string
		extension models.NewSandboxRequestExtension
	}{
		{
			name: "sandbox name",
			extension: models.NewSandboxRequestExtension{
				Name: "test-sandbox",
			},
		},
		{
			name: "sandbox generate name",
			extension: models.NewSandboxRequestExtension{
				GenerateName: "test-sandbox-",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := &Controller{}
			request := models.NewSandboxRequest{
				TemplateID: "test-template",
				Extensions: tt.extension,
			}

			var apiErr *web.ApiError
			require.NotPanics(t, func() {
				_, apiErr = ctrl.createSandboxWithClaim(context.Background(), request, user)
			})
			require.NotNil(t, apiErr)
			assert.Equal(t, http.StatusBadRequest, apiErr.Code)
			assert.Contains(t, apiErr.Message, "only supported for clone")
		})
	}
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

// TestCreateSandboxWithClone_Naming asserts that Extensions.Name and
// Extensions.GenerateName flow through the controller into the
// CloneSandboxOptions and onto the resulting Sandbox ObjectMeta.
func TestCreateSandboxWithClone_Naming(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Name: "team-a-user",
		Team: &models.Team{Name: "team-a"},
	}
	cleanupCP := CreateCheckpointAndTemplateInNamespace(
		t, controller, "team-a", "tmpl", "cp-id",
		user.ID.String(), "src-sbx", "2024-07-01T00:00:01Z",
	)
	defer cleanupCP()

	var capturedName, capturedGenerateName string
	origCreateSandbox := sandboxcr.DefaultCreateSandbox
	sandboxcr.DefaultCreateSandbox = func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, c ctrlclient.Client) (*agentsv1alpha1.Sandbox, error) {
		capturedName = sbx.Name
		capturedGenerateName = sbx.GenerateName
		// The fake client does not auto-resolve GenerateName; assign a unique
		// Name so Create succeeds. Capture happens before mutation.
		if sbx.Name == "" {
			sbx.Name = sbx.GenerateName + uuid.NewString()[:8]
		}
		created, err := origCreateSandbox(ctx, sbx, c)
		if err != nil {
			return nil, err
		}
		created.Status = agentsv1alpha1.SandboxStatus{
			Phase:              agentsv1alpha1.SandboxRunning,
			ObservedGeneration: created.Generation,
			Conditions: []metav1.Condition{
				{
					Type:   string(agentsv1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: agentsv1alpha1.SandboxReadyReasonPodReady,
				},
			},
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "1.2.3.4"},
		}
		if err := c.Status().Update(ctx, created); err != nil {
			return nil, err
		}
		return created, nil
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateSandbox = origCreateSandbox })

	tests := []struct {
		name               string
		ext                models.NewSandboxRequestExtension
		expectName         string
		expectGenerateName string
	}{
		{
			name: "explicit name passes through",
			ext: models.NewSandboxRequestExtension{
				Name:            "explicit-sbx",
				CreateOnNoStock: true,
			},
			expectName:         "explicit-sbx",
			expectGenerateName: "",
		},
		{
			name: "explicit generateName passes through",
			ext: models.NewSandboxRequestExtension{
				GenerateName:    "pool-",
				CreateOnNoStock: true,
			},
			expectName:         "",
			expectGenerateName: "pool-",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			capturedName = ""
			capturedGenerateName = ""
			request := models.NewSandboxRequest{
				TemplateID: "cp-id",
				Timeout:    600,
				Extensions: tt.ext,
			}
			_, apiErr := controller.createSandboxWithClone(t.Context(), request, user)
			require.Nil(t, apiErr)
			assert.Equal(t, tt.expectName, capturedName, "ObjectMeta.Name should reflect Extensions.Name")
			assert.Equal(t, tt.expectGenerateName, capturedGenerateName, "ObjectMeta.GenerateName should reflect Extensions.GenerateName")
		})
	}
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

func TestMapInfraErrorToApiError(t *testing.T) {
	tests := []struct {
		name         string
		err          error
		expectedCode int
	}{
		{
			name:         "ErrorBadRequest maps to 400",
			err:          managererrors.NewError(managererrors.ErrorBadRequest, "quota exceeded"),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "ErrorNotFound maps to 400",
			err:          managererrors.NewError(managererrors.ErrorNotFound, "template not found"),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "ErrorConflict maps to 409",
			err:          managererrors.NewError(managererrors.ErrorConflict, "sandbox already exists"),
			expectedCode: http.StatusConflict,
		},
		{
			name:         "ErrorInternal maps to 500",
			err:          managererrors.NewError(managererrors.ErrorInternal, "platform issue"),
			expectedCode: http.StatusInternalServerError,
		},
		{
			name:         "plain error maps to 500",
			err:          fmt.Errorf("some unknown error"),
			expectedCode: http.StatusInternalServerError,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			apiErr := mapInfraErrorToApiError(tt.err)
			assert.Equal(t, tt.expectedCode, apiErr.Code)
			assert.Contains(t, apiErr.Message, tt.err.Error())
		})
	}
}
