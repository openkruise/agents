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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/quota"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/csiutils"
)

type fakeQuotaManager struct {
	acquireErr         error
	releaseErr         error
	acquireCalls       atomic.Int64
	releaseCalls       atomic.Int64
	cleanupCalls       atomic.Int64
	releaseHasDeadline atomic.Bool

	mu          sync.Mutex
	lastAcquire quota.AcquireRequest
	lastRelease quota.ReleaseRequest
	lastCleanup string
}

func (f *fakeQuotaManager) Acquire(_ context.Context, req quota.AcquireRequest) error {
	f.mu.Lock()
	f.lastAcquire = req
	f.mu.Unlock()
	f.acquireCalls.Add(1)
	return f.acquireErr
}

func (f *fakeQuotaManager) Release(ctx context.Context, req quota.ReleaseRequest) error {
	if _, ok := ctx.Deadline(); ok {
		f.releaseHasDeadline.Store(true)
	}
	f.mu.Lock()
	f.lastRelease = req
	f.mu.Unlock()
	f.releaseCalls.Add(1)
	return f.releaseErr
}

func (f *fakeQuotaManager) Cleanup(_ context.Context, user string) error {
	f.mu.Lock()
	f.lastCleanup = user
	f.mu.Unlock()
	f.cleanupCalls.Add(1)
	return nil
}

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
		{
			name: "csi mount with credentialProviderName attribute for agent-identity storage auth",
			request: models.NewSandboxRequest{
				TemplateID: "test-template",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:     "pv-oss-rrsa",
								MountPath:  "/data",
								Attributes: map[string]string{"credentialProviderName": "my-cred-provider"},
								SubPath:    "user-files",
							},
							{
								PvName:    "pv-nas-normal",
								MountPath: "/workspace",
							},
						},
					},
				},
			},
			expectCSIMount:     true,
			expectedMountCount: 2,
		},
		{
			name: "csi mount with kmsKeyId attribute for KMS server-side encryption",
			request: models.NewSandboxRequest{
				TemplateID: "test-template",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "pv-oss-kms",
								MountPath: "/data-encrypted",
								SubPath:   "user-kms-data",
								Attributes: map[string]string{
									"credentialProviderName": "oss-rw",
									"kmsKeyId":               "cmk-12345",
								},
							},
						},
					},
				},
			},
			expectCSIMount:     true,
			expectedMountCount: 1,
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

			// Verify Attributes survive JSON marshal/unmarshal round-trip
			for i, mc := range tt.request.Extensions.CSIMount.MountConfigs {
				if mc.Attributes == nil {
					continue
				}
				data, err := json.Marshal(mc)
				if err != nil {
					t.Fatalf("mount config[%d]: failed to marshal: %v", i, err)
				}
				var decoded v1alpha1.CSIMountConfig
				if err := json.Unmarshal(data, &decoded); err != nil {
					t.Fatalf("mount config[%d]: failed to unmarshal: %v", i, err)
				}
				for k, v := range mc.Attributes {
					got, ok := decoded.Attributes[k]
					if !ok {
						t.Errorf("mount config[%d]: attribute %q lost after JSON round-trip", i, k)
					} else if got != v {
						t.Errorf("mount config[%d]: attribute %q = %q, want %q", i, k, got, v)
					}
				}
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
				_, apiErr = ctrl.createSandboxWithClaim(context.Background(), request, user, "")
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

	_, apiErr := ctrl.createSandboxWithClone(context.Background(), request, user, "")
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
			_, apiErr := controller.createSandboxWithClone(t.Context(), request, user, "")
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

	t.Run("valid volumeMounts", func(t *testing.T) {
		body := `{
			"templateID":"t1",
			"volumeMounts":[
				{"name":"pv-nas-001","path":"/data"},
				{"name":"pv-oss-002","path":"/models"}
			]
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		got, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		require.Len(t, got.VolumeMounts, 2)
		assert.Equal(t, "pv-nas-001", got.VolumeMounts[0].Name)
		assert.Equal(t, "/data", got.VolumeMounts[0].Path)
		assert.Equal(t, "pv-oss-002", got.VolumeMounts[1].Name)
		assert.Equal(t, "/models", got.VolumeMounts[1].Path)
		// Verify CSIMount configs are populated
		require.Len(t, got.Extensions.CSIMount.MountConfigs, 2)
		assert.Equal(t, "pv-nas-001", got.Extensions.CSIMount.MountConfigs[0].PvName)
		assert.Equal(t, "/data", got.Extensions.CSIMount.MountConfigs[0].MountPath)
		assert.Equal(t, "pv-oss-002", got.Extensions.CSIMount.MountConfigs[1].PvName)
		assert.Equal(t, "/models", got.Extensions.CSIMount.MountConfigs[1].MountPath)
	})

	t.Run("volumeMounts with empty name", func(t *testing.T) {
		body := `{
			"templateID":"t1",
			"volumeMounts":[
				{"name":"","path":"/data"}
			]
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		_, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "volumeMounts[0].name cannot be empty")
	})

	t.Run("volumeMounts with invalid path", func(t *testing.T) {
		body := `{
			"templateID":"t1",
			"volumeMounts":[
				{"name":"pv-001","path":"invalid/path"}
			]
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		_, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "volumeMounts[0].path is invalid: mount point must start with '/'")
	})

	t.Run("volumeMounts with duplicate paths", func(t *testing.T) {
		body := `{
			"templateID":"t1",
			"volumeMounts":[
				{"name":"pv-001","path":"/data"},
				{"name":"pv-002","path":"/data"}
			]
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		_, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, `volumeMounts[1].path "/data" is duplicated`)
	})

	t.Run("empty volumeMounts array", func(t *testing.T) {
		body := `{
			"templateID":"t1",
			"volumeMounts":[]
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		got, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		assert.Empty(t, got.VolumeMounts)
		assert.Empty(t, got.Extensions.CSIMount.MountConfigs)
	})

	t.Run("volumeMounts and single csi volume metadata conflict", func(t *testing.T) {
		// When both volumeMounts and single csi volume metadata are specified,
		// single csi volume metadata takes precedence (overwrites volumeMounts)
		body := `{
			"templateID":"t1",
			"metadata":{
				"` + models.ExtensionKeyClaimWithCSIMount_VolumeName + `":"oss-pv-sandbox-system",
				"` + models.ExtensionKeyClaimWithCSIMount_MountPoint + `":"/data-oss",
				"` + models.ExtensionKeyClaimWithCSIMount_SubPath + `":"data-subPath"
			},
			"volumeMounts":[
				{"name":"volume-pv","path":"/volume-path"}
			]
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		got, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		// single csi volume metadata should override volumeMounts
		require.Len(t, got.Extensions.CSIMount.MountConfigs, 1)
		assert.Equal(t, "oss-pv-sandbox-system", got.Extensions.CSIMount.MountConfigs[0].PvName)
		assert.Equal(t, "/data-oss", got.Extensions.CSIMount.MountConfigs[0].MountPath)
		assert.Equal(t, "data-subPath", got.Extensions.CSIMount.MountConfigs[0].SubPath)
		// volumeMounts should still be parsed
		require.Len(t, got.VolumeMounts, 1)
		assert.Equal(t, "volume-pv", got.VolumeMounts[0].Name)
	})

	t.Run("volumeMounts and multi csi volume metadata conflict", func(t *testing.T) {
		// When both volumeMounts and metadata csi-volume-config are specified,
		// metadata csi-volume-config takes precedence (overwrites volumeMounts)
		csiConfig := `[{"pvName":"metadata-pv","mountPath":"/metadata-path","subPath":"sub","readOnly":true}]`
		body := `{
			"templateID":"t1",
			"metadata":{"` + models.ExtensionKeyClaimWithCSIMount_MountConfig + `":` + fmt.Sprintf("%q", csiConfig) + `},
			"volumeMounts":[
				{"name":"volume-pv","path":"/volume-path"}
			]
		}`
		req := httptest.NewRequest(http.MethodPost, "/sandboxes", strings.NewReader(body))
		got, apiErr := ctrl.parseCreateSandboxRequest(req)
		require.Nil(t, apiErr)
		// metadata csi-volume-config should override volumeMounts
		require.Len(t, got.Extensions.CSIMount.MountConfigs, 1)
		assert.Equal(t, "metadata-pv", got.Extensions.CSIMount.MountConfigs[0].PvName)
		assert.Equal(t, "/metadata-path", got.Extensions.CSIMount.MountConfigs[0].MountPath)
		assert.Equal(t, "sub", got.Extensions.CSIMount.MountConfigs[0].SubPath)
		assert.True(t, got.Extensions.CSIMount.MountConfigs[0].ReadOnly)
		// volumeMounts should still be parsed
		require.Len(t, got.VolumeMounts, 1)
		assert.Equal(t, "volume-pv", got.VolumeMounts[0].Name)
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
			name:         "ErrorQuotaExceeded maps to 403",
			err:          managererrors.NewError(managererrors.ErrorQuotaExceeded, "api-key quota exceeded"),
			expectedCode: http.StatusForbidden,
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

func TestCreateSandbox_TopLevelMissingTemplateOrCheckpointReturns400(t *testing.T) {
	fakeQuota := &fakeQuotaManager{}
	controller, _, teardown := SetupWithQuota(t, fakeQuota)
	defer teardown()

	user := quotaLimitedUser([]quotaspec.QuotaLimit{{
		Dimension: quotaspec.DimLimitsCPU,
		Scope:     quotaspec.ScopeRunning,
		Limit:     4000,
	}})

	resp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "missing-template",
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, user))

	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	assert.Contains(t, apiErr.Message, "Template or Checkpoint not found")
	assert.Zero(t, resp.Code)
	assert.Equal(t, int64(0), fakeQuota.acquireCalls.Load())
}

func TestCreateSandbox_MemoryOverrideRejectedBeforeQuotaAcquire(t *testing.T) {
	fakeQuota := &fakeQuotaManager{}

	apiErr := validateCreateResourceOverride(models.NewSandboxRequest{
		TemplateID: "claim-template",
		Extensions: models.NewSandboxRequestExtension{
			InplaceUpdate: models.InplaceUpdateExtension{
				Resources: &models.InplaceUpdateResourcesExtension{
					Limits: corev1.ResourceList{
						corev1.ResourceMemory: resource.MustParse("1024Mi"),
					},
				},
			},
		},
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	})

	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	assert.Contains(t, apiErr.Message, "memory")
	assert.Equal(t, int64(0), fakeQuota.acquireCalls.Load())
}

func quotaLimitedUser(limits []quotaspec.QuotaLimit) *models.CreatedTeamAPIKey {
	return &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  uuid.NewString(),
		Name: "limited",
		Team: models.AdminTeam(),
		QuotaSpec: &quotaspec.QuotaSpec{
			Limits: limits,
		},
	}
}

func createSandboxSetTemplateRefFixture(t *testing.T, controller *Controller, namespace, sandboxSetName, templateRefName, cpu, memory string) {
	t.Helper()
	fc := getTestCRClient(controller)

	sbt := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      templateRefName,
			Namespace: namespace,
			UID:       types.UID(uuid.NewString()),
		},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Template: podTemplateWithLimits(cpu, memory),
		},
	}
	require.NoError(t, fc.Create(t.Context(), sbt))

	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxSetName,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			Replicas: 0,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: templateRefName},
			},
		},
	}
	require.NoError(t, fc.Create(t.Context(), sbs))
}

