package e2b

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
)

func TestCreateSnapshot(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	templateName := "snapshot-test-template"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	// Define context key types for checkpoint status override
	type cpStatusKey struct{}
	type tmplOverrideKey struct{}
	type tmplOverride struct {
		Name string
		UID  types.UID
	}

	// Decorator 1: DefaultCreateSandboxTemplate - handle FakeClient not supporting GenerateName
	origCreateSandboxTemplate := sandboxcr.DefaultCreateSandboxTemplate
	sandboxcr.DefaultCreateSandboxTemplate = func(ctx context.Context, client clients.SandboxClient, tmpl *v1alpha1.SandboxTemplate) (*v1alpha1.SandboxTemplate, error) {
		// Handle GenerateName for FakeClient
		if tmpl.Name == "" && tmpl.GenerateName != "" {
			tmpl.Name = tmpl.GenerateName + rand.String(5)
		}
		// Apply override from context if present
		if override, ok := ctx.Value(tmplOverrideKey{}).(tmplOverride); ok {
			if override.Name != "" {
				tmpl.Name = override.Name
			}
			if override.UID != "" {
				tmpl.UID = override.UID
			}
		}
		return origCreateSandboxTemplate(ctx, client, tmpl)
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateSandboxTemplate = origCreateSandboxTemplate })

	// Decorator 2: DefaultCreateCheckpoint - set checkpoint status to Succeeded with CheckpointId
	origCreateCheckpoint := sandboxcr.DefaultCreateCheckpoint
	sandboxcr.DefaultCreateCheckpoint = func(ctx context.Context, client clients.SandboxClient, cp *v1alpha1.Checkpoint) (*v1alpha1.Checkpoint, error) {
		// Set status from context if present
		if status, ok := ctx.Value(cpStatusKey{}).(v1alpha1.CheckpointStatus); ok {
			cp.Status = status
		}
		created, err := origCreateCheckpoint(ctx, client, cp)
		if err != nil {
			return nil, err
		}
		// Wait for informer sync
		time.Sleep(50 * time.Millisecond)
		return created, nil
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateCheckpoint = origCreateCheckpoint })

	// Helper function to claim a sandbox and return its ID
	claimSandbox := func(t *testing.T) string {
		cleanup := CreateSandboxPool(t, controller, templateName, 1)
		t.Cleanup(cleanup)

		// Wait for sandbox pool to be ready
		require.Eventually(t, func() bool {
			list, err := controller.cache.ListSandboxesInPool(templateName)
			return err == nil && len(list) == 1
		}, time.Second, 50*time.Millisecond)

		// Claim a sandbox
		createResp, apiErr := controller.CreateSandbox(NewRequest(t, nil, models.NewSandboxRequest{
			TemplateID: templateName,
			Timeout:    600,
			Metadata: map[string]string{
				models.ExtensionKeySkipInitRuntime: v1alpha1.True,
				models.ExtensionKeyClaimTimeout:    "1",
			},
		}, nil, user))
		require.Nil(t, apiErr)
		require.NotNil(t, createResp.Body)
		return createResp.Body.SandboxID
	}

	tests := []struct {
		name        string
		setupCtx    func(ctx context.Context) context.Context
		setupReq    func(t *testing.T, sandboxID string) *http.Request
		getSandbox  func(t *testing.T) string
		expectError bool
		errorCode   int
		errorMsg    string
		postCheck   func(t *testing.T, resp *models.Snapshot)
	}{
		{
			name: "success with minimal options",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-minimal",
				})
				ctx = context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-minimal", UID: "uid-minimal"})
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, models.NewSnapshotRequest{
					Name: "test-snapshot",
				}, map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "snapshot-id-minimal", resp.SnapshotID)
			},
		},
		{
			name: "success with all options",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-all-opts",
				})
				ctx = context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-all-opts", UID: "uid-all-opts"})
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				req := NewRequest(t, nil, models.NewSnapshotRequest{
					Name: "test-snapshot-all-opts",
				}, map[string]string{
					"sandboxID": sandboxID,
				}, user)
				// Set extension headers
				req.Header.Set(models.ExtensionHeaderSnapshotKeepRunning, "true")
				req.Header.Set(models.ExtensionHeaderSnapshotTTL, "30m")
				req.Header.Set(models.ExtensionHeaderSnapshotPersistentContents, "memory,filesystem")
				req.Header.Set(models.ExtensionHeaderWaitSuccessSeconds, "60")
				return req
			},
			getSandbox: claimSandbox,
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "snapshot-id-all-opts", resp.SnapshotID)
			},
		},
		{
			name: "sandbox not found",
			setupCtx: func(ctx context.Context) context.Context {
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, models.NewSnapshotRequest{
					Name: "test-snapshot",
				}, map[string]string{
					"sandboxID": "default--non-existent-sandbox",
				}, user)
			},
			getSandbox: func(t *testing.T) string {
				return "default--non-existent-sandbox"
			},
			expectError: true,
			errorCode:   http.StatusNotFound,
			errorMsg:    "Cannot get sandbox",
		},
		{
			name: "invalid request body",
			setupCtx: func(ctx context.Context) context.Context {
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				// Create request with invalid body (string instead of object)
				return NewRequest(t, nil, "invalid-json-body", map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox:  claimSandbox,
			expectError: true,
			errorCode:   0, // Default error code
			errorMsg:    "cannot unmarshal",
		},
		{
			name: "checkpoint creation failed",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:   v1alpha1.CheckpointFailed,
					Message: "disk full",
				})
				ctx = context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-failed", UID: "uid-failed"})
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, models.NewSnapshotRequest{
					Name: "test-snapshot-fail",
				}, map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox:  claimSandbox,
			expectError: true,
			errorCode:   0,
			errorMsg:    "failed",
		},
		{
			name: "sandbox state is not running",
			setupCtx: func(ctx context.Context) context.Context {
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, models.NewSnapshotRequest{
					Name: "test-snapshot-paused",
				}, map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: func(t *testing.T) string {
				sandboxID := claimSandbox(t)
				// Update sandbox to paused state
				sbx := GetSandbox(t, sandboxID, controller.client.SandboxClient)
				sbx.Spec.Paused = true
				_, err := controller.client.ApiV1alpha1().Sandboxes(sbx.Namespace).Update(context.Background(), sbx, metav1.UpdateOptions{})
				require.NoError(t, err)
				time.Sleep(200 * time.Millisecond) // wait cache sync
				return sandboxID
			},
			expectError: true,
			errorCode:   http.StatusBadRequest,
			errorMsg:    "is not running",
		},
		{
			name: "parse extension error with invalid wait success seconds",
			setupCtx: func(ctx context.Context) context.Context {
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				req := NewRequest(t, nil, models.NewSnapshotRequest{
					Name: "test-snapshot-bad-extension",
				}, map[string]string{
					"sandboxID": sandboxID,
				}, user)
				// Set invalid header value: negative number for WaitSuccessSeconds
				req.Header.Set(models.ExtensionHeaderWaitSuccessSeconds, "-1")
				return req
			},
			getSandbox:  claimSandbox,
			expectError: true,
			errorCode:   http.StatusBadRequest,
			errorMsg:    "Bad extension param",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandboxID := tt.getSandbox(t)
			req := tt.setupReq(t, sandboxID)

			// Apply context modifications
			ctx := tt.setupCtx(req.Context())
			req = req.WithContext(ctx)

			resp, apiErr := controller.CreateSnapshot(req)

			if tt.expectError {
				require.NotNil(t, apiErr, "expected error but got nil")
				if tt.errorCode != 0 {
					assert.Equal(t, tt.errorCode, apiErr.Code)
				}
				if tt.errorMsg != "" {
					assert.Contains(t, apiErr.Message, tt.errorMsg)
				}
			} else {
				require.Nil(t, apiErr, "unexpected error: %v", apiErr)
				require.NotNil(t, resp.Body, "response body should not be nil")
				assert.NotEmpty(t, resp.Body.SnapshotID, "SnapshotID should not be empty")
				assert.Equal(t, http.StatusCreated, resp.Code)
				if tt.postCheck != nil {
					tt.postCheck(t, resp.Body)
				}
			}
		})
	}
}
