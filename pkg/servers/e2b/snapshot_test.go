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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/servers/e2b/keys"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
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
	unauthorizedUser := &models.CreatedTeamAPIKey{
		ID:   uuid.New(),
		Key:  "unauthorized-key",
		Name: "unauthorized-user",
	}

	// Define context key types for checkpoint status override
	type cpStatusKey struct{}
	type tmplOverrideKey struct{}
	type createHookKey struct{}
	type tmplOverride struct {
		Name string
		UID  types.UID
	}

	// Decorator 1: DefaultCreateSandboxTemplate - handle FakeClient not supporting GenerateName
	origCreateSandboxTemplate := sandboxcr.DefaultCreateSandboxTemplate
	sandboxcr.DefaultCreateSandboxTemplate = func(ctx context.Context, c ctrlclient.Client, tmpl *v1alpha1.SandboxTemplate) (*v1alpha1.SandboxTemplate, error) {
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
		return origCreateSandboxTemplate(ctx, c, tmpl)
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateSandboxTemplate = origCreateSandboxTemplate })

	// Decorator 2: DefaultCreateCheckpoint - set checkpoint status to Succeeded with CheckpointId
	var createCount int
	var lastContents []string
	origCreateCheckpoint := sandboxcr.DefaultCreateCheckpoint
	sandboxcr.DefaultCreateCheckpoint = func(ctx context.Context, c ctrlclient.Client, cp *v1alpha1.Checkpoint) (*v1alpha1.Checkpoint, error) {
		if hook, ok := ctx.Value(createHookKey{}).(func()); ok {
			hook()
		}
		// The API server sets CreationTimestamp; the fake client does not, and
		// alias reconciliation ranks checkpoints by it.
		if cp.CreationTimestamp.IsZero() {
			cp.CreationTimestamp = metav1.Now()
		}
		createCount++
		lastContents = append([]string(nil), cp.Spec.PersistentContents...)
		// Set status from context if present
		if status, ok := ctx.Value(cpStatusKey{}).(v1alpha1.CheckpointStatus); ok {
			cp.Status = status
		}
		created, err := origCreateCheckpoint(ctx, c, cp)
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
			list, err := controller.cache.ListSandboxesInPool(t.Context(), cache.ListSandboxesInPoolOptions{
				Pool: templateName,
			})
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

	// namedCheckpoint builds a Succeeded checkpoint of the test user carrying
	// the alias at the given whole-second metadata creation time.
	namedCheckpoint := func(name, alias string, created time.Time) *v1alpha1.Checkpoint {
		return &v1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:              name,
				Namespace:         Namespace,
				CreationTimestamp: metav1.NewTime(created),
				Labels: map[string]string{
					v1alpha1.CheckpointLabelAlias: alias,
				},
				Annotations: map[string]string{
					v1alpha1.AnnotationOwner: user.ID.String(),
				},
			},
			Status: v1alpha1.CheckpointStatus{
				Phase:        v1alpha1.CheckpointSucceeded,
				CheckpointId: name + "-id",
			},
		}
	}
	// seedCheckpoint creates the checkpoint and waits until the informer cache
	// can resolve it by its alias.
	seedCheckpoint := func(t *testing.T, seeded *v1alpha1.Checkpoint) {
		t.Helper()
		fc := getTestCRClient(controller)
		require.NoError(t, fc.Create(context.Background(), seeded))
		require.NoError(t, fc.Status().Update(context.Background(), seeded))
		t.Cleanup(func() { _ = fc.Delete(context.Background(), seeded) })
		require.Eventually(t, func() bool {
			cps, err := controller.cache.ListCheckpoints(t.Context(), cache.ListCheckpointsOptions{
				Namespace:      Namespace,
				MatchingLabels: map[string]string{v1alpha1.CheckpointLabelAlias: seeded.Labels[v1alpha1.CheckpointLabelAlias]},
			})
			if err != nil {
				return false
			}
			for _, cp := range cps {
				if cp.Name == seeded.Name && cp.Status.Phase == v1alpha1.CheckpointSucceeded {
					return true
				}
			}
			return false
		}, 2*time.Second, 50*time.Millisecond)
	}
	seedNamedCheckpointAt := func(t *testing.T, name, alias string, created time.Time) {
		seedCheckpoint(t, namedCheckpoint(name, alias, created))
	}
	seedNamedCheckpoint := func(t *testing.T, name, alias string) {
		seedNamedCheckpointAt(t, name, alias, time.Now().Add(-time.Minute))
	}

	tests := []struct {
		name           string
		setupCtx       func(ctx context.Context) context.Context
		setupReq       func(t *testing.T, sandboxID string) *http.Request
		getSandbox     func(t *testing.T) string
		errorCode      int
		errorMsg       string
		expectResource bool
		forbidCreate   bool
		wantNames      []string
		postCheck      func(t *testing.T, resp *models.Snapshot)
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
				return NewRequest(t, nil, json.RawMessage(`{}`), map[string]string{
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
				req := NewRequest(t, nil, json.RawMessage(`{}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
				req.Header.Set(models.ExtensionHeaderSnapshotKeepRunning, "true")
				req.Header.Set(models.ExtensionHeaderSnapshotTTL, "30m")
				req.Header.Set(models.ExtensionHeaderSnapshotPersistentContents, "filesystem")
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
				return NewRequest(t, nil, json.RawMessage(`{}`), map[string]string{
					"sandboxID": Namespace + "--non-existent-sandbox",
				}, user)
			},
			getSandbox: func(t *testing.T) string {
				return Namespace + "--non-existent-sandbox"
			},
			errorCode: http.StatusNotFound,
			errorMsg:  "Cannot get sandbox",
		},
		{
			name: "unauthorized sandbox does not expose resource context",
			setupCtx: func(ctx context.Context) context.Context {
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, json.RawMessage(`{}`), map[string]string{
					"sandboxID": sandboxID,
				}, unauthorizedUser)
			},
			getSandbox: claimSandbox,
			errorCode:  http.StatusNotFound,
			errorMsg:   "Cannot get sandbox",
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
			getSandbox: claimSandbox,
			errorCode:  http.StatusBadRequest,
			errorMsg:   "invalid snapshot request body",
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
				return NewRequest(t, nil, json.RawMessage(`{}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox:     claimSandbox,
			errorCode:      0,
			errorMsg:       "failed",
			expectResource: true,
		},
		{
			name: "sandbox state is not running",
			setupCtx: func(ctx context.Context) context.Context {
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, json.RawMessage(`{}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: func(t *testing.T) string {
				sandboxID := claimSandbox(t)
				// Update sandbox to paused state
				fc := getTestCRClient(controller)
				sbx := GetSandbox(t, sandboxID, fc)
				sbx.Spec.Paused = true
				err := fc.Update(context.Background(), sbx)
				require.NoError(t, err)
				time.Sleep(200 * time.Millisecond) // wait cache sync
				return sandboxID
			},
			errorCode:      http.StatusBadRequest,
			errorMsg:       "is not running",
			expectResource: true,
		},
		{
			name: "parse extension error with invalid wait success seconds",
			setupCtx: func(ctx context.Context) context.Context {
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				req := NewRequest(t, nil, json.RawMessage(`{}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
				// Set invalid header value: negative number for WaitSuccessSeconds
				req.Header.Set(models.ExtensionHeaderWaitSuccessSeconds, "-1")
				return req
			},
			getSandbox: claimSandbox,
			errorCode:  http.StatusBadRequest,
			errorMsg:   "Bad extension param",
		},
		{
			name: "memory true creates a memory and filesystem checkpoint",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-memory",
				})
				return context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-memory", UID: "uid-memory"})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, json.RawMessage(`{"memory":true}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "snapshot-id-memory", resp.SnapshotID)
				assert.ElementsMatch(t, []string{
					v1alpha1.CheckpointPersistentContentMemory,
					v1alpha1.CheckpointPersistentContentFilesystem,
				}, lastContents)
			},
		},
		{
			name: "explicit snapshot name creates a named snapshot",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-named",
				})
				return context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-named", UID: "uid-named"})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, json.RawMessage(`{"name":"snap"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			wantNames:  []string{"snap"},
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "snapshot-id-named", resp.SnapshotID)
			},
		},
		{
			name: "named snapshot replaces the succeeded snapshot with the same name",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-replaced",
				})
				return context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-replaced", UID: "uid-replaced"})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				seedNamedCheckpoint(t, "old-named-checkpoint", "snap-replace")
				return NewRequest(t, nil, json.RawMessage(`{"name":"snap-replace"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			wantNames:  []string{"snap-replace"},
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "snapshot-id-replaced", resp.SnapshotID)
				var stale v1alpha1.Checkpoint
				err := getTestCRClient(controller).Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: "old-named-checkpoint"}, &stale)
				assert.True(t, apierrors.IsNotFound(err), "old named checkpoint should be deleted, got %v", err)
			},
		},
		{
			name: "named snapshot keeps the old snapshot when creation fails",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:   v1alpha1.CheckpointFailed,
					Message: "disk full",
				})
				return context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-keep", UID: "uid-keep"})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				seedNamedCheckpoint(t, "old-keep-checkpoint", "snap-keep")
				return NewRequest(t, nil, json.RawMessage(`{"name":"snap-keep"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox:     claimSandbox,
			errorCode:      http.StatusInternalServerError,
			errorMsg:       "failed",
			expectResource: true,
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				var kept v1alpha1.Checkpoint
				err := getTestCRClient(controller).Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: "old-keep-checkpoint"}, &kept)
				assert.NoError(t, err, "existing named snapshot must survive a failed replacement")
			},
		},
		{
			name: "named snapshot deletes the old snapshot only after the new one is created",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-order",
				})
				ctx = context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-order", UID: "uid-order"})
				return context.WithValue(ctx, createHookKey{}, func() {
					var stale v1alpha1.Checkpoint
					err := getTestCRClient(controller).Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: "old-order-checkpoint"}, &stale)
					assert.NoError(t, err, "old named snapshot must exist while the new one is being created")
				})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				seedNamedCheckpoint(t, "old-order-checkpoint", "snap-order")
				return NewRequest(t, nil, json.RawMessage(`{"name":"snap-order"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			wantNames:  []string{"snap-order"},
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "snapshot-id-order", resp.SnapshotID)
				var stale v1alpha1.Checkpoint
				err := getTestCRClient(controller).Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: "old-order-checkpoint"}, &stale)
				assert.True(t, apierrors.IsNotFound(err), "old named checkpoint should be deleted after replacement, got %v", err)
			},
		},
		{
			name: "named snapshot removes every older holder of the alias",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-multi",
				})
				return context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-multi", UID: "uid-multi"})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				seedNamedCheckpointAt(t, "old-multi-1", "snap-multi", time.Now().Add(-2*time.Minute))
				seedNamedCheckpointAt(t, "old-multi-2", "snap-multi", time.Now().Add(-time.Minute))
				return NewRequest(t, nil, json.RawMessage(`{"name":"snap-multi"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			wantNames:  []string{"snap-multi"},
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.NotEmpty(t, resp.SnapshotID)
				fc := getTestCRClient(controller)
				for _, name := range []string{"old-multi-1", "old-multi-2"} {
					var stale v1alpha1.Checkpoint
					err := fc.Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: name}, &stale)
					assert.True(t, apierrors.IsNotFound(err), "older alias holder %s should be deleted, got %v", name, err)
				}
			},
		},
		{
			name: "superseded named snapshot yields to the newer holder",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-superseded",
				})
				return context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-superseded", UID: "uid-superseded"})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				seedNamedCheckpointAt(t, "newer-holder", "snap-newer", time.Now().Add(time.Hour))
				return NewRequest(t, nil, json.RawMessage(`{"name":"snap-newer"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			wantNames:  []string{"snap-newer"},
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "newer-holder-id", resp.SnapshotID)
				var kept v1alpha1.Checkpoint
				err := getTestCRClient(controller).Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: "newer-holder"}, &kept)
				assert.NoError(t, err, "newer alias holder must be preserved")
			},
		},
		{
			name: "named snapshot ignores other owners' snapshots with the same name",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-own",
				})
				return context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-own", UID: "uid-own"})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				// A newer holder would win if other owners took part in the replacement.
				other := namedCheckpoint("other-owner-holder", "snap-shared", time.Now().Add(time.Hour))
				other.Annotations[v1alpha1.AnnotationOwner] = unauthorizedUser.ID.String()
				seedCheckpoint(t, other)
				return NewRequest(t, nil, json.RawMessage(`{"name":"snap-shared"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			wantNames:  []string{"snap-shared"},
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "snapshot-id-own", resp.SnapshotID)
				var kept v1alpha1.Checkpoint
				err := getTestCRClient(controller).Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: "other-owner-holder"}, &kept)
				assert.NoError(t, err, "another owner's snapshot must not be replaced")
			},
		},
		{
			name: "recorded creation time outranks the whole-second metadata time",
			setupCtx: func(ctx context.Context) context.Context {
				ctx = context.WithValue(ctx, cpStatusKey{}, v1alpha1.CheckpointStatus{
					Phase:        v1alpha1.CheckpointSucceeded,
					CheckpointId: "snapshot-id-recorded",
				})
				return context.WithValue(ctx, tmplOverrideKey{}, tmplOverride{Name: "tmpl-recorded", UID: "uid-recorded"})
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				// The metadata time alone would rank the older holder as newest.
				older := namedCheckpoint("zzz-recorded-holder", "snap-recorded", time.Now().Add(time.Hour))
				older.Annotations[v1alpha1.AnnotationCheckpointCreationTime] = time.Now().Add(-time.Second).UTC().Format(time.RFC3339Nano)
				seedCheckpoint(t, older)
				return NewRequest(t, nil, json.RawMessage(`{"name":"snap-recorded"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox: claimSandbox,
			wantNames:  []string{"snap-recorded"},
			postCheck: func(t *testing.T, resp *models.Snapshot) {
				assert.Equal(t, "snapshot-id-recorded", resp.SnapshotID)
				var stale v1alpha1.Checkpoint
				err := getTestCRClient(controller).Get(context.Background(), types.NamespacedName{Namespace: Namespace, Name: "zzz-recorded-holder"}, &stale)
				assert.True(t, apierrors.IsNotFound(err), "older alias holder should be deleted, got %v", err)
			},
		},
		{
			name: "invalid snapshot name is rejected by the sandbox backend",
			setupCtx: func(ctx context.Context) context.Context {
				return ctx
			},
			setupReq: func(t *testing.T, sandboxID string) *http.Request {
				return NewRequest(t, nil, json.RawMessage(`{"name":"bad name!"}`), map[string]string{
					"sandboxID": sandboxID,
				}, user)
			},
			getSandbox:     claimSandbox,
			errorCode:      http.StatusBadRequest,
			errorMsg:       "invalid checkpoint alias",
			expectResource: true,
			forbidCreate:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			createsBefore := createCount
			sandboxID := tt.getSandbox(t)
			req := tt.setupReq(t, sandboxID)

			// Apply context modifications
			ctx := tt.setupCtx(req.Context())
			req = req.WithContext(ctx)

			resp, apiErr := controller.CreateSnapshot(req)

			if tt.errorMsg != "" {
				require.NotNil(t, apiErr, "expected error but got nil")
				if tt.errorCode != 0 {
					assert.Equal(t, tt.errorCode, apiErr.Code)
				}
				if tt.errorMsg != "" {
					assert.Contains(t, apiErr.Message, tt.errorMsg)
				}
				if tt.expectResource {
					assert.Contains(t, apiErr.Message, "sandboxResource=")
				} else {
					assert.NotContains(t, apiErr.Message, "sandboxResource=")
				}
				if tt.forbidCreate {
					assert.Equal(t, createsBefore, createCount)
				}
			} else {
				require.Nil(t, apiErr, "unexpected error: %v", apiErr)
				require.NotNil(t, resp.Body, "response body should not be nil")
				assert.NotEmpty(t, resp.Body.SnapshotID, "SnapshotID should not be empty")
				assert.Equal(t, http.StatusCreated, resp.Code)
				wantNames := []string{}
				if tt.wantNames != nil {
					wantNames = tt.wantNames
				}
				assert.NotNil(t, resp.Body.Names)
				assert.Equal(t, wantNames, resp.Body.Names)
				encoded, marshalErr := json.Marshal(resp.Body)
				require.NoError(t, marshalErr)
				assert.Contains(t, string(encoded), fmt.Sprintf(`"names":%q`, wantNames))
				assert.Greater(t, createCount, createsBefore)
			}
			if tt.postCheck != nil {
				tt.postCheck(t, resp.Body)
			}
		})
	}
}

func TestParseCreateSnapshotRequest(t *testing.T) {
	sc := &Controller{}
	tests := []struct {
		name         string
		body         string
		headers      []string
		wantMessage  string
		wantContents []string
		wantName     string
	}{
		{name: "memory true", body: `{"memory":true}`, wantContents: []string{v1alpha1.CheckpointPersistentContentMemory, v1alpha1.CheckpointPersistentContentFilesystem}},
		{name: "memory true with filesystem header", body: `{"memory":true}`, headers: []string{"filesystem"}, wantContents: []string{v1alpha1.CheckpointPersistentContentFilesystem}},
		{name: "memory false", body: `{"memory":false}`, wantContents: []string{v1alpha1.CheckpointPersistentContentFilesystem}},
		{name: "omitted memory without header", body: `{}`},
		{name: "memory header", body: `{}`, headers: []string{"memory"}, wantContents: []string{v1alpha1.CheckpointPersistentContentMemory}},
		{name: "null memory", body: `{"memory":null}`},
		{name: "string memory", body: `{"memory":"false"}`, wantMessage: "invalid snapshot request body"},
		{name: "explicit name", body: `{"name":"snap"}`, wantName: "snap"},
		// The name format is a sandbox-backend storage constraint, so parsing
		// keeps an invalid name and the backend rejects it.
		{name: "invalid name passes parse and is rejected by the backend", body: `{"name":"bad name!"}`, wantName: "bad name!"},
		{name: "empty name", body: `{"name":""}`},
		{name: "null name", body: `{"name":null}`},
		{name: "missing body", body: ``, wantMessage: "snapshot request body is required"},
		{name: "null body", body: `null`, wantMessage: "invalid snapshot request body: must be a JSON object"},
		{name: "non object", body: `[]`, wantMessage: "invalid snapshot request body"},
		{name: "invalid persistent content", body: `{}`, headers: []string{"disk"}, wantMessage: "Bad extension param"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := strings.NewReader(tt.body)
			req := httptest.NewRequest(http.MethodPost, "/sandboxes/sbx/snapshots", body)
			req.ContentLength = -1
			for _, header := range tt.headers {
				req.Header.Add(models.ExtensionHeaderSnapshotPersistentContents, header)
			}
			request, apiErr := sc.parseCreateSnapshotRequest(req)
			if tt.wantMessage != "" {
				require.NotNil(t, apiErr)
				assert.Equal(t, http.StatusBadRequest, apiErr.Code)
				assert.Contains(t, apiErr.Message, tt.wantMessage)
				return
			}
			require.Nil(t, apiErr)
			assert.ElementsMatch(t, tt.wantContents, request.Extensions.PersistentContents)
			if tt.wantName != "" {
				require.NotNil(t, request.Name)
				assert.Equal(t, tt.wantName, *request.Name)
			}
		})
	}
}

func TestCreateSnapshotResponseJSON(t *testing.T) {
	controller, _, teardown := Setup(t)
	defer teardown()

	templateName := "snapshot-json-template"
	user := &models.CreatedTeamAPIKey{
		ID:   keys.AdminKeyID,
		Key:  InitKey,
		Name: "test-user",
	}

	origCreateSandboxTemplate := sandboxcr.DefaultCreateSandboxTemplate
	sandboxcr.DefaultCreateSandboxTemplate = func(ctx context.Context, c ctrlclient.Client, tmpl *v1alpha1.SandboxTemplate) (*v1alpha1.SandboxTemplate, error) {
		if tmpl.Name == "" && tmpl.GenerateName != "" {
			tmpl.Name = tmpl.GenerateName + rand.String(5)
		}
		return origCreateSandboxTemplate(ctx, c, tmpl)
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateSandboxTemplate = origCreateSandboxTemplate })

	origCreateCheckpoint := sandboxcr.DefaultCreateCheckpoint
	sandboxcr.DefaultCreateCheckpoint = func(ctx context.Context, c ctrlclient.Client, cp *v1alpha1.Checkpoint) (*v1alpha1.Checkpoint, error) {
		cp.Status = v1alpha1.CheckpointStatus{
			Phase:        v1alpha1.CheckpointSucceeded,
			CheckpointId: "snapshot-json-id",
		}
		created, err := origCreateCheckpoint(ctx, c, cp)
		if err != nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
		return created, nil
	}
	t.Cleanup(func() { sandboxcr.DefaultCreateCheckpoint = origCreateCheckpoint })

	cleanup := CreateSandboxPool(t, controller, templateName, 1)
	t.Cleanup(cleanup)

	require.Eventually(t, func() bool {
		list, err := controller.cache.ListSandboxesInPool(t.Context(), cache.ListSandboxesInPoolOptions{
			Pool: templateName,
		})
		return err == nil && len(list) == 1
	}, time.Second, 50*time.Millisecond)

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
	sandboxID := createResp.Body.SandboxID

	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+sandboxID+"/snapshots", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(models.HeaderApiKey, InitKey)
	rec := httptest.NewRecorder()
	controller.mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusCreated, rec.Code)
	assert.Contains(t, rec.Body.String(), `"names":[]`)
	assert.NotContains(t, rec.Body.String(), `"names":null`)
}
