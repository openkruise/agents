package e2b

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	testutils "github.com/openkruise/agents/test/utils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
)

func imageChecker(image string, controller *Controller) func(t *testing.T, resp *models.Sandbox) {
	return func(t *testing.T, resp *models.Sandbox) {
		sbx, err := controller.manager.GetClaimedSandbox(t.Context(), keys.AdminKeyID.String(), resp.SandboxID)
		assert.NoError(t, err)
		assert.Equal(t, image, sbx.GetImage())
	}
}

func TestCreateSandbox(t *testing.T) {
	controller, clientSet, teardown := Setup(t)
	defer teardown()

	// Create test runtime server for InitRuntime
	opts := testutils.TestRuntimeServerOptions{
		RunCommandResult: testutils.RunCommandResult{
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
		setup       func(t *testing.T, controller *Controller, clientSet *clients.ClientSet)
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
				Code:    0,
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
				assert.Empty(t, resp.EndAt)
				sbx, err := controller.manager.GetClaimedSandbox(t.Context(), keys.AdminKeyID.String(), resp.SandboxID)
				assert.NoError(t, err)
				assert.Equal(t, infra.TimeoutOptions{}, sbx.GetTimeout())
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
				},
			},
			setup: func(t *testing.T, controller *Controller, clientSet *clients.ClientSet) {
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
				_, err := clientSet.CoreV1().PersistentVolumes().Create(context.Background(), pv, metav1.CreateOptions{})
				require.NoError(t, err)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t, controller, clientSet)
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
				AccessToken: testutils.AccessToken,
			})
			require.Eventually(t, func() bool {
				list, err := controller.cache.ListSandboxesInPool(templateName)
				return err == nil && len(list) == tt.available
			}, time.Second, 50*time.Millisecond)
			defer cleanup()
			now := time.Now()
			if tt.request.Metadata == nil {
				tt.request.Metadata = make(map[string]string)
			}
			// mock runtime server is not supported in e2b layer, the runtime is tested in infra package
			tt.request.Metadata[models.ExtensionKeyClaimTimeout] = "1" // let errors like "no stock" stop early
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
					assert.Equal(t, v, sbx.Metadata[k], fmt.Sprintf("metadata key: %s", k))
				}
				startedAt, err := time.Parse(time.RFC3339, sbx.StartedAt)
				assert.NoError(t, err)
				assert.WithinDuration(t, now, startedAt, 5*time.Second)
				timeout := 300
				if tt.request.Timeout != 0 {
					timeout = tt.request.Timeout
				}
				if tt.request.Metadata[models.ExtensionKeyNeverTimeout] != v1alpha1.True {
					endAt, err := time.Parse(time.RFC3339, sbx.EndAt)
					assert.NoError(t, err)
					assert.WithinDuration(t, startedAt.Add(time.Duration(timeout)*time.Second), endAt, 5*time.Second)
				}
				assert.Equal(t, models.SandboxStateRunning, sbx.State)
			}
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
	client := controller.client.SandboxClient
	_, err := client.ApiV1alpha1().SandboxTemplates(Namespace).Create(t.Context(), sbt, metav1.CreateOptions{})
	require.NoError(t, err)
	// Wait for SandboxTemplate to be cached via API Get
	require.Eventually(t, func() bool {
		_, err := client.ApiV1alpha1().SandboxTemplates(Namespace).Get(t.Context(), checkpointID, metav1.GetOptions{})
		return err == nil
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
	_, err = client.ApiV1alpha1().Checkpoints(Namespace).Create(t.Context(), cp, metav1.CreateOptions{})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return controller.manager.GetInfra().HasCheckpoint(checkpointID)
	}, time.Second, 10*time.Millisecond)

	return func() {
		assert.NoError(t, client.ApiV1alpha1().SandboxTemplates(Namespace).Delete(context.Background(), checkpointID, metav1.DeleteOptions{}))
		assert.NoError(t, client.ApiV1alpha1().Checkpoints(Namespace).Delete(context.Background(), checkpointID, metav1.DeleteOptions{}))
	}
}