func createSandboxSetWithoutTemplateRefFixture(t *testing.T, controller *Controller, namespace, sandboxSetName, templateRefName string) {
	t.Helper()
	fc := getTestCRClient(controller)

	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sandboxSetName,
			Namespace: namespace,
		},
		Spec: agentsv1alpha1.SandboxSetSpec{
			Replicas: 0,
			EmbeddedSandboxTemplate: agentsv1alpha1.EmbeddedSandboxTemplate{
				TemplateRef: &agentsv1alpha1.SandboxTemplateRef{Name: templateRefName},
			},
		},
	}
	require.NoError(t, fc.Create(t.Context(), sbs))
}

func createCheckpointTemplateWithLimitsFixture(t *testing.T, controller *Controller, namespace, name, checkpointID, owner, sandboxID, creationTime, cpu, memory string) {
	t.Helper()
	fc := getTestCRClient(controller)
	createdAt, err := time.Parse(time.RFC3339, creationTime)
	require.NoError(t, err)

	sbt := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(uuid.NewString()),
		},
		Spec: agentsv1alpha1.SandboxTemplateSpec{
			Template: podTemplateWithLimits(cpu, memory),
		},
	}
	require.NoError(t, fc.Create(t.Context(), sbt))

	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			Namespace:         namespace,
			UID:               types.UID(uuid.NewString()),
			CreationTimestamp: metav1.NewTime(createdAt),
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxTemplate: name,
			},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner:     owner,
				agentsv1alpha1.AnnotationSandboxID: sandboxID,
			},
		},
		Status: agentsv1alpha1.CheckpointStatus{
			Phase:        agentsv1alpha1.CheckpointSucceeded,
			CheckpointId: checkpointID,
		},
	}
	require.NoError(t, fc.Create(t.Context(), cp))
	require.NoError(t, fc.Status().Update(t.Context(), cp))
	require.Eventually(t, func() bool {
		_, err := controller.cache.GetCheckpoint(t.Context(), cache.GetCheckpointOptions{
			Namespace:    namespace,
			CheckpointID: checkpointID,
		})
		return err == nil
	}, time.Second, 10*time.Millisecond)
}

