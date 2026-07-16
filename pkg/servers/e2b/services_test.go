/*
Copyright 2025.

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
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandbox-manager/quota"
	quotaspec "github.com/openkruise/agents/pkg/sandbox-manager/quota/spec"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	"github.com/openkruise/agents/pkg/utils/proxyutils"
	"github.com/openkruise/agents/pkg/utils/runtime"
	"github.com/openkruise/agents/pkg/utils/timeout"
	testutils "github.com/openkruise/agents/test/utils"
)

func imageChecker(image string, controller *Controller) func(t *testing.T, resp *models.Sandbox) {
	return func(t *testing.T, resp *models.Sandbox) {
		sbx, err := controller.manager.GetSandbox(t.Context(), keys.AdminKeyID.String(), []string{v1alpha1.SandboxStateRunning, v1alpha1.SandboxStatePaused}, infra.GetSandboxOptions{
			SandboxID: resp.SandboxID,
		})
		assert.NoError(t, err)
		assert.Equal(t, image, sbx.GetImage())
	}
}

func TestCreateSandbox(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	// Create test runtime server for InitRuntime
	opts := testutils.TestRuntimeServerOptions{
		RunCommandResult: runtime.RunCommandResult{
			PID:    1,
			Exited: true,
		},
		RunCommandImmediately: true,
	}
	server := testutils.NewTestRuntimeServer(opts)
	defer server.Close()

	templateName := "test-template"
	tests := []struct {
		name        string
		available   int
		userName    string
		request     models.NewSandboxRequest
		expectError *web.ApiError
		postCheck   func(t *testing.T, resp *models.Sandbox)
		setup       func(t *testing.T, controller *Controller, fc ctrlclient.Client)
	}{
		{
			name:      "success",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    600,
				Metadata: map[string]string{
					"test-metadata": "test-value",
				},
				EnvVars: models.EnvVars{
					"TEST_ENV": "test-value",
				},
			},
			postCheck: imageChecker("old-image", controller),
		},
		{
			name:      "success with default timeout",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					"test-key": "test-value",
				},
			},
		},
		{
			name:      "success with minimum timeout",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    30,
			},
		},
		{
			name:      "success with maximum timeout",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    7200,
			},
		},
		{
			name:      "fail with timeout too small",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    29,
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "timeout should between 30 and 2592000",
			},
		},
		{
			name:      "fail with timeout too large",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    2592001,
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "timeout should between 30 and 2592000",
			},
		},
		{
			name:      "fail with unqualified metadata key",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					"invalid@key": "test-value",
				},
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "Unqualified metadata key [invalid@key]: name part must consist of alphanumeric characters, '-', '_' or '.', and must start and end with an alphanumeric character (e.g. 'MyName',  or 'my.name',  or '123-abc', regex used for validation is '([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]')",
			},
		},
		{
			name:      "fail with forbidden metadata key",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					v1alpha1.E2BPrefix + "key": "test-value",
				},
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "Forbidden metadata key [e2b.agents.kruise.io/key]: cannot contain prefixes: [e2b.agents.kruise.io/ agents.kruise.io/",
			},
		},
		{
			name:      "fail without user",
			available: 2,
			userName:  "",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
			},
			expectError: &web.ApiError{
				Code:    401,
				Message: "User is empty",
			},
		},
		{
			name:      "fail with no available sandboxes",
			available: 0,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyCreateOnNoStock: v1alpha1.False,
				},
			},
			expectError: &web.ApiError{
				Code:    500,
				Message: "no available sandboxes for template test-template (no stock)",
			},
		},
		{
			name:      "claim with image",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithImage: "new-image",
				},
			},
			postCheck: imageChecker("new-image", controller),
		},
		{
			name:      "claim with bad image",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithImage: "bad-@@-image",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "Bad extension param: invalid image [bad-@@-image]: invalid reference format",
			},
		},
		{
			name:      "never timeout",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    300,
				Metadata: map[string]string{
					models.ExtensionKeyNeverTimeout: v1alpha1.True,
				},
			},
			postCheck: func(t *testing.T, resp *models.Sandbox) {
				assert.Equal(t, "0001-01-01T00:00:00Z", resp.EndAt)
				sbx, err := controller.manager.GetSandbox(t.Context(), keys.AdminKeyID.String(), []string{v1alpha1.SandboxStateRunning, v1alpha1.SandboxStatePaused}, infra.GetSandboxOptions{
					SandboxID: resp.SandboxID,
				})
				assert.NoError(t, err)
				assert.Equal(t, timeout.Options{}, sbx.GetTimeout())
			},
		},
		{
			name:      "claim with csi mount missing mount-point",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_VolumeName: "test-pv-name",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "must exist together",
			},
		},
		{
			name:      "claim with csi mount missing volume-name",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_MountPoint: "/mnt/data",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "must exist together",
			},
		},
		{
			name:      "claim with csi mount invalid mount point",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_VolumeName: "test-pv",
					models.ExtensionKeyClaimWithCSIMount_MountPoint: "../invalid/path",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "invalid containerMountPoint",
			},
		},
		{
			name:      "claim with csi mount pv not found",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_VolumeName: "non-existent-pv",
					models.ExtensionKeyClaimWithCSIMount_MountPoint: "/mnt/data",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "failed to get persistent volume object by name",
			},
		},
		{
			name:      "claim with csi mount success",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_VolumeName: "test-csi-pv",
					models.ExtensionKeyClaimWithCSIMount_MountPoint: "/mnt/data",
					models.ExtensionKeyClaimTimeout:                 "10", // CSI mount needs more time
				},
			},
			setup: func(t *testing.T, controller *Controller, fc ctrlclient.Client) {
				// Register a test CSI driver in the storage registry
				controller.storageRegistry.RegisterProvider("test-csi-driver", &storages.MountProvider{})

				// Create a PersistentVolume with CSI info
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-csi-pv",
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
				err := fc.Create(t.Context(), pv)
				require.NoError(t, err)
			},
		},
		{
			name:      "success with custom labels",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    600,
				Metadata: map[string]string{
					v1alpha1.E2BLabelPrefix + "app":         "my-app",
					v1alpha1.E2BLabelPrefix + "environment": "test",
					v1alpha1.E2BLabelPrefix + "team":        "backend",
					"regular-metadata-key":                  "should-remain-in-metadata",
				},
			},
			postCheck: func(t *testing.T, resp *models.Sandbox) {
				sbx := GetSandbox(t, resp.SandboxID, fc)
				assert.NotNil(t, sbx.Spec.Template)
				assert.NotNil(t, sbx.Spec.Template.Labels)

				assert.Equal(t, "my-app", sbx.Spec.Template.Labels["app"])
				assert.Equal(t, "test", sbx.Spec.Template.Labels["environment"])
				assert.Equal(t, "backend", sbx.Spec.Template.Labels["team"])

				assert.NotContains(t, sbx.Spec.Template.Labels, "regular-metadata-key")

				sandboxFromManager, err := controller.manager.GetSandbox(t.Context(), keys.AdminKeyID.String(), []string{v1alpha1.SandboxStateRunning, v1alpha1.SandboxStatePaused}, infra.GetSandboxOptions{
					SandboxID: resp.SandboxID,
				})
				assert.NoError(t, err)
				assert.NotNil(t, sandboxFromManager.GetPodLabels())
				assert.Equal(t, "my-app", sandboxFromManager.GetPodLabels()["app"])
				assert.Equal(t, "test", sandboxFromManager.GetPodLabels()["environment"])
				assert.Equal(t, "backend", sandboxFromManager.GetPodLabels()["team"])
			},
		},
		{
			name:      "fail with invalid label name",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					v1alpha1.E2BLabelPrefix + "invalid@label": "value",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "invalid label name",
			},
		},
		{
			name:      "fail with invalid label value",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					v1alpha1.E2BLabelPrefix + "valid-label": "invalid value with spaces!",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "invalid label value",
			},
		},
		{
			name:      "success with labels and metadata together",
			available: 2,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Timeout:    600,
				Metadata: map[string]string{
					v1alpha1.E2BLabelPrefix + "label-key": "label-value",
					"metadata-key":                        "metadata-value",
					"another-metadata":                    "another-value",
				},
			},
			postCheck: func(t *testing.T, resp *models.Sandbox) {
				sbx := GetSandbox(t, resp.SandboxID, fc)
				assert.NotNil(t, sbx.Spec.Template.Labels)
				assert.Equal(t, "label-value", sbx.Spec.Template.Labels["label-key"])

				assert.Equal(t, "metadata-value", resp.Metadata["metadata-key"])
				assert.Equal(t, "another-value", resp.Metadata["another-metadata"])

				assert.NotContains(t, sbx.Spec.Template.Labels, "metadata-key")
				assert.NotContains(t, sbx.Spec.Template.Labels, "another-metadata")
			},
		},
		{
			name:      "success with return pod ip metadata",
			available: 1,
			userName:  "test-user",
			request: models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeyReturnPodIP: v1alpha1.True,
				},
			},
			postCheck: func(t *testing.T, resp *models.Sandbox) {
				assert.Equal(t, "1.2.3.4", resp.Metadata[models.MetadataKeyPodIP])
				assert.NotContains(t, resp.Metadata, models.ExtensionKeyReturnPodIP)

				sbx := GetSandbox(t, resp.SandboxID, fc)
				assert.Equal(t, v1alpha1.True, sbx.Annotations[models.ExtensionKeyReturnPodIP])
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t, controller, fc)
			}
			var user *models.CreatedTeamAPIKey
			if tt.userName != "" {
				user = &models.CreatedTeamAPIKey{
					ID:   keys.AdminKeyID,
					Key:  InitKey,
					Name: tt.userName,
				}
			}
			cleanup := CreateSandboxPool(t, controller, templateName, tt.available, CreateSandboxPoolOptions{
				RuntimeURL:  server.URL,
				AccessToken: runtime.AccessToken,
			})
			require.Eventually(t, func() bool {
				list, err := controller.cache.ListSandboxesInPool(t.Context(), cache.ListSandboxesInPoolOptions{
					Pool: templateName,
				})
				return err == nil && len(list) == tt.available
			}, time.Second, 50*time.Millisecond)
			defer cleanup()
			now := time.Now()
			if tt.request.Metadata == nil {
				tt.request.Metadata = make(map[string]string)
			}
			// mock runtime server is not supported in e2b layer, the runtime is tested in infra package
			// only set default timeout if not already set
			if _, exists := tt.request.Metadata[models.ExtensionKeyClaimTimeout]; !exists {
				tt.request.Metadata[models.ExtensionKeyClaimTimeout] = "1" // let errors like "no stock" stop early
			}
			resp, apiError := controller.CreateSandbox(NewRequest(t, nil, tt.request, nil, user))
			if tt.expectError != nil {
				require.NotNil(t, apiError)
				if apiError != nil {
					assert.Equal(t, tt.expectError.Code, apiError.Code)
					assert.Contains(t, apiError.Message, tt.expectError.Message)
				}
			} else {
				require.Nil(t, apiError)
				sbx := resp.Body
				assert.True(t, strings.HasPrefix(sbx.SandboxID, fmt.Sprintf("%s--%s-", Namespace, templateName)))
				for k, v := range tt.request.Metadata {
					if !ValidateMetadataKey(k) {
						continue
					}
					if strings.HasPrefix(k, v1alpha1.E2BLabelPrefix) {
						continue
					}
					assert.Equal(t, v, sbx.Metadata[k], fmt.Sprintf("metadata key: %s", k))
				}
				startedAt, err := time.Parse(time.RFC3339, sbx.StartedAt)
				assert.NoError(t, err)
				assert.WithinDuration(t, now, startedAt, 5*time.Second)
				timeoutSeconds := 300
				if tt.request.Timeout != 0 {
					timeoutSeconds = tt.request.Timeout
				}
				if tt.request.Metadata[models.ExtensionKeyNeverTimeout] != v1alpha1.True {
					endAt, err := time.Parse(time.RFC3339, sbx.EndAt)
					assert.NoError(t, err)
					assert.WithinDuration(t, startedAt.Add(time.Duration(timeoutSeconds)*time.Second), endAt, 5*time.Second)
				}
				assert.Equal(t, models.SandboxStateRunning, sbx.State)
				if tt.postCheck != nil {
					tt.postCheck(t, sbx)
				}
			}
		})
	}
}

func TestCreateSandboxReturnsImmediatelyWhenCreateOnNoStockHitsQuota(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	templateName := "quota-denied-template"
	cleanup := CreateSandboxPool(t, controller, templateName, 0)
	defer cleanup()

	// This simulates the create-on-no-stock path reaching the apiserver and
	// being rejected by ResourceQuota. Forbidden create failures are classified
	// as terminal platform errors, so CreateSandbox must return immediately
	// instead of sleeping and retrying sandbox creation.
	quotaError := apierrors.NewForbidden(
		schema.GroupResource{Group: "agents.kruise.io", Resource: "sandboxes"},
		"quota-denied-sandbox",
		fmt.Errorf("exceeded quota: cpu-quota, requested: cpu=2, used: cpu=10, limited: cpu=10"),
	)
	origCreateSandbox := sandboxcr.DefaultCreateSandbox
	var createCalls atomic.Int32
	sandboxcr.DefaultCreateSandbox = func(context.Context, *v1alpha1.Sandbox, ctrlclient.Client) (*v1alpha1.Sandbox, error) {
		createCalls.Add(1)
		return nil, quotaError
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateSandbox = origCreateSandbox })

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}
	start := time.Now()
	resp, apiError := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		Metadata: map[string]string{
			models.ExtensionKeyCreateOnNoStock: v1alpha1.True,
			models.ExtensionKeyClaimTimeout:    "10",
		},
	}, nil, user))
	elapsed := time.Since(start)

	require.NotNil(t, apiError)
	assert.Nil(t, resp.Body)
	assert.Equal(t, http.StatusInternalServerError, apiError.Code)
	assert.Contains(t, apiError.Message, "platform configuration issue")
	assert.Equal(t, int32(1), createCalls.Load(), "terminal quota denial must not retry sandbox creation")
	assert.Less(t, elapsed, sandboxcr.CreateRetryInterval, "terminal quota denial must return before retry backoff")
}

func TestCreateSandbox_QuotaExceededReturns403WithoutRetry(t *testing.T) {
	fakeQuota := &fakeQuotaManager{acquireErr: quota.ErrQuotaExceeded}
	controller, _, teardown := SetupWithQuota(t, fakeQuota)
	defer teardown()

	cleanup := CreateSandboxPool(t, controller, "tmpl", 1)
	defer cleanup()

	limit := int64(0)
	user := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  uuid.NewString(),
		Name: "limited",
		Team: models.AdminTeam(),
		QuotaSpec: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
			Dimension: quotaspec.DimSandboxCount,
			Scope:     quotaspec.ScopeRunning,
			Limit:     limit,
		}}},
	}
	resp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "tmpl",
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, user))

	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Code)
	assert.Contains(t, apiErr.Message, "quota exceeded")
	assert.Zero(t, resp.Code)
	assert.Equal(t, int64(1), fakeQuota.acquireCalls.Load())
	assert.Equal(t, []quotaspec.QuotaScope{quotaspec.ScopeRunning}, fakeQuota.lastAcquire.Scopes)
}

func TestCreateSandbox_QuotaExceededLeavesPooledSandboxClaimable(t *testing.T) {
	fakeQuota := &fakeQuotaManager{acquireErr: quota.ErrQuotaExceeded}
	controller, _, teardown := SetupWithQuota(t, fakeQuota)
	defer teardown()

	cleanup := CreateSandboxPool(t, controller, "tmpl", 1)
	defer cleanup()

	limit := int64(0)
	limited := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  uuid.NewString(),
		Name: "limited",
		Team: models.AdminTeam(),
		QuotaSpec: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
			Dimension: quotaspec.DimSandboxCount,
			Scope:     quotaspec.ScopeRunning,
			Limit:     limit,
		}}},
	}
	resp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "tmpl",
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, limited))
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Code)
	assert.Zero(t, resp.Code)
	assert.Equal(t, int64(1), fakeQuota.acquireCalls.Load(), "quota miss must not retry")
	assert.Equal(t, []quotaspec.QuotaScope{quotaspec.ScopeRunning}, fakeQuota.lastAcquire.Scopes)

	unlimited := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  uuid.NewString(),
		Name: "unlimited",
		Team: models.AdminTeam(),
	}
	resp2, apiErr2 := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "tmpl",
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, unlimited))
	require.Nil(t, apiErr2)
	assert.Equal(t, http.StatusCreated, resp2.Code)
	require.NotNil(t, resp2.Body)
	assert.Equal(t, fmt.Sprintf("%s--tmpl-0", Namespace), resp2.Body.SandboxID)
	assert.Equal(t, int64(1), fakeQuota.acquireCalls.Load(), "unlimited key performs zero quota IO")

	claimed := GetSandbox(t, resp2.Body.SandboxID, getTestCRClient(controller))
	assert.Equal(t, unlimited.ID.String(), claimed.Annotations[v1alpha1.AnnotationOwner])
	assert.NotEmpty(t, claimed.Annotations[v1alpha1.AnnotationLock])
}

func TestCreateSandbox_CloneQuotaExceededReturns403WithoutRetry(t *testing.T) {
	fakeQuota := &fakeQuotaManager{acquireErr: quota.ErrQuotaExceeded}
	controller, _, teardown := SetupWithQuota(t, fakeQuota)
	defer teardown()

	team := &models.Team{ID: uuid.New(), Name: "team-a"}
	limit := int64(0)
	user := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  uuid.NewString(),
		Name: "limited",
		Team: team,
		QuotaSpec: &quotaspec.QuotaSpec{Limits: []quotaspec.QuotaLimit{{
			Dimension: quotaspec.DimSandboxCount,
			Scope:     quotaspec.ScopeRunning,
			Limit:     limit,
		}}},
	}
	cleanup := CreateCheckpointAndTemplateInNamespace(t, controller, team.Name, "clone-template", "checkpoint-1", user.ID.String(), "source-sandbox", "2026-06-19T00:00:00Z")
	defer cleanup()

	resp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "checkpoint-1",
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, user))

	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusForbidden, apiErr.Code)
	assert.Contains(t, apiErr.Message, "quota exceeded")
	assert.Zero(t, resp.Code)
	assert.Equal(t, int64(1), fakeQuota.acquireCalls.Load())
	assert.Equal(t, []quotaspec.QuotaScope{quotaspec.ScopeRunning}, fakeQuota.lastAcquire.Scopes)
}

func TestCreateSandbox_UnlimitedKeyDoesNotCallQuota(t *testing.T) {
	fakeQuota := &fakeQuotaManager{}
	controller, _, teardown := SetupWithQuota(t, fakeQuota)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  uuid.NewString(),
		Name: "unlimited",
		Team: models.AdminTeam(),
	}

	// Unlimited user (no QuotaSpec) must produce nil admission in the manager
	// and generate zero quota acquire calls.
	assert.Nil(t, user.QuotaSpec, "user must not have a QuotaSpec")
	assert.NotNil(t, controller.manager.GetInfra(), "manager infra must be wired")
	assert.Equal(t, int64(0), fakeQuota.acquireCalls.Load())
}

func TestCreateSandboxAlwaysCreatesAccessToken(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	server := testutils.NewTestRuntimeServer(testutils.TestRuntimeServerOptions{
		RunCommandResult: runtime.RunCommandResult{
			PID:    1,
			Exited: true,
		},
		RunCommandImmediately: true,
	})
	defer server.Close()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "secure omitted",
			body: map[string]any{},
		},
		{
			name: "secure false ignored",
			body: map[string]any{
				"secure": false,
			},
		},
		{
			name: "secure true ignored",
			body: map[string]any{
				"secure": true,
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templateName := fmt.Sprintf("access-token-template-%d", i)
			cleanup := CreateSandboxPool(t, controller, templateName, 1, CreateSandboxPoolOptions{
				RuntimeURL: server.URL,
			})
			defer cleanup()

			body := map[string]any{
				"templateID": templateName,
				"metadata": map[string]string{
					models.ExtensionKeyClaimTimeout: "1",
				},
			}
			for k, v := range tt.body {
				body[k] = v
			}

			resp, apiError := controller.CreateSandbox(NewRequest(t, nil, body, nil, user))
			require.Nil(t, apiError)
			require.NotNil(t, resp.Body)
			assert.NotEmpty(t, resp.Body.EnvdAccessToken)

			sbx := GetSandbox(t, resp.Body.SandboxID, fc)
			assert.Equal(t, resp.Body.EnvdAccessToken, sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken])
		})
	}
}

func TestCreateSandboxSkipsAccessTokenWhenInitRuntimeIsSkipped(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	tests := []struct {
		name string
		body map[string]any
	}{
		{
			name: "skip init runtime extension",
			body: map[string]any{
				"metadata": map[string]string{
					models.ExtensionKeyClaimTimeout:    "1",
					models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				},
			},
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templateName := fmt.Sprintf("skip-init-access-token-template-%d", i)
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			body := map[string]any{
				"templateID": templateName,
			}
			for k, v := range tt.body {
				body[k] = v
			}

			resp, apiError := controller.CreateSandbox(NewRequest(t, nil, body, nil, user))
			require.Nil(t, apiError)
			require.NotNil(t, resp.Body)
			assert.Empty(t, resp.Body.EnvdAccessToken)

			sbx := GetSandbox(t, resp.Body.SandboxID, fc)
			assert.NotContains(t, sbx.Annotations, v1alpha1.AnnotationRuntimeAccessToken)
		})
	}
}

func TestCreateSandboxRetentionSemantics(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	tests := []struct {
		name             string
		metadata         map[string]string
		autoPause        bool
		neverTimeout     bool
		wantAnnotation   string
		wantShutdownFrom func(pauseTime time.Time) time.Time
		expectCreateErr  string
	}{
		{
			name: "default retention persisted when metadata absent",
			metadata: map[string]string{
				models.ExtensionKeySkipInitRuntime: v1alpha1.True,
			},
			autoPause:      true,
			wantAnnotation: timeout.ReservePausedSandboxDurationForeverValue,
			wantShutdownFrom: func(pauseTime time.Time) time.Time {
				return pauseTime.Add(timeout.ForeverReservePausedSandboxDuration)
			},
		},
		{
			name: "custom retention persisted",
			metadata: map[string]string{
				models.ExtensionKeySkipInitRuntime:              v1alpha1.True,
				models.ExtensionKeyReservePausedSandboxDuration: "240h",
			},
			autoPause:      true,
			wantAnnotation: "240h",
			wantShutdownFrom: func(pauseTime time.Time) time.Time {
				return pauseTime.Add(240 * time.Hour)
			},
		},
		{
			name: "never timeout persists annotation but leaves deadlines nil",
			metadata: map[string]string{
				models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				models.ExtensionKeyNeverTimeout:    v1alpha1.True,
			},
			autoPause:      true,
			neverTimeout:   true,
			wantAnnotation: timeout.ReservePausedSandboxDurationForeverValue,
		},
		{
			name: "invalid retention metadata returns bad request",
			metadata: map[string]string{
				models.ExtensionKeyReservePausedSandboxDuration: "0s",
			},
			expectCreateErr: "Bad extension param",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			templateName := fmt.Sprintf("retention-semantics-%d", i)
			if tt.expectCreateErr == "" {
				cleanup := CreateSandboxPool(t, controller, templateName, 1)
				defer cleanup()
			}

			createResp, apiError := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				AutoPause:  tt.autoPause,
				Timeout:    300,
				Metadata:   maps.Clone(tt.metadata),
			}, nil, user))

			if tt.expectCreateErr != "" {
				require.NotNil(t, apiError)
				assert.Contains(t, apiError.Message, tt.expectCreateErr)
				return
			}

			require.Nil(t, apiError)
			require.NotNil(t, createResp.Body)

			sbx := GetSandbox(t, createResp.Body.SandboxID, client)
			assert.Equal(t, tt.wantAnnotation, sbx.Annotations[v1alpha1.AnnotationReservePausedSandboxDuration])
			assert.NotContains(t, createResp.Body.Metadata, v1alpha1.AnnotationReservePausedSandboxDuration)
			assert.NotContains(t, createResp.Body.Metadata, models.ExtensionKeyReservePausedSandboxDuration)
			if tt.neverTimeout {
				assert.Nil(t, sbx.Spec.PauseTime)
				assert.Nil(t, sbx.Spec.ShutdownTime)
			} else if tt.autoPause {
				require.NotNil(t, sbx.Spec.PauseTime)
				require.NotNil(t, sbx.Spec.ShutdownTime)
				assert.WithinDuration(t, tt.wantShutdownFrom(sbx.Spec.PauseTime.Time), sbx.Spec.ShutdownTime.Time, 5*time.Second)
			}
		})
	}
}

func TestPausedSandboxRetentionLifecycle(t *testing.T) {
	tests := []struct {
		name                  string
		templateName          string
		createRetention       string
		createRetentionPeriod time.Duration
		manualRetention       string
		manualRetentionPeriod time.Duration
	}{
		{
			name:                  "custom create retention survives connect and manual pause can override",
			templateName:          "paused-retention-lifecycle",
			createRetention:       "1h",
			createRetentionPeriod: time.Hour,
			manualRetention:       "30m",
			manualRetentionPeriod: 30 * time.Minute,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, client, teardown := Setup(t)
			defer teardown()
			user := &models.CreatedTeamAPIKey{ID: keys.AdminKeyID, Key: InitKey, Name: "admin"}
			cleanup := CreateSandboxPool(t, controller, tt.templateName, 1)
			defer cleanup()

			createResp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: tt.templateName,
				AutoPause:  true,
				Timeout:    300,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime:              v1alpha1.True,
					models.ExtensionKeyReservePausedSandboxDuration: tt.createRetention,
				},
			}, nil, user))
			require.Nil(t, apiErr)
			sbx := GetSandbox(t, createResp.Body.SandboxID, client)
			require.NotNil(t, sbx.Spec.PauseTime)
			require.NotNil(t, sbx.Spec.ShutdownTime)
			assert.WithinDuration(t, sbx.Spec.PauseTime.Time.Add(tt.createRetentionPeriod), sbx.Spec.ShutdownTime.Time, 5*time.Second)

			connectResp, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: 600,
			}, map[string]string{"sandboxID": createResp.Body.SandboxID}, user))
			require.Nil(t, apiErr)
			require.Equal(t, http.StatusOK, connectResp.Code)
			sbx = GetSandbox(t, createResp.Body.SandboxID, client)
			require.NotNil(t, sbx.Spec.PauseTime)
			require.NotNil(t, sbx.Spec.ShutdownTime)
			assert.WithinDuration(t, sbx.Spec.PauseTime.Time.Add(tt.createRetentionPeriod), sbx.Spec.ShutdownTime.Time, 5*time.Second)

			beforeAutoPause := time.Now()
			sbx.Spec.PauseTime = &metav1.Time{Time: beforeAutoPause.Add(-time.Minute)}
			shutdownAfterAutoPause := metav1.NewTime(beforeAutoPause.Add(tt.createRetentionPeriod))
			sbx.Spec.ShutdownTime = &shutdownAfterAutoPause
			sbx.Spec.Paused = true
			require.NoError(t, client.Update(t.Context(), sbx))
			DoSetSandboxStatus(v1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse)(t, client, sbx.DeepCopy())

			sbx = GetSandbox(t, createResp.Body.SandboxID, client)
			require.True(t, sbx.Spec.Paused)
			assert.Equal(t, v1alpha1.SandboxPaused, sbx.Status.Phase)
			assert.WithinDuration(t, beforeAutoPause.Add(tt.createRetentionPeriod), sbx.Spec.ShutdownTime.Time, 5*time.Second)

			EnableWaitSim(t, controller, createResp.Body.SandboxID)
			go UpdateSandboxWhen(t, client, createResp.Body.SandboxID, func(s *v1alpha1.Sandbox) bool {
				return !s.Spec.Paused
			}, DoSetSandboxStatus(v1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
			_, apiErr = controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: 900,
			}, map[string]string{"sandboxID": createResp.Body.SandboxID}, user))
			require.Nil(t, apiErr)
			sbx = GetSandbox(t, createResp.Body.SandboxID, client)
			assert.WithinDuration(t, sbx.Spec.PauseTime.Time.Add(tt.createRetentionPeriod), sbx.Spec.ShutdownTime.Time, 5*time.Second)

			req := NewRequest(t, nil, nil, map[string]string{"sandboxID": createResp.Body.SandboxID}, user)
			req.Header.Set(models.ExtensionHeaderReservePausedSandboxDuration, tt.manualRetention)
			go UpdateSandboxWhen(t, client, createResp.Body.SandboxID, func(s *v1alpha1.Sandbox) bool {
				return s.Spec.Paused
			}, DoSetSandboxStatus(v1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
			beforeManualPause := time.Now()
			_, apiErr = controller.PauseSandbox(req)
			require.Nil(t, apiErr)
			sbx = GetSandbox(t, createResp.Body.SandboxID, client)
			assert.Equal(t, tt.manualRetention, sbx.Annotations[v1alpha1.AnnotationReservePausedSandboxDuration])
			assert.WithinDuration(t, beforeManualPause.Add(tt.manualRetentionPeriod), sbx.Spec.ShutdownTime.Time, 5*time.Second)
			assert.WithinDuration(t, beforeManualPause.Add(tt.manualRetentionPeriod), sbx.Spec.PauseTime.Time, 5*time.Second)
		})
	}
}

// CreateCheckpointAndTemplate creates a Checkpoint with associated SandboxTemplate for clone tests
func CreateCheckpointAndTemplate(t *testing.T, controller *Controller, checkpointID string) func() {
	tmpl := v1alpha1.EmbeddedSandboxTemplate{
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "checkpoint-image",
					},
				},
			},
		},
	}

	// Create SandboxTemplate
	sbt := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointID,
			Namespace: Namespace,
			UID:       types.UID(uuid.NewString()),
		},
		Spec: v1alpha1.SandboxTemplateSpec{
			Template: tmpl.Template,
		},
	}
	// Use the controller-runtime client (CacheV2's fake client) for all CRD operations
	fc := getTestCRClient(controller)
	err := fc.Create(t.Context(), sbt)
	require.NoError(t, err)
	// Wait for SandboxTemplate to be cached
	require.Eventually(t, func() bool {
		got := &v1alpha1.SandboxTemplate{}
		return fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: Namespace, Name: checkpointID}, got) == nil
	}, time.Second, 10*time.Millisecond)

	// Create Checkpoint with template label
	cp := &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointID,
			Namespace: Namespace,
			Labels: map[string]string{
				v1alpha1.LabelSandboxTemplate: checkpointID,
			},
		},
		Status: v1alpha1.CheckpointStatus{
			CheckpointId: checkpointID,
		},
	}
	err = fc.Create(t.Context(), cp)
	require.NoError(t, err)
	// UpdateStatus is required because Kubernetes API ignores Status field during Create
	err = fc.Status().Update(t.Context(), cp)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return controller.manager.GetInfra().HasCheckpoint(t.Context(), infra.HasCheckpointOptions{
			CheckpointID: checkpointID,
		})
	}, time.Second, 10*time.Millisecond)

	return func() {
		_ = fc.Delete(t.Context(), sbt)
		_ = fc.Delete(t.Context(), cp)
	}
}

// CreateCheckpointAndTemplateWithAnnotations creates a Checkpoint with associated SandboxTemplate.
// The annotations are applied to the Checkpoint (not the SandboxTemplate),
// since necessary annotations (e.g., CSI mount config) are propagated via checkpoint
// and restored to sandbox during clone via restoreAnnotationsFromCheckpoint.
func CreateCheckpointAndTemplateWithAnnotations(t *testing.T, controller *Controller, checkpointID string, annotations map[string]string) func() {
	tmpl := v1alpha1.EmbeddedSandboxTemplate{
		Template: &corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{
				Containers: []corev1.Container{
					{
						Name:  "main",
						Image: "checkpoint-image",
					},
				},
			},
		},
	}

	// Create SandboxTemplate without custom annotations
	sbt := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointID,
			Namespace: Namespace,
			UID:       types.UID(uuid.NewString()),
		},
		Spec: v1alpha1.SandboxTemplateSpec{
			Template: tmpl.Template,
		},
	}
	// Use the controller-runtime client (CacheV2's fake client) for all CRD operations
	fc := getTestCRClient(controller)
	err := fc.Create(t.Context(), sbt)
	require.NoError(t, err)
	// Wait for SandboxTemplate to be cached
	require.Eventually(t, func() bool {
		got := &v1alpha1.SandboxTemplate{}
		return fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: Namespace, Name: checkpointID}, got) == nil
	}, time.Second, 10*time.Millisecond)

	// Create Checkpoint with template label and custom annotations
	cp := &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:        checkpointID,
			Namespace:   Namespace,
			Annotations: annotations,
			Labels: map[string]string{
				v1alpha1.LabelSandboxTemplate: checkpointID,
			},
		},
		Status: v1alpha1.CheckpointStatus{
			CheckpointId: checkpointID,
		},
	}
	err = fc.Create(t.Context(), cp)
	require.NoError(t, err)
	// UpdateStatus is required because Kubernetes API ignores Status field during Create
	err = fc.Status().Update(t.Context(), cp)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return controller.manager.GetInfra().HasCheckpoint(t.Context(), infra.HasCheckpointOptions{
			CheckpointID: checkpointID,
		})
	}, time.Second, 10*time.Millisecond)

	return func() {
		_ = fc.Delete(t.Context(), sbt)
		_ = fc.Delete(t.Context(), cp)
	}
}

func TestCloneSandbox(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	// Create test runtime server for InitRuntime
	runtimeOpts := testutils.TestRuntimeServerOptions{
		RunCommandResult: runtime.RunCommandResult{
			PID:    1,
			Exited: true,
		},
		RunCommandImmediately: true,
	}
	server := testutils.NewTestRuntimeServer(runtimeOpts)
	defer server.Close()

	// Decorator: DefaultCreateSandbox - set sandbox ready after creation
	origCreateSandbox := sandboxcr.DefaultCreateSandbox
	t.Cleanup(func() { sandboxcr.DefaultCreateSandbox = origCreateSandbox })
	sandboxcr.DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, c ctrlclient.Client) (*v1alpha1.Sandbox, error) {
		// Set Name (FakeClient does not handle GenerateName)
		if sbx.Name == "" && sbx.GenerateName != "" {
			sbx.Name = sbx.GenerateName + rand.String(5)
		}
		// Set RuntimeURL annotation and AccessToken
		if sbx.Annotations == nil {
			sbx.Annotations = map[string]string{}
		}
		sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
		sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = runtime.AccessToken

		// Call original createSandbox
		created, err := origCreateSandbox(ctx, sbx, c)
		if err != nil {
			return nil, err
		}

		// Set Ready status
		created.Status = v1alpha1.SandboxStatus{
			Phase:              v1alpha1.SandboxRunning,
			ObservedGeneration: created.Generation,
			Conditions: []metav1.Condition{
				{
					Type:               string(v1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Ready",
				},
			},
			PodInfo: v1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		}
		if err = c.Status().Update(ctx, created); err != nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
		return created, nil
	}

	checkpointID := "test-checkpoint-001"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	tests := []struct {
		name        string
		request     models.NewSandboxRequest
		expectError *web.ApiError
		postCheck   func(t *testing.T, resp *models.Sandbox, controller *Controller)
		setup       func(t *testing.T, controller *Controller, fc ctrlclient.Client)
	}{
		{
			name: "clone success",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Timeout:    600,
				Metadata: map[string]string{
					"test-metadata": "test-value",
				},
				EnvVars: models.EnvVars{
					"TEST_ENV": "test-value",
				},
			},
		},
		{
			name: "clone success with default timeout",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Metadata: map[string]string{
					"test-key": "test-value",
				},
			},
		},
		{
			name: "clone success with custom timeout",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Timeout:    1200,
			},
		},
		{
			name: "clone fail with timeout too small",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Timeout:    29,
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "timeout should between 30 and 2592000",
			},
		},
		{
			name: "clone fail with checkpoint not found",
			request: models.NewSandboxRequest{
				TemplateID: "non-existent-checkpoint",
				Timeout:    300,
			},
			expectError: &web.ApiError{
				Code:    400,
				Message: "Template or Checkpoint not found",
			},
		},
		{
			name: "clone success with never timeout",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Timeout:    300,
				Metadata: map[string]string{
					models.ExtensionKeyNeverTimeout: v1alpha1.True,
				},
			},
			postCheck: func(t *testing.T, resp *models.Sandbox, controller *Controller) {
				assert.Equal(t, "0001-01-01T00:00:00Z", resp.EndAt)
				sbx, err := controller.manager.GetSandbox(t.Context(), keys.AdminKeyID.String(), []string{v1alpha1.SandboxStateRunning, v1alpha1.SandboxStatePaused}, infra.GetSandboxOptions{
					SandboxID: resp.SandboxID,
				})
				assert.NoError(t, err)
				assert.Equal(t, timeout.Options{}, sbx.GetTimeout())
			},
		},
		{
			name: "clone success with auto pause",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Timeout:    300,
				AutoPause:  true,
			},
			postCheck: func(t *testing.T, resp *models.Sandbox, controller *Controller) {
				sbx, err := controller.manager.GetSandbox(t.Context(), keys.AdminKeyID.String(), []string{v1alpha1.SandboxStateRunning, v1alpha1.SandboxStatePaused}, infra.GetSandboxOptions{
					SandboxID: resp.SandboxID,
				})
				assert.NoError(t, err)
				// When autoPause is true, both ShutdownTime and PauseTime should be set
				assert.NotNil(t, sbx.GetTimeout().ShutdownTime)
				assert.NotNil(t, sbx.GetTimeout().PauseTime)
			},
		},
		{
			name: "clone with csi mount missing mount-point",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_VolumeName: "test-pv-name",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "must exist together",
			},
		},
		{
			name: "clone with csi mount missing volume-name",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_MountPoint: "/mnt/data",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "must exist together",
			},
		},
		{
			name: "clone with csi mount invalid mount point",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_VolumeName: "test-pv",
					models.ExtensionKeyClaimWithCSIMount_MountPoint: "../invalid/path",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "invalid containerMountPoint",
			},
		},
		{
			name: "clone with csi mount pv not found",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_VolumeName: "non-existent-pv",
					models.ExtensionKeyClaimWithCSIMount_MountPoint: "/mnt/data",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "failed to get persistent volume object by name",
			},
		},
		{
			name: "clone with csi mount success",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithCSIMount_VolumeName: "test-clone-csi-pv",
					models.ExtensionKeyClaimWithCSIMount_MountPoint: "/mnt/data",
				},
			},
			setup: func(t *testing.T, controller *Controller, fc ctrlclient.Client) {
				// Register a test CSI driver in the storage registry
				controller.storageRegistry.RegisterProvider("test-clone-csi-driver", &storages.MountProvider{})

				// Create a PersistentVolume with CSI info
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "test-clone-csi-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-clone-csi-driver",
								VolumeHandle: "test-clone-volume-handle",
							},
						},
					},
				}
				err := fc.Create(t.Context(), pv)
				require.NoError(t, err)
			},
		},
		{
			name: "clone fail with inplace update not supported",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Timeout:    300,
				Metadata: map[string]string{
					models.ExtensionKeyClaimWithImage: "new-image:latest",
				},
			},
			expectError: &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: "InplaceUpdate is not supported for clone",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t, controller, fc)
			}
			cleanup := CreateCheckpointAndTemplate(t, controller, checkpointID)
			defer cleanup()

			now := time.Now()
			if tt.request.Metadata == nil {
				tt.request.Metadata = make(map[string]string)
			}

			resp, apiError := controller.CreateSandbox(NewRequest(t, nil, tt.request, nil, user))
			if tt.expectError != nil {
				require.NotNil(t, apiError)
				if apiError != nil {
					assert.Equal(t, tt.expectError.Code, apiError.Code)
					assert.Contains(t, apiError.Message, tt.expectError.Message)
				}
			} else {
				require.Nil(t, apiError)
				defer func() {
					_, deleteErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
						"sandboxID": resp.Body.SandboxID,
					}, user))
					require.Nil(t, deleteErr)
				}()
				sbx := resp.Body
				// Verify sandbox ID format (cloned sandbox has different naming pattern)
				assert.NotEmpty(t, sbx.SandboxID)
				assert.True(t, strings.HasPrefix(sbx.SandboxID, Namespace+"--"))

				// Verify metadata is preserved
				for k, v := range tt.request.Metadata {
					if !ValidateMetadataKey(k) {
						continue
					}
					assert.Equal(t, v, sbx.Metadata[k], fmt.Sprintf("metadata key: %s", k))
				}

				// Verify timestamps
				startedAt, err := time.Parse(time.RFC3339, sbx.StartedAt)
				assert.NoError(t, err)
				assert.WithinDuration(t, now, startedAt, 5*time.Second)

				// Verify timeout/endAt
				timeoutSeconds := 300
				if tt.request.Timeout != 0 {
					timeoutSeconds = tt.request.Timeout
				}
				if tt.request.Metadata[models.ExtensionKeyNeverTimeout] != v1alpha1.True {
					endAt, err := time.Parse(time.RFC3339, sbx.EndAt)
					assert.NoError(t, err)
					assert.WithinDuration(t, startedAt.Add(time.Duration(timeoutSeconds)*time.Second), endAt, 5*time.Second)
				}

				// Verify state
				assert.Equal(t, models.SandboxStateRunning, sbx.State)

				// Run post check if defined
				if tt.postCheck != nil {
					tt.postCheck(t, sbx, controller)
				}
			}
		})
	}
}

func TestCloneSandboxWithCSIMountFromCheckpointAnnotation(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	// Create test runtime server for InitRuntime
	runtimeOpts := testutils.TestRuntimeServerOptions{
		RunCommandResult: runtime.RunCommandResult{
			PID:    1,
			Exited: true,
		},
		RunCommandImmediately: true,
	}
	server := testutils.NewTestRuntimeServer(runtimeOpts)
	defer server.Close()

	// Decorator: DefaultCreateSandbox - set sandbox ready after creation
	origCreateSandbox := sandboxcr.DefaultCreateSandbox
	t.Cleanup(func() { sandboxcr.DefaultCreateSandbox = origCreateSandbox })
	sandboxcr.DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, client ctrlclient.Client) (*v1alpha1.Sandbox, error) {
		if sbx.Name == "" && sbx.GenerateName != "" {
			sbx.Name = sbx.GenerateName + rand.String(5)
		}
		if sbx.Annotations == nil {
			sbx.Annotations = map[string]string{}
		}
		sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
		sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = runtime.AccessToken

		created, err := origCreateSandbox(ctx, sbx, client)
		if err != nil {
			return nil, err
		}
		created.Status = v1alpha1.SandboxStatus{
			Phase:              v1alpha1.SandboxRunning,
			ObservedGeneration: created.Generation,
			Conditions: []metav1.Condition{
				{
					Type:               string(v1alpha1.SandboxConditionReady),
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Now(),
					Reason:             "Ready",
				},
			},
			PodInfo: v1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		}
		if err = client.Status().Update(ctx, created); err != nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
		return created, nil
	}

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	tests := []struct {
		name                  string
		checkpointID          string
		checkpointAnnotations map[string]string
		request               models.NewSandboxRequest
		expectError           *web.ApiError
		setup                 func(t *testing.T, controller *Controller, c ctrlclient.Client)
		postCheck             func(t *testing.T, resp *models.Sandbox, controller *Controller)
	}{
		{
			name:         "clone fail with csi mount config from checkpoint annotation - driver not supported in sandbox",
			checkpointID: "cp-with-csi-mount",
			checkpointAnnotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"pv-nas-001","mountPath":"/data","subPath":"subdir","readOnly":true}]`,
			},
			request: models.NewSandboxRequest{
				Timeout: 300,
			},
			setup: func(t *testing.T, controller *Controller, c ctrlclient.Client) {
				controller.storageRegistry.RegisterProvider("test-csi-driver-from-cp", &storages.MountProvider{})
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-nas-001",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:       "test-csi-driver-from-cp",
								VolumeHandle: "test-volume-handle-001",
							},
						},
					},
				}
				require.NoError(t, c.Create(t.Context(), pv))
			},
			expectError: &web.ApiError{
				Message: "not supported in current environment",
			},
		},
		{
			name:         "clone fail with multiple csi mount configs from checkpoint annotation - driver not supported in sandbox",
			checkpointID: "cp-with-multi-csi",
			checkpointAnnotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"pv-multi-1","mountPath":"/data1"},{"pvName":"pv-multi-2","mountPath":"/data2","readOnly":true}]`,
			},
			request: models.NewSandboxRequest{
				Timeout: 300,
			},
			setup: func(t *testing.T, controller *Controller, c ctrlclient.Client) {
				controller.storageRegistry.RegisterProvider("test-multi-driver", &storages.MountProvider{})
				for _, pvName := range []string{"pv-multi-1", "pv-multi-2"} {
					pv := &corev1.PersistentVolume{
						ObjectMeta: metav1.ObjectMeta{
							Name: pvName,
						},
						Spec: corev1.PersistentVolumeSpec{
							PersistentVolumeSource: corev1.PersistentVolumeSource{
								CSI: &corev1.CSIPersistentVolumeSource{
									Driver:       "test-multi-driver",
									VolumeHandle: pvName + "-handle",
								},
							},
						},
					}
					require.NoError(t, c.Create(t.Context(), pv))
				}
			},
			expectError: &web.ApiError{
				Message: "not supported in current environment",
			},
		},
		{
			name:         "clone fail with invalid csi mount json in checkpoint annotation",
			checkpointID: "cp-invalid-csi-json",
			checkpointAnnotations: map[string]string{
				models.ExtensionKeyClaimWithCSIMount_MountConfig: "not-valid-json",
			},
			request: models.NewSandboxRequest{
				Timeout: 300,
			},
			expectError: &web.ApiError{
				Message: "failed to parse csi mount config from annotation",
			},
		},
		{
			name:         "clone success with no csi mount annotation in checkpoint",
			checkpointID: "cp-no-csi-annotation",
			checkpointAnnotations: map[string]string{
				"some-other-annotation": "value",
			},
			request: models.NewSandboxRequest{
				Timeout: 300,
			},
		},
		{
			name:                  "clone success with empty checkpoint annotations",
			checkpointID:          "cp-empty-annotations",
			checkpointAnnotations: nil,
			request: models.NewSandboxRequest{
				Timeout: 300,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t, controller, fc)
			}
			tt.request.TemplateID = tt.checkpointID
			cleanup := CreateCheckpointAndTemplateWithAnnotations(t, controller, tt.checkpointID, tt.checkpointAnnotations)
			defer cleanup()

			if tt.request.Metadata == nil {
				tt.request.Metadata = make(map[string]string)
			}

			resp, apiError := controller.CreateSandbox(NewRequest(t, nil, tt.request, nil, user))
			if tt.expectError != nil {
				require.NotNil(t, apiError)
				if apiError != nil {
					assert.Contains(t, apiError.Message, tt.expectError.Message)
				}
			} else {
				require.Nil(t, apiError)
				defer func() {
					_, deleteErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
						"sandboxID": resp.Body.SandboxID,
					}, user))
					require.Nil(t, deleteErr)
				}()
				sbx := resp.Body
				assert.NotEmpty(t, sbx.SandboxID)
				assert.True(t, strings.HasPrefix(sbx.SandboxID, Namespace+"--"))
				assert.Equal(t, models.SandboxStateRunning, sbx.State)

				if tt.postCheck != nil {
					tt.postCheck(t, sbx, controller)
				}
			}
		})
	}
}

func TestAutoPause(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	timeoutSeconds := 300
	now := time.Now()
	timeoutTime := now.Add(time.Duration(timeoutSeconds) * time.Second)
	timeoutAfterPaused := now.Add(timeout.ForeverReservePausedSandboxDuration)
	templateName := "auto-pause"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}
	tests := []struct {
		name          string
		autoPause     bool
		createChecker func(t *testing.T, sbx *v1alpha1.Sandbox)
		pauseChecker  func(t *testing.T, sbx *v1alpha1.Sandbox)
		resumeChecker func(t *testing.T, sbx *v1alpha1.Sandbox)
	}{
		{
			name:      "autoPause == false",
			autoPause: false,
			createChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.Nil(t, sbx.Spec.PauseTime)
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, timeoutTime, 5*time.Second)
				}
			},
			pauseChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.Nil(t, sbx.Spec.PauseTime)
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, timeoutAfterPaused, 5*time.Second)
				}
			},
			resumeChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.Nil(t, sbx.Spec.PauseTime)
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, timeoutTime, 5*time.Second)
				}
			},
		},
		{
			name:      "autoPause == true",
			autoPause: true,
			createChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.NotNil(t, sbx.Spec.PauseTime)
				if sbx.Spec.PauseTime != nil {
					assert.WithinDuration(t, sbx.Spec.PauseTime.Time, timeoutTime, 5*time.Second)
				}
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.PauseTime != nil && sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.PauseTime.Time.Add(timeout.ForeverReservePausedSandboxDuration), sbx.Spec.ShutdownTime.Time, 5*time.Second)
				}
			},
			pauseChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.NotNil(t, sbx.Spec.PauseTime)
				if sbx.Spec.PauseTime != nil {
					assert.WithinDuration(t, sbx.Spec.PauseTime.Time, timeoutAfterPaused, 5*time.Second)
				}
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, timeoutAfterPaused, 5*time.Second)
				}
			},
			resumeChecker: func(t *testing.T, sbx *v1alpha1.Sandbox) {
				assert.NotNil(t, sbx.Spec.PauseTime)
				if sbx.Spec.PauseTime != nil {
					assert.WithinDuration(t, sbx.Spec.PauseTime.Time, timeoutTime, 5*time.Second)
				}
				assert.NotNil(t, sbx.Spec.ShutdownTime)
				if sbx.Spec.PauseTime != nil && sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.PauseTime.Time.Add(timeout.ForeverReservePausedSandboxDuration), sbx.Spec.ShutdownTime.Time, 5*time.Second)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cleanup := CreateSandboxPool(t, controller, templateName, 1)
			defer cleanup()

			createResp, apiError := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				AutoPause:  tt.autoPause,
				Timeout:    timeoutSeconds,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				},
			}, nil, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, timeoutTime, createResp.Body.EndAt)
			tt.createChecker(t, GetSandbox(t, createResp.Body.SandboxID, client))

			// Register sandbox key for wait simulation
			mockMgr := controller.cache.(*cache.Cache).GetMockManager()
			sbx := GetSandbox(t, createResp.Body.SandboxID, client)
			mockMgr.AddWaitReconcileKey(sbx)

			// Schedule async status update BEFORE calling blocking PauseSandbox
			go UpdateSandboxWhen(t, client, createResp.Body.SandboxID, func(sbx *v1alpha1.Sandbox) bool {
				return sbx.Spec.Paused == true
			}, DoSetSandboxStatus(v1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))

			_, apiError = controller.PauseSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			describeResp, apiError := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, timeoutAfterPaused, describeResp.Body.EndAt)
			tt.pauseChecker(t, GetSandbox(t, createResp.Body.SandboxID, client))
			go UpdateSandboxWhen(t, client, createResp.Body.SandboxID, func(sbx *v1alpha1.Sandbox) bool {
				return sbx.Spec.Paused == false
			}, DoSetSandboxStatus(v1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
			connectResp, apiError := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: timeoutSeconds,
			}, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, timeoutTime, connectResp.Body.EndAt)
			tt.resumeChecker(t, GetSandbox(t, createResp.Body.SandboxID, client))
		})
	}
}

func TestDeleteSandbox(t *testing.T) {
	templateName := "test-template"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
	}

	tests := []struct {
		name          string
		sandboxID     string // if not set, use the created sandbox ID
		mockDeleteErr error
		expectStatus  int
	}{
		{
			name:         "delete running sandbox successfully",
			expectStatus: http.StatusNoContent,
		},
		{
			name:         "delete non-existent sandbox returns success (idempotent)",
			sandboxID:    "non-existent-sandbox",
			expectStatus: http.StatusNoContent,
		},
		{
			name:          "delete sandbox with kill error",
			mockDeleteErr: fmt.Errorf("mock delete error"),
			expectStatus:  http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller, _, teardown := Setup(t)
			defer teardown()
			_ = CreateSandboxPool(t, controller, templateName, 1)
			// Note: do not defer cleanup() because sandbox may be deleted during test

			createResp, err := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: templateName,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, err)
			assert.Equal(t, models.SandboxStateRunning, createResp.Body.State)

			// Decorator: DefaultDeleteSandbox - control delete result (set after create)
			if tt.mockDeleteErr != nil {
				origDeleteSandbox := sandboxcr.DefaultDeleteSandbox
				sandboxcr.DefaultDeleteSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, client ctrlclient.Client) error {
					return tt.mockDeleteErr
				}
				t.Cleanup(func() { sandboxcr.DefaultDeleteSandbox = origDeleteSandbox })
			}

			sandboxID := tt.sandboxID
			if sandboxID == "" {
				sandboxID = createResp.Body.SandboxID
			}

			deleteResp, apiErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": sandboxID,
			}, user))

			if tt.expectStatus >= 300 {
				require.NotNil(t, apiErr)
				if apiErr.Code == 0 {
					apiErr.Code = http.StatusInternalServerError
				}
				assert.Equal(t, tt.expectStatus, apiErr.Code)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.expectStatus, deleteResp.Code)
			}
		})
	}
}

func TestDeleteSandbox_ReleasesQuotaAfterAcceptedDelete(t *testing.T) {
	fakeQuota := &fakeQuotaManager{}
	controller, _, teardown := SetupWithQuota(t, fakeQuota)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:        uuid.New(),
		Key:       InitKey,
		Name:      "admin",
		Team:      models.AdminTeam(),
		QuotaSpec: quotaSpecForAPIKeyTest(2),
	}

	cleanup := CreateSandboxPool(t, controller, "test-template", 1)
	_ = cleanup

	createResp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "test-template",
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, apiErr)
	require.NotNil(t, createResp.Body)

	deleteResp, apiErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))

	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, deleteResp.Code)
	assert.Equal(t, int64(1), fakeQuota.releaseCalls.Load())
	assert.True(t, fakeQuota.releaseHasDeadline.Load())
	assert.Equal(t, user.ID.String(), fakeQuota.lastRelease.User)
	assert.NotEmpty(t, fakeQuota.lastRelease.LockString)
}

func TestDeleteSandbox_UnlimitedKeyDoesNotReleaseQuota(t *testing.T) {
	fakeQuota := &fakeQuotaManager{}
	controller, _, teardown := SetupWithQuota(t, fakeQuota)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}

	cleanup := CreateSandboxPool(t, controller, "test-template", 1)
	_ = cleanup

	createResp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "test-template",
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, apiErr)
	require.NotNil(t, createResp.Body)

	deleteResp, apiErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))

	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, deleteResp.Code)
	assert.Equal(t, int64(0), fakeQuota.releaseCalls.Load())
}

type deleteReleaseQuotaManager struct {
	release func(context.Context, quota.ReleaseRequest) error

	releaseCalls       atomic.Int64
	releaseHasDeadline atomic.Bool
	lastRelease        quota.ReleaseRequest
}

func (m *deleteReleaseQuotaManager) Acquire(context.Context, quota.AcquireRequest) error {
	return nil
}

func (m *deleteReleaseQuotaManager) Release(ctx context.Context, req quota.ReleaseRequest) error {
	if _, ok := ctx.Deadline(); ok {
		m.releaseHasDeadline.Store(true)
	}
	m.lastRelease = req
	m.releaseCalls.Add(1)
	if m.release != nil {
		return m.release(ctx, req)
	}
	return nil
}

func (m *deleteReleaseQuotaManager) Cleanup(context.Context, string) error {
	return nil
}

func assertShortQuotaRequestReleaseDeadline(t *testing.T, ctx context.Context) {
	t.Helper()

	deadline, ok := ctx.Deadline()
	require.True(t, ok, "release should use a bounded context")
	remaining := time.Until(deadline)
	require.Greater(t, remaining, time.Duration(0), "release deadline should not already be expired")
	require.LessOrEqual(t, remaining, infra.SandboxAdmissionReleaseTimeout+50*time.Millisecond)
	assert.Less(t, infra.SandboxAdmissionReleaseTimeout, time.Second)
}

func TestDeleteSandbox_ReleaseErrorsStillReturnAcceptedDelete(t *testing.T) {
	tests := []struct {
		name    string
		release func(*testing.T, context.Context, quota.ReleaseRequest) error
	}{
		{
			name: "immediate release error",
			release: func(t *testing.T, ctx context.Context, req quota.ReleaseRequest) error {
				assertShortQuotaRequestReleaseDeadline(t, ctx)
				return fmt.Errorf("mock release error")
			},
		},
		{
			name: "release deadline exceeded",
			release: func(t *testing.T, ctx context.Context, req quota.ReleaseRequest) error {
				assertShortQuotaRequestReleaseDeadline(t, ctx)
				<-ctx.Done()
				return ctx.Err()
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeQuota := &deleteReleaseQuotaManager{
				release: func(ctx context.Context, req quota.ReleaseRequest) error {
					return tt.release(t, ctx, req)
				},
			}
			controller, _, teardown := SetupWithQuota(t, fakeQuota)
			defer teardown()

			user := &models.CreatedTeamAPIKey{
				ID:        uuid.New(),
				Key:       InitKey,
				Name:      "admin",
				Team:      models.AdminTeam(),
				QuotaSpec: quotaSpecForAPIKeyTest(2),
			}

			cleanup := CreateSandboxPool(t, controller, "test-template", 1)
			_ = cleanup

			createResp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
				TemplateID: "test-template",
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				},
			}, nil, user))
			require.Nil(t, apiErr)
			require.NotNil(t, createResp.Body)

			deleteResp, apiErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))

			require.Nil(t, apiErr)
			assert.Equal(t, http.StatusNoContent, deleteResp.Code)
			assert.Equal(t, int64(1), fakeQuota.releaseCalls.Load())
			assert.True(t, fakeQuota.releaseHasDeadline.Load())
			assert.Equal(t, user.ID.String(), fakeQuota.lastRelease.User)
			assert.NotEmpty(t, fakeQuota.lastRelease.LockString)
		})
	}
}

func TestDeleteSandbox_MissingSandboxOrLookupFailureDoesNotReleaseQuota(t *testing.T) {
	tests := []struct {
		name        string
		setupReq    func(t *testing.T, controller *Controller) *http.Request
		wantStatus  int
		expectError string
	}{
		{
			name: "missing sandbox returns idempotent success without release",
			setupReq: func(t *testing.T, _ *Controller) *http.Request {
				user := &models.CreatedTeamAPIKey{
					ID:   uuid.New(),
					Key:  InitKey,
					Name: "admin",
					Team: models.AdminTeam(),
				}
				return NewRequest(t, nil, nil, map[string]string{
					"sandboxID": "missing-sandbox",
				}, user)
			},
			wantStatus: http.StatusNoContent,
		},
		{
			name: "missing user returns unauthorized without release",
			setupReq: func(t *testing.T, _ *Controller) *http.Request {
				return NewRequest(t, nil, nil, map[string]string{
					"sandboxID": "any-sandbox",
				}, nil)
			},
			wantStatus:  http.StatusUnauthorized,
			expectError: "User not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeQuota := &fakeQuotaManager{}
			controller, _, teardown := SetupWithQuota(t, fakeQuota)
			defer teardown()

			cleanup := CreateSandboxPool(t, controller, "test-template", 1)
			defer cleanup()

			deleteResp, apiErr := controller.DeleteSandbox(tt.setupReq(t, controller))

			if tt.expectError != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, tt.wantStatus, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.expectError)
			} else {
				require.Nil(t, apiErr)
				assert.Equal(t, tt.wantStatus, deleteResp.Code)
			}
			assert.Equal(t, int64(0), fakeQuota.releaseCalls.Load())
		})
	}
}

func TestDeleteSandbox_DeleteFailureDoesNotReleaseQuota(t *testing.T) {
	fakeQuota := &fakeQuotaManager{}
	controller, _, teardown := SetupWithQuota(t, fakeQuota)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}

	cleanup := CreateSandboxPool(t, controller, "test-template", 1)
	defer cleanup()

	createResp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: "test-template",
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, apiErr)
	require.NotNil(t, createResp.Body)

	origDeleteSandbox := sandboxcr.DefaultDeleteSandbox
	sandboxcr.DefaultDeleteSandbox = func(context.Context, *v1alpha1.Sandbox, ctrlclient.Client) error {
		return fmt.Errorf("mock delete error")
	}
	t.Cleanup(func() { sandboxcr.DefaultDeleteSandbox = origDeleteSandbox })

	_, apiErr = controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": createResp.Body.SandboxID,
	}, user))

	require.NotNil(t, apiErr)
	assert.Contains(t, apiErr.Message, "mock delete error")
	assert.Equal(t, int64(0), fakeQuota.releaseCalls.Load())
}

func TestDeleteSandboxDeadClaimedSandbox(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	sandbox := CreateClaimedSandboxCR(t, controller, Namespace, "dead-delete-sandbox", "test-template", user.ID.String(), nil)
	sandboxID := fmt.Sprintf("%s--%s", sandbox.Namespace, sandbox.Name)
	UpdateSandboxWhen(t, fc, sandboxID, Immediately, DoSetSandboxStatus(v1alpha1.SandboxRunning, "", metav1.ConditionFalse))
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Name}, &v1alpha1.Sandbox{}))

	deleteCalls := 0
	origDeleteSandbox := sandboxcr.DefaultDeleteSandbox
	sandboxcr.DefaultDeleteSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, client ctrlclient.Client) error {
		deleteCalls++
		return origDeleteSandbox(ctx, sbx, client)
	}
	t.Cleanup(func() { sandboxcr.DefaultDeleteSandbox = origDeleteSandbox })

	deleteResp, apiErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": sandboxID,
	}, user))

	require.Nil(t, apiErr)
	assert.Equal(t, http.StatusNoContent, deleteResp.Code)
	require.Equal(t, 1, deleteCalls)
	var getErr error
	require.Eventually(t, func() bool {
		got := &v1alpha1.Sandbox{}
		getErr = fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: sandbox.Namespace, Name: sandbox.Name}, got)
		return apierrors.IsNotFound(getErr)
	}, time.Second, 10*time.Millisecond)
	require.True(t, apierrors.IsNotFound(getErr), "expected sandbox to be deleted, got error: %v", getErr)
}

func TestDescribeSandboxDeadClaimedSandbox(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	sandbox := CreateClaimedSandboxCR(t, controller, Namespace, "dead-describe-sandbox", "test-template", user.ID.String(), nil)
	sandboxID := fmt.Sprintf("%s--%s", sandbox.Namespace, sandbox.Name)
	UpdateSandboxWhen(t, fc, sandboxID, Immediately, DoSetSandboxStatus(v1alpha1.SandboxRunning, "", metav1.ConditionFalse))

	describeResp, apiErr := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": sandboxID,
	}, user))

	// A dead sandbox should be treated as not found from the E2B API perspective,
	// because the E2B SDK cannot parse the "dead" state. Returning 404 allows
	// clients to detect sandbox removal via SandboxNotFoundException.
	assert.Nil(t, describeResp.Body)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.Code)
}

func TestDescribeSandboxReservedFailedSandboxReturnsNotFound(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	sandbox := CreateClaimedSandboxCR(t, controller, Namespace, "reserved-failed-describe", "test-template", user.ID.String(), nil)
	sandbox.Labels[v1alpha1.LabelSandboxReservedFailed] = v1alpha1.True
	require.NoError(t, fc.Update(t.Context(), sandbox))
	sandboxID := fmt.Sprintf("%s--%s", sandbox.Namespace, sandbox.Name)

	resp, apiErr := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
		"sandboxID": sandboxID,
	}, user))

	assert.Nil(t, resp.Body)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.Code)
}

func TestConnectSandboxDeadClaimedSandbox(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "admin",
		Team: models.AdminTeam(),
	}
	sandbox := CreateClaimedSandboxCR(t, controller, Namespace, "dead-connect-sandbox", "test-template", user.ID.String(), nil)
	sandboxID := fmt.Sprintf("%s--%s", sandbox.Namespace, sandbox.Name)
	UpdateSandboxWhen(t, fc, sandboxID, Immediately, DoSetSandboxStatus(v1alpha1.SandboxRunning, "", metav1.ConditionFalse))

	connectResp, apiErr := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
		TimeoutSeconds: 60,
	}, map[string]string{
		"sandboxID": sandboxID,
	}, user))

	assert.Equal(t, web.ApiResponse[*models.Sandbox]{}, connectResp)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusNotFound, apiErr.Code)
	assert.Contains(t, apiErr.Message, "is not healthy")
}

func TestSandboxNamespaceIsolationWithSameName(t *testing.T) {
	controller, fc, teardown := Setup(t)
	defer teardown()

	ownerID := uuid.New()
	teamAUser := &models.CreatedTeamAPIKey{
		ID:   ownerID,
		Key:  "team-a-key",
		Name: "team-a-user",
		Team: &models.Team{
			Name: "team-a",
		},
	}
	adminUser := &models.CreatedTeamAPIKey{
		ID:   ownerID,
		Key:  "admin-key",
		Name: "admin-user",
		Team: models.AdminTeam(),
	}

	teamASandbox := CreateClaimedSandboxCR(t, controller, "team-a", "shared-sandbox", "shared-template", ownerID.String(), map[string]string{
		"scope": "team-a",
	})
	teamBSandbox := CreateClaimedSandboxCR(t, controller, "team-b", "shared-sandbox", "shared-template", ownerID.String(), map[string]string{
		"scope": "team-b",
	})
	teamASandboxID := fmt.Sprintf("%s--%s", teamASandbox.Namespace, teamASandbox.Name)
	teamBSandboxID := fmt.Sprintf("%s--%s", teamBSandbox.Namespace, teamBSandbox.Name)

	t.Run("list is namespace-scoped for normal team and cluster-scoped for admin", func(t *testing.T) {
		teamAResp, apiErr := controller.ListSandboxes(NewRequest(t, nil, nil, nil, teamAUser))
		require.Nil(t, apiErr)
		require.Len(t, teamAResp.Body, 1)
		assert.Equal(t, teamASandboxID, teamAResp.Body[0].SandboxID)
		assert.Equal(t, "team-a", teamAResp.Body[0].Metadata["scope"])

		adminResp, apiErr := controller.ListSandboxes(NewRequest(t, nil, nil, nil, adminUser))
		require.Nil(t, apiErr)
		gotIDs := make([]string, 0, len(adminResp.Body))
		for _, sbx := range adminResp.Body {
			gotIDs = append(gotIDs, sbx.SandboxID)
		}
		assert.ElementsMatch(t, []string{teamASandboxID, teamBSandboxID}, gotIDs)
	})

	t.Run("get is namespace-scoped for normal team and cluster-scoped for admin", func(t *testing.T) {
		teamAResp, apiErr := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
			"sandboxID": teamASandboxID,
		}, teamAUser))
		require.Nil(t, apiErr)
		assert.Equal(t, teamASandboxID, teamAResp.Body.SandboxID)

		_, apiErr = controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
			"sandboxID": teamBSandboxID,
		}, teamAUser))
		require.NotNil(t, apiErr)
		assert.Equal(t, http.StatusNotFound, apiErr.Code)

		adminResp, apiErr := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
			"sandboxID": teamBSandboxID,
		}, adminUser))
		require.Nil(t, apiErr)
		assert.Equal(t, teamBSandboxID, adminResp.Body.SandboxID)
	})

	t.Run("delete cannot remove same-name sandbox from another namespace", func(t *testing.T) {
		resp, apiErr := controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
			"sandboxID": teamBSandboxID,
		}, teamAUser))
		require.Nil(t, apiErr)
		assert.Equal(t, http.StatusNoContent, resp.Code)

		got := &v1alpha1.Sandbox{}
		require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-b", Name: "shared-sandbox"}, got))

		resp, apiErr = controller.DeleteSandbox(NewRequest(t, nil, nil, map[string]string{
			"sandboxID": teamBSandboxID,
		}, adminUser))
		require.Nil(t, apiErr)
		assert.Equal(t, http.StatusNoContent, resp.Code)
		require.Eventually(t, func() bool {
			err := fc.Get(t.Context(), ctrlclient.ObjectKey{Namespace: "team-b", Name: "shared-sandbox"}, got)
			return apierrors.IsNotFound(err)
		}, time.Second, 10*time.Millisecond)
	})
}

func TestBrowserUse(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	user := &models.CreatedTeamAPIKey{
		ID: keys.AdminKeyID,
	}

	templateName := "browseruse-template"
	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	createResp, createErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
		TemplateID: templateName,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}, nil, user))
	require.Nil(t, createErr)

	sandboxID := createResp.Body.SandboxID
	expectedBody := `{"Browser":"Chrome","Protocol-Version":"1.3","User-Agent":"Test","V8-Version":"12.0","WebKit-Version":"537.36","webSocketDebuggerUrl":"ws://127.0.0.1:9222/devtools/browser/abc"}`

	origRequest := proxyutils.DefaultRequestFunc
	t.Cleanup(func() {
		proxyutils.DefaultRequestFunc = origRequest
	})

	t.Run("CDP port", func(t *testing.T) {
		tests := []struct {
			name         string
			query        map[string]string
			expectedPort int
			expectError  string
		}{
			{name: "uses default port when query missing", expectedPort: models.CDPPort},
			{name: "uses custom cdp port", query: map[string]string{"cdpPort": "9333"}, expectedPort: 9333},
			{name: "rejects non integer cdp port", query: map[string]string{"cdpPort": "abc"}, expectError: "Invalid cdpPort"},
			{name: "rejects out of range cdp port", query: map[string]string{"cdpPort": "65536"}, expectError: "Invalid cdpPort"},
			{name: "rejects zero cdp port", query: map[string]string{"cdpPort": "0"}, expectError: "Invalid cdpPort"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				proxyutils.DefaultRequestFunc = func(ctx context.Context, sbx *v1alpha1.Sandbox, method, path string, port int, body io.Reader) (*http.Response, error) {
					assert.Equal(t, "/json/version", path)
					assert.Equal(t, tt.expectedPort, port)
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(expectedBody)),
					}, nil
				}

				req := NewRequest(t, tt.query, nil, map[string]string{"sandboxID": sandboxID}, user)
				resp, apiErr := controller.BrowserUse(req)
				if tt.expectError != "" {
					require.NotNil(t, apiErr)
					assert.Equal(t, http.StatusBadRequest, apiErr.Code)
					assert.Contains(t, apiErr.Message, tt.expectError)
					return
				}

				require.Nil(t, apiErr)
				require.NotNil(t, resp.Body)
				assert.Equal(t, http.StatusOK, resp.Code)
				assert.Equal(t, "Chrome", resp.Body.Browser)
				assert.Contains(t, resp.Body.WebSocketDebuggerURL,
					fmt.Sprintf("wss://%d-%s.%s", tt.expectedPort, sandboxID, controller.domain))
			})
		}
	})

	t.Run("domain resolution", func(t *testing.T) {
		controller.domain = ""
		tests := []struct {
			name, host, path, expectURL, expectError string
		}{
			{
				name:      "customized ipv6 uses bracketed path-style address",
				host:      "2001:db8::1",
				path:      "/kruise/api/sandboxes/" + sandboxID + "/connect",
				expectURL: fmt.Sprintf("wss://[2001:db8::1]/kruise/%s/%d/devtools/browser/abc", sandboxID, models.CDPPort),
			},
			{name: "empty host returns 400 without upstream", path: "/sandboxes/" + sandboxID + "/connect", expectError: "empty host"},
		}

		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				upstreamCalled := false
				proxyutils.DefaultRequestFunc = func(ctx context.Context, sbx *v1alpha1.Sandbox, method, path string, port int, body io.Reader) (*http.Response, error) {
					upstreamCalled = true
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(expectedBody)),
					}, nil
				}

				req := NewRequest(t, nil, nil, map[string]string{"sandboxID": sandboxID}, user)
				req.Host = tt.host
				req.URL.Path = tt.path

				resp, apiErr := controller.BrowserUse(req)
				assert.Equal(t, tt.expectError == "", upstreamCalled)
				if tt.expectError != "" {
					require.NotNil(t, apiErr)
					assert.Equal(t, http.StatusBadRequest, apiErr.Code)
					assert.Contains(t, apiErr.Message, tt.expectError)
					return
				}
				require.Nil(t, apiErr)
				require.NotNil(t, resp.Body)
				assert.Equal(t, tt.expectURL, resp.Body.WebSocketDebuggerURL)
			})
		}
	})
}

func TestCreateSandbox_EmptyHostDoesNotClaim(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	templateName := "empty-host-template"
	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	defer cleanup()

	user := &models.CreatedTeamAPIKey{ID: keys.AdminKeyID}
	controller.domain = ""

	request := models.NewSandboxRequest{
		TemplateID: templateName,
		Metadata: map[string]string{
			models.ExtensionKeySkipInitRuntime: v1alpha1.True,
		},
	}
	req := NewRequest(t, nil, request, nil, user)
	req.Host = ""

	_, apiErr := controller.CreateSandbox(req)
	require.NotNil(t, apiErr)
	assert.Equal(t, http.StatusBadRequest, apiErr.Code)
	assert.Contains(t, apiErr.Message, "empty host")

	okReq := NewRequest(t, nil, request, nil, user)
	okReq.Host = "api.example.com"
	resp, apiErr := controller.CreateSandbox(okReq)
	require.Nil(t, apiErr)
	require.NotNil(t, resp.Body)
	assert.Equal(t, "example.com", resp.Body.Domain)
}