func TestCloneSandbox(t *testing.T) {
	controller, clientSet, teardown := Setup(t)
	defer teardown()

	// Create test runtime server for InitRuntime
	runtimeOpts := testutils.TestRuntimeServerOptions{
		RunCommandResult: testutils.RunCommandResult{
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
	sandboxcr.DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, client *clients.ClientSet) (*v1alpha1.Sandbox, error) {
		// Set Name (FakeClient does not handle GenerateName)
		if sbx.Name == "" && sbx.GenerateName != "" {
			sbx.Name = sbx.GenerateName + rand.String(5)
		}
		// Set RuntimeURL annotation and AccessToken
		if sbx.Annotations == nil {
			sbx.Annotations = map[string]string{}
		}
		sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = server.URL
		sbx.Annotations[v1alpha1.AnnotationRuntimeAccessToken] = testutils.AccessToken

		// Call original createSandbox
		created, err := origCreateSandbox(ctx, sbx, client)
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
		created, err = client.ApiV1alpha1().Sandboxes(created.Namespace).UpdateStatus(ctx, created, metav1.UpdateOptions{})
		if err != nil {
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
		setup       func(t *testing.T, controller *Controller, clientSet *clients.ClientSet)
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
			name: "clone success with secure mode",
			request: models.NewSandboxRequest{
				TemplateID: checkpointID,
				Timeout:    300,
				Secure:     true,
			},
			postCheck: func(t *testing.T, resp *models.Sandbox, controller *Controller) {
				// In secure mode, access token should be generated
				assert.NotEmpty(t, resp.SandboxID)
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
				sbx, err := controller.manager.GetClaimedSandbox(t.Context(), keys.AdminKeyID.String(), resp.SandboxID)
				assert.NoError(t, err)
				assert.Equal(t, infra.TimeoutOptions{}, sbx.GetTimeout())
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
				sbx, err := controller.manager.GetClaimedSandbox(t.Context(), keys.AdminKeyID.String(), resp.SandboxID)
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
			setup: func(t *testing.T, controller *Controller, clientSet *clients.ClientSet) {
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
				_, err := clientSet.CoreV1().PersistentVolumes().Create(context.Background(), pv, metav1.CreateOptions{})
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setup != nil {
				tt.setup(t, controller, clientSet)
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
				timeout := 300
				if tt.request.Timeout != 0 {
					timeout = tt.request.Timeout
				}
				if tt.request.Metadata[models.ExtensionKeyNeverTimeout] != v1alpha1.True {
					endAt, err := time.Parse(time.RFC3339, sbx.EndAt)
					assert.NoError(t, err)
					assert.WithinDuration(t, startedAt.Add(time.Duration(timeout)*time.Second), endAt, 5*time.Second)
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

func TestAutoPause(t *testing.T) {
	controller, client, teardown := Setup(t)
	defer teardown()
	timeout := 300
	now := time.Now()
	timeoutTime := now.Add(time.Duration(timeout) * time.Second)
	maxTimeoutTime := now.Add(time.Duration(models.DefaultMaxTimeout) * time.Second)
	timeoutAfterPaused := now.AddDate(1000, 0, 0)
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
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, maxTimeoutTime, 5*time.Second)
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
				if sbx.Spec.ShutdownTime != nil {
					assert.WithinDuration(t, sbx.Spec.ShutdownTime.Time, maxTimeoutTime, 5*time.Second)
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
				Timeout:    timeout,
				Metadata: map[string]string{
					models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				},
			}, nil, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, timeoutTime, createResp.Body.EndAt)
			tt.createChecker(t, GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient))
			AvoidGetFromCache(t, createResp.Body.SandboxID, client.SandboxClient)

			_, apiError = controller.PauseSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			UpdateSandboxWhen(t, client.SandboxClient, createResp.Body.SandboxID, func(sbx *v1alpha1.Sandbox) bool {
				return sbx.Spec.Paused == true
			}, DoSetSandboxStatus(v1alpha1.SandboxPaused, metav1.ConditionTrue, metav1.ConditionFalse))
			describeResp, apiError := controller.DescribeSandbox(NewRequest(t, nil, nil, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, timeoutAfterPaused, describeResp.Body.EndAt)
			tt.pauseChecker(t, GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient))
			var wg sync.WaitGroup
			wg.Add(1)
			go func() {
				defer wg.Done()
				UpdateSandboxWhen(t, client.SandboxClient, createResp.Body.SandboxID, func(sbx *v1alpha1.Sandbox) bool {
					return sbx.Spec.Paused == false
				}, DoSetSandboxStatus(v1alpha1.SandboxRunning, metav1.ConditionFalse, metav1.ConditionTrue))
			}()
			connectResp, apiError := controller.ConnectSandbox(NewRequest(t, nil, models.SetTimeoutRequest{
				TimeoutSeconds: timeout,
			}, map[string]string{
				"sandboxID": createResp.Body.SandboxID,
			}, user))
			assert.Nil(t, apiError)
			AssertEndAt(t, timeoutTime, connectResp.Body.EndAt)
			tt.resumeChecker(t, GetSandbox(t, createResp.Body.SandboxID, client.SandboxClient))
			wg.Wait()
		})
	}
}