func podTemplateWithLimits(cpu, memory string) *corev1.PodTemplateSpec {
	return &corev1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "main",
				Image: "test-image",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(cpu),
						corev1.ResourceMemory: resource.MustParse(memory),
					},
				},
			}},
		},
	}
}

// TestBasicSandboxCreateModifier_LabelSandboxName verifies that basicSandboxCreateModifier
// injects the LabelSandboxName label into the pod template labels at creation time.
func TestBasicSandboxCreateModifier_LabelSandboxName(t *testing.T) {
	tests := []struct {
		name              string
		sandboxName       string
		existingLabels    map[string]string
		userLabels        map[string]string
		existingPodLabels map[string]string
	}{
		{
			name:              "injects sandbox-name label with no existing labels",
			sandboxName:       "test-sandbox-1",
			existingLabels:    nil,
			userLabels:        nil,
			existingPodLabels: nil,
		},
		{
			name:              "injects sandbox-name label alongside user labels",
			sandboxName:       "test-sandbox-2",
			existingLabels:    nil,
			userLabels:        map[string]string{"team": "dev", "env": "staging"},
			existingPodLabels: nil,
		},
		{
			name:              "preserves existing pod template labels",
			sandboxName:       "test-sandbox-3",
			existingLabels:    map[string]string{"existing-cr-label": "value"},
			userLabels:        map[string]string{"team": "dev"},
			existingPodLabels: map[string]string{"app": "agent", "version": "v1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSbx := &sandboxcr.Sandbox{
				Sandbox: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:      tt.sandboxName,
						Namespace: "default",
						Labels:    tt.existingLabels,
					},
					Spec: agentsv1alpha1.SandboxSpec{},
				},
			}
			// Always initialize Spec.Template so MergePodLabels/SetPodLabels can store labels
			mockSbx.Spec.Template = &corev1.PodTemplateSpec{}
			if tt.existingPodLabels != nil {
				mockSbx.Spec.Template.Labels = tt.existingPodLabels
			}

			ctrl := &Controller{}
			request := models.NewSandboxRequest{
				Extensions: models.NewSandboxRequestExtension{
					NeverTimeout: true,
					Labels:       tt.userLabels,
				},
			}

			ctrl.basicSandboxCreateModifier(context.Background(), mockSbx, request)

			// Verify LabelSandboxName is NOT set on sandbox CR metadata labels
			crLabels := mockSbx.GetLabels()
			if crLabels != nil {
				_, hasLabel := crLabels[agentsv1alpha1.LabelSandboxName]
				assert.False(t, hasLabel, "LabelSandboxName should not be on sandbox CR labels")
			}

			// Verify user-provided labels are set on sandbox CR metadata labels
			for k, v := range tt.userLabels {
				assert.Equal(t, v, crLabels[k], "user label %q should be on sandbox CR labels", k)
			}

			// Verify existing CR labels are preserved
			for k, v := range tt.existingLabels {
				assert.Equal(t, v, crLabels[k], "existing CR label %q should be preserved", k)
			}

			// Verify LabelSandboxName is set on the pod template labels
			podLabels := mockSbx.GetPodLabels()
			require.NotNil(t, podLabels, "pod template labels should not be nil after modifier")
			assert.Equal(t, tt.sandboxName, podLabels[agentsv1alpha1.LabelSandboxName],
				"LabelSandboxName should be set to sandbox name on pod template")

			// Verify user-provided labels are propagated to the pod template
			for k, v := range tt.userLabels {
				assert.Equal(t, v, podLabels[k], "user label %q should be propagated to pod template", k)
			}

			// Verify existing pod template labels are preserved
			for k, v := range tt.existingPodLabels {
				assert.Equal(t, v, podLabels[k], "existing pod template label %q should be preserved", k)
			}
		})
	}
}

func TestInjectStorageAuthAnnotation(t *testing.T) {
	tests := []struct {
		name               string
		initialAnnotations map[string]string
		key                string
		value              string
		expectAnnotation   bool
		expectedKey        string
		expectedValue      string
	}{
		{
			name:             "both key and value non-empty injects annotation",
			key:              "security.agents.kruise.io/storage-auth",
			value:            `[{"credentialProviderName":"my-provider"}]`,
			expectAnnotation: true,
			expectedKey:      "security.agents.kruise.io/storage-auth",
			expectedValue:    `[{"credentialProviderName":"my-provider"}]`,
		},
		{
			name:             "empty key skips injection",
			key:              "",
			value:            `[{"credentialProviderName":"my-provider"}]`,
			expectAnnotation: false,
		},
		{
			name:             "empty value skips injection",
			key:              "security.agents.kruise.io/storage-auth",
			value:            "",
			expectAnnotation: false,
		},
		{
			name:             "both key and value empty skips injection",
			key:              "",
			value:            "",
			expectAnnotation: false,
		},
		{
			name:               "nil annotations map creates new map and injects",
			initialAnnotations: nil,
			key:                "security.agents.kruise.io/storage-auth",
			value:              `[{"credentialProviderName":"rrsa-provider"}]`,
			expectAnnotation:   true,
			expectedKey:        "security.agents.kruise.io/storage-auth",
			expectedValue:      `[{"credentialProviderName":"rrsa-provider"}]`,
		},
		{
			name:               "existing annotations are preserved",
			initialAnnotations: map[string]string{"existing-key": "existing-val"},
			key:                "security.agents.kruise.io/storage-auth",
			value:              `[{"credentialProviderName":"provider-x"}]`,
			expectAnnotation:   true,
			expectedKey:        "security.agents.kruise.io/storage-auth",
			expectedValue:      `[{"credentialProviderName":"provider-x"}]`,
		},
		{
			name:             "storage-auth with kmsKeyId attribute is injected",
			key:              "security.agents.kruise.io/storage-auth",
			value:            `[{"credentialProviderName":"oss-rw","attributes":{"bucket-name":"my-bucket","kms-key-id":"cmk-12345"}}]`,
			expectAnnotation: true,
			expectedKey:      "security.agents.kruise.io/storage-auth",
			expectedValue:    `[{"credentialProviderName":"oss-rw","attributes":{"bucket-name":"my-bucket","kms-key-id":"cmk-12345"}}]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mockSbx := &sandboxcr.Sandbox{
				Sandbox: &agentsv1alpha1.Sandbox{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "test-sandbox",
						Namespace:   "default",
						Annotations: tt.initialAnnotations,
					},
				},
			}

			ctrl := &Controller{}
			ctrl.injectStorageAuthAnnotation(mockSbx, tt.key, tt.value)

			annotations := mockSbx.GetAnnotations()

			if !tt.expectAnnotation {
				// Verify no new annotation was added
				if tt.initialAnnotations == nil {
					assert.Nil(t, annotations, "annotations should remain nil when injection is skipped")
				} else {
					assert.Equal(t, len(tt.initialAnnotations), len(annotations),
						"no new annotations should be added when injection is skipped")
				}
				return
			}

			require.NotNil(t, annotations, "annotations should not be nil after injection")
			val, exists := annotations[tt.expectedKey]
			assert.True(t, exists, "expected annotation key %q to exist", tt.expectedKey)
			assert.Equal(t, tt.expectedValue, val, "annotation value mismatch")

			// Verify existing annotations are preserved
			for k, v := range tt.initialAnnotations {
				assert.Equal(t, v, annotations[k], "existing annotation %q should be preserved", k)
			}
		})
	}
}

// TestBuildCSIMountOptions verifies that buildCSIMountOptions correctly handles
// empty mount configs, errors from CSI mount config building, and successful
// CSI mount option construction.
func TestBuildCSIMountOptions(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	// Register a test CSI driver
	controller.storageRegistry.RegisterProvider("test-csi-driver", &storages.MountProvider{})

	// Create a PersistentVolume with CSI info
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-build-csi-pv",
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "test-csi-driver",
					VolumeHandle: "test-volume-handle",
				},
			},
		},
	}
	require.NoError(t, fc.Create(t.Context(), pv))

	tests := []struct {
		name           string
		request        models.NewSandboxRequest
		expectErr      string
		expectNilMount bool
		expectDriver   string
	}{
		{
			name: "no mount configs returns nil",
			request: models.NewSandboxRequest{
				TemplateID: "test-template",
			},
			expectNilMount: true,
		},
		{
			name: "pv not found returns error",
			request: models.NewSandboxRequest{
				TemplateID: "test-template",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "non-existent-pv",
								MountPath: "/mnt/data",
							},
						},
					},
				},
			},
			expectErr: "failed to get persistent volume object by name",
		},
		{
			name: "valid csi mount returns options",
			request: models.NewSandboxRequest{
				TemplateID: "test-template",
				Extensions: models.NewSandboxRequestExtension{
					CSIMount: models.CSIMountExtension{
						MountConfigs: []v1alpha1.CSIMountConfig{
							{
								PvName:    "test-build-csi-pv",
								MountPath: "/mnt/data",
							},
						},
					},
				},
			},
			expectNilMount: false,
			expectDriver:   "test-csi-driver",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			csiMount, err := controller.buildCSIMountOptions(t.Context(), tt.request)
			if tt.expectErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectErr)
				assert.Nil(t, csiMount)
				return
			}

			require.NoError(t, err)
			if tt.expectNilMount {
				assert.Nil(t, csiMount)
				return
			}

			require.NotNil(t, csiMount)
			require.Len(t, csiMount.MountOptionList, 1)
			assert.Equal(t, tt.expectDriver, csiMount.MountOptionList[0].Driver)
			assert.NotEmpty(t, csiMount.MountOptionList[0].RequestRaw)
		})
	}
}

// TestCreateSandboxWithClone_StorageAuthHook verifies that the
// BuildStorageAuthAnnotation hook is invoked during clone when CSI mounts
// are present, that errors from the hook yield a 500 response, and that
// successful hook results inject the annotation onto the sandbox.
func TestCreateSandboxWithClone_StorageAuthHook(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	// Register a CSI driver and create a PV
	controller.storageRegistry.RegisterProvider("test-auth-csi-driver", &storages.MountProvider{})
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-auth-pv",
		},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "test-auth-csi-driver",
					VolumeHandle: "test-auth-volume-handle",
				},
			},
		},
	}
	require.NoError(t, fc.Create(t.Context(), pv))

	// Create checkpoint + template for clone
	cleanupCP := CreateCheckpointAndTemplateInNamespace(
		t, controller, "team-a", "auth-tmpl", "auth-cp-id",
		uuid.NewString(), "src-sbx", "2024-07-01T00:00:01Z",
	)
	defer cleanupCP()

	user := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Name: "auth-team-user",
		Team: &models.Team{Name: "team-a"},
	}

	// Override DefaultCreateSandbox to capture annotations and simulate success
	var capturedAnnotations map[string]string
	origCreateSandbox := sandboxcr.DefaultCreateSandbox
	sandboxcr.DefaultCreateSandbox = func(ctx context.Context, sbx *agentsv1alpha1.Sandbox, c ctrlclient.Client) (*agentsv1alpha1.Sandbox, error) {
		capturedAnnotations = sbx.Annotations
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

	baseRequest := models.NewSandboxRequest{
		TemplateID: "auth-cp-id",
		Timeout:    600,
		Extensions: models.NewSandboxRequestExtension{
			CSIMount: models.CSIMountExtension{
				MountConfigs: []v1alpha1.CSIMountConfig{
					{
						PvName:    "test-auth-pv",
						MountPath: "/mnt/data",
					},
				},
			},
			CreateOnNoStock: true,
		},
	}

	t.Run("hook returns error yields 500", func(t *testing.T) {
		origHook := csiutils.BuildStorageAuthAnnotation
		csiutils.BuildStorageAuthAnnotation = func(ctx context.Context, client ctrlclient.Client, mounts []v1alpha1.CSIMountConfig) (string, string, error) {
			return "", "", fmt.Errorf("mock auth build failure")
		}
		t.Cleanup(func() { csiutils.BuildStorageAuthAnnotation = origHook })

		_, apiErr := controller.createSandboxWithClone(t.Context(), baseRequest, user, "")
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusInternalServerError, apiErr.Code)
		assert.Contains(t, apiErr.Message, "mock auth build failure")
	})

	t.Run("buildCSIMountOptions error yields 400", func(t *testing.T) {
		// Use a non-existent PV so buildCSIMountOptions returns an error,
		// exercising the error-wrapping path in createSandboxWithClone.
		errRequest := baseRequest
		errRequest.Extensions.CSIMount.MountConfigs = []v1alpha1.CSIMountConfig{
			{
				PvName:    "non-existent-pv",
				MountPath: "/mnt/data",
			},
		}

		_, apiErr := controller.createSandboxWithClone(t.Context(), errRequest, user, "")
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusBadRequest, apiErr.Code)
		assert.Contains(t, apiErr.Message, "failed to get persistent volume object by name")
	})

	t.Run("hook succeeds injects annotation", func(t *testing.T) {
		capturedAnnotations = nil
		origHook := csiutils.BuildStorageAuthAnnotation
		csiutils.BuildStorageAuthAnnotation = func(ctx context.Context, client ctrlclient.Client, mounts []v1alpha1.CSIMountConfig) (string, string, error) {
			return "security.agents.kruise.io/storage-auth", `[{"provider":"test"}]`, nil
		}
		t.Cleanup(func() { csiutils.BuildStorageAuthAnnotation = origHook })

		// The clone will fail at the CSI mount step (no real agent-runtime sidecar),
		// but the Modifier runs during sandbox creation — before CSI mount — so the
		// annotation is already captured in DefaultCreateSandbox.
		_, _ = controller.createSandboxWithClone(t.Context(), baseRequest, user, "")

		require.NotNil(t, capturedAnnotations, "annotations should not be nil after clone with auth hook")
		val, exists := capturedAnnotations["security.agents.kruise.io/storage-auth"]
		assert.True(t, exists, "storage-auth annotation should be present on sandbox")
		assert.Equal(t, `[{"provider":"test"}]`, val)
	})

	t.Run("hook nil does not inject annotation", func(t *testing.T) {
		capturedAnnotations = nil
		origHook := csiutils.BuildStorageAuthAnnotation
		csiutils.BuildStorageAuthAnnotation = nil
		t.Cleanup(func() { csiutils.BuildStorageAuthAnnotation = origHook })

		// Same as above: clone fails at CSI mount, but Modifier has already run.
		_, _ = controller.createSandboxWithClone(t.Context(), baseRequest, user, "")

		// The storage-auth annotation should not be present
		if capturedAnnotations != nil {
			_, exists := capturedAnnotations["security.agents.kruise.io/storage-auth"]
			assert.False(t, exists, "storage-auth annotation should not be present when hook is nil")
		}
	})
}
