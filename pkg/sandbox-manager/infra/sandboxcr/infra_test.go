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
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils/runtime"
	utils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
	stateutils "github.com/openkruise/agents/pkg/utils/sandboxutils"
	testutils "github.com/openkruise/agents/test/utils"
)

func createTestSandbox(name, user string, phase v1alpha1.SandboxPhase, ready bool) *v1alpha1.Sandbox {
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Annotations: map[string]string{
				v1alpha1.AnnotationOwner: user,
			},
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
		},
		Status: v1alpha1.SandboxStatus{
			Phase: phase,
		},
	}

	if ready {
		sbx.Status.Conditions = []metav1.Condition{
			{
				Type:   string(v1alpha1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
			},
		}
	}

	return sbx
}

//goland:noinspection GoDeprecation
func NewTestInfra(t *testing.T, opts ...config.SandboxManagerOptions) (*Infra, client.Client) {
	options := config.SandboxManagerOptions{}
	if len(opts) > 0 {
		options = opts[0]
	}
	options = config.InitOptions(options)
	cache, fc, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	infraInstance := NewInfraBuilder(options).
		WithCache(cache).
		WithAPIReader(fc).
		WithProxy(proxy.NewServer(options)).
		Build()
	if err := infraInstance.Run(t.Context()); err != nil {
		return nil, nil
	}
	return infraInstance.(*Infra), fc
}

func TestInfra_SelectSandboxes(t *testing.T) {
	utils.InitLogOutput()
	tests := []struct {
		name        string
		sandboxes   []*v1alpha1.Sandbox
		user        string
		expectNames []string
		expectCount int
	}{
		{
			name: "select all sandboxes for user",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-2", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-3", "user2", v1alpha1.SandboxRunning, true),
			},
			user:        "user1",
			expectNames: []string{"sandbox-1", "sandbox-2"},
		},
		{
			name: "select with no matching user",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-2", "user1", v1alpha1.SandboxRunning, true),
			},
			user:        "user2",
			expectNames: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.expectCount == 0 {
				tt.expectCount = len(tt.expectNames)
			}

			infraInstance, c := NewTestInfra(t)

			for _, sbx := range tt.sandboxes {
				CreateSandboxWithStatus(t, c, sbx)
			}
			time.Sleep(50 * time.Millisecond)

			// Test SelectSandboxes
			result, err := infraInstance.SelectSandboxes(t.Context(), infra.SelectSandboxesOptions{User: tt.user})
			assert.NoError(t, err)
			assert.Len(t, result, tt.expectCount)
			if len(tt.expectNames) > 0 {
				var gotNames []string
				for _, sandbox := range result {
					gotNames = append(gotNames, sandbox.GetName())
				}
				assert.ElementsMatch(t, tt.expectNames, gotNames)
			}
		})
	}
}

func TestInfra_SelectSandboxesWithOptions_NamespaceScoped(t *testing.T) {
	infraInstance, c := NewTestInfra(t)
	sandboxes := []*v1alpha1.Sandbox{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sandbox-a",
				Namespace:   "team-a",
				Annotations: map[string]string{v1alpha1.AnnotationOwner: "same-user"},
				Labels:      map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True},
			},
			Status: v1alpha1.SandboxStatus{
				Phase:      v1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				PodInfo:    v1alpha1.PodInfo{PodIP: "10.0.0.1"},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sandbox-b",
				Namespace:   "team-b",
				Annotations: map[string]string{v1alpha1.AnnotationOwner: "same-user"},
				Labels:      map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True},
			},
			Status: v1alpha1.SandboxStatus{
				Phase:      v1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
				PodInfo:    v1alpha1.PodInfo{PodIP: "10.0.0.2"},
			},
		},
	}
	for _, sbx := range sandboxes {
		CreateSandboxWithStatus(t, c, sbx)
	}

	result, err := infraInstance.SelectSandboxes(t.Context(), infra.SelectSandboxesOptions{
		Namespace: "team-a",
		User:      "same-user",
	})
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, "sandbox-a", result[0].GetName())
	assert.Equal(t, "team-a", result[0].GetNamespace())
}

func TestInfra_SelectSandboxesWithOptions_WithoutUserReturnsNamespaceScopedResults(t *testing.T) {
	infraInstance, c := NewTestInfra(t)
	for _, sbx := range []*v1alpha1.Sandbox{
		createTestSandbox("sandbox-a", "user-a", v1alpha1.SandboxRunning, true),
		createTestSandbox("sandbox-b", "user-b", v1alpha1.SandboxRunning, true),
	} {
		sbx.Namespace = "team-a"
		CreateSandboxWithStatus(t, c, sbx)
	}
	result, err := infraInstance.SelectSandboxes(t.Context(), infra.SelectSandboxesOptions{
		Namespace: "team-a",
	})
	require.NoError(t, err)
	require.Len(t, result, 2)
}

func TestInfra_GetSandbox(t *testing.T) {
	tests := []struct {
		name        string
		sandboxes   []*v1alpha1.Sandbox
		sandboxID   string
		expectError bool
		expectFound bool
	}{
		{
			name: "get existing sandbox",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
				createTestSandbox("sandbox-2", "user2", v1alpha1.SandboxRunning, true),
			},
			sandboxID:   "default--sandbox-1",
			expectError: false,
			expectFound: true,
		},
		{
			name: "get non-existing sandbox",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandbox("sandbox-1", "user1", v1alpha1.SandboxRunning, true),
			},
			sandboxID:   "non-existent",
			expectError: true,
			expectFound: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, fc := NewTestInfra(t)

			// Create sandboxes
			for _, sbx := range tt.sandboxes {
				CreateSandboxWithStatus(t, fc, sbx)
			}
			time.Sleep(100 * time.Millisecond)

			// Test GetClaimedSandbox
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			result, err := infraInstance.GetClaimedSandbox(ctx, infra.GetClaimedSandboxOptions{SandboxID: tt.sandboxID})
			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				assert.NotNil(t, result)
			}
		})
	}
}

func TestInfra_GetClaimedSandboxWithOptions_NamespaceScoped(t *testing.T) {
	infraInstance, fc := NewTestInfra(t)
	sbx := &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "sandbox-a",
			Namespace:   "team-a",
			Annotations: map[string]string{v1alpha1.AnnotationOwner: "same-user"},
			Labels:      map[string]string{v1alpha1.LabelSandboxIsClaimed: v1alpha1.True},
		},
		Status: v1alpha1.SandboxStatus{
			Phase:      v1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{{Type: string(v1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			PodInfo:    v1alpha1.PodInfo{PodIP: "10.0.0.1"},
		},
	}
	CreateSandboxWithStatus(t, fc, sbx)
	sandboxID := stateutils.GetSandboxID(sbx)

	got, err := infraInstance.GetClaimedSandbox(t.Context(), infra.GetClaimedSandboxOptions{
		Namespace: "team-a",
		SandboxID: sandboxID,
	})
	require.NoError(t, err)
	assert.Equal(t, "team-a", got.GetNamespace())
	assert.Equal(t, "sandbox-a", got.GetName())

	getCtx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err = infraInstance.GetClaimedSandbox(getCtx, infra.GetClaimedSandboxOptions{
		Namespace: "team-b",
		SandboxID: sandboxID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestInfra_DeleteCheckpointWithOptions_NamespaceScoped(t *testing.T) {
	infraInstance, fc := NewTestInfra(t)
	for _, namespace := range []string{"team-a", "team-b"} {
		tmpl := &v1alpha1.SandboxTemplate{
			ObjectMeta: metav1.ObjectMeta{Name: "checkpoint-template", Namespace: namespace},
		}
		require.NoError(t, fc.Create(t.Context(), tmpl))
		cp := &v1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "checkpoint-template",
				Namespace:   namespace,
				Annotations: map[string]string{v1alpha1.AnnotationOwner: "test-user"},
			},
			Status: v1alpha1.CheckpointStatus{CheckpointId: "shared-checkpoint-id"},
		}
		require.NoError(t, fc.Create(t.Context(), cp))
		require.NoError(t, fc.Status().Update(t.Context(), cp))
	}

	err := infraInstance.DeleteCheckpoint(t.Context(), infra.DeleteCheckpointOptions{
		Namespace:    "team-a",
		CheckpointID: "shared-checkpoint-id",
	})
	require.NoError(t, err)

	err = fc.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "checkpoint-template"}, &v1alpha1.SandboxTemplate{})
	require.Error(t, err)
	err = fc.Get(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "checkpoint-template"}, &v1alpha1.Checkpoint{})
	require.Error(t, err)

	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "team-b", Name: "checkpoint-template"}, &v1alpha1.SandboxTemplate{}))
	require.NoError(t, fc.Get(t.Context(), types.NamespacedName{Namespace: "team-b", Name: "checkpoint-template"}, &v1alpha1.Checkpoint{}))
}

func createTestSandboxWithDefaults(name string, namespace string) *v1alpha1.Sandbox {
	return &v1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				v1alpha1.LabelSandboxIsClaimed: "true",
			},
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
	}
}

func TestInfra_reconcileSandbox(t *testing.T) {
	tests := []struct {
		name               string
		sandbox            *v1alpha1.Sandbox
		notFound           bool
		addRouteFirst      bool
		initialRoute       *proxy.Route // initial route state for update tests
		expectRouteExist   bool
		expectRouteState   string // expected route state
		expectRouteIP      string // expected route IP
		expectRouteChanged bool   // whether route should be changed
	}{
		{
			name:          "reconcile sandbox not found - should delete route",
			sandbox:       createTestSandboxWithDefaults("test-sandbox", "default"),
			notFound:      true,
			addRouteFirst: true,
		},
		{
			name:             "reconcile sandbox exists - should create route",
			sandbox:          createTestSandboxWithDefaults("test-sandbox", "default"),
			notFound:         false,
			expectRouteExist: true,
			expectRouteState: v1alpha1.SandboxStateRunning,
			expectRouteIP:    "10.0.0.1",
		},
		{
			name: "reconcile sandbox with changed state - should update route",
			sandbox: func() *v1alpha1.Sandbox {
				sbx := createTestSandboxWithDefaults("test-sandbox", "default")
				sbx.Status.Phase = v1alpha1.SandboxPaused
				return sbx
			}(),
			notFound:           false,
			expectRouteExist:   true,
			expectRouteState:   v1alpha1.SandboxStatePaused,
			expectRouteIP:      "10.0.0.1",
			expectRouteChanged: true,
			initialRoute: &proxy.Route{
				ID:    "default--test-sandbox",
				IP:    "10.0.0.1",
				State: v1alpha1.SandboxStateRunning,
			},
		},
		{
			name: "reconcile sandbox with unchanged state - should not update route",
			sandbox: func() *v1alpha1.Sandbox {
				sbx := createTestSandboxWithDefaults("test-sandbox", "default")
				sbx.Status.Phase = v1alpha1.SandboxRunning
				return sbx
			}(),
			notFound:           false,
			expectRouteExist:   true,
			expectRouteState:   v1alpha1.SandboxStateRunning,
			expectRouteIP:      "10.0.0.1",
			expectRouteChanged: false,
			initialRoute: &proxy.Route{
				ID:    "default--test-sandbox",
				IP:    "10.0.0.1",
				State: v1alpha1.SandboxStateRunning,
			},
		},
		{
			name: "reconcile sandbox with changed IP - should update route",
			sandbox: func() *v1alpha1.Sandbox {
				sbx := createTestSandboxWithDefaults("test-sandbox", "default")
				sbx.Status.PodInfo.PodIP = "10.0.0.2"
				return sbx
			}(),
			notFound:           false,
			expectRouteExist:   true,
			expectRouteState:   v1alpha1.SandboxStateRunning,
			expectRouteIP:      "10.0.0.2",
			expectRouteChanged: true,
			initialRoute: &proxy.Route{
				ID:    "default--test-sandbox",
				IP:    "10.0.0.1",
				State: v1alpha1.SandboxStateRunning,
			},
		},
		{
			name: "reconcile sandbox with existing route and no changes - route remains unchanged",
			sandbox: func() *v1alpha1.Sandbox {
				sbx := createTestSandboxWithDefaults("test-sandbox", "default")
				sbx.Status.Phase = v1alpha1.SandboxRunning
				sbx.Status.PodInfo.PodIP = "10.0.0.1"
				return sbx
			}(),
			notFound:           false,
			expectRouteExist:   true,
			expectRouteState:   v1alpha1.SandboxStateRunning,
			expectRouteIP:      "10.0.0.1",
			expectRouteChanged: false,
			initialRoute: &proxy.Route{
				ID:    "default--test-sandbox",
				IP:    "10.0.0.1",
				State: v1alpha1.SandboxStateRunning,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, _ := NewTestInfra(t, config.SandboxManagerOptions{
				DisableRouteReconciliation: true,
			})

			if tt.addRouteFirst {
				// Add route first for notFound case
				id := stateutils.GetSandboxID(tt.sandbox)
				infraInstance.Proxy.SetRoute(t.Context(), proxy.Route{
					ID:    id,
					IP:    tt.sandbox.Status.PodInfo.PodIP,
					State: v1alpha1.SandboxStateRunning,
				})
			}

			// Set initial route for tests that need pre-existing route state
			if tt.initialRoute != nil {
				infraInstance.Proxy.SetRoute(t.Context(), *tt.initialRoute)
			}

			// Call reconcileSandbox
			_, err := infraInstance.reconcileSandbox(t.Context(), tt.sandbox, tt.notFound)
			assert.NoError(t, err)

			// Check route
			id := stateutils.GetSandboxID(tt.sandbox)
			route, ok := infraInstance.Proxy.LoadRoute(id)
			require.Equal(t, tt.expectRouteExist, ok, "expect route exist %v, got %v", tt.expectRouteExist, ok)
			if tt.expectRouteExist {
				assert.Equal(t, id, route.ID, "expect route ID %v, got %v", id, route.ID)
				if tt.expectRouteIP != "" {
					assert.Equal(t, tt.expectRouteIP, route.IP, "expect route IP %v, got %v", tt.expectRouteIP, route.IP)
				} else {
					assert.Equal(t, tt.sandbox.Status.PodInfo.PodIP, route.IP, "expect route IP %v, got %v", tt.sandbox.Status.PodInfo.PodIP, route.IP)
				}
				if tt.expectRouteState != "" {
					assert.Equal(t, tt.expectRouteState, route.State, "expect route state %v, got %v", tt.expectRouteState, route.State)
				}
			}
		})
	}
}

func TestInfra_reconcileRoutes(t *testing.T) {
	tests := []struct {
		name               string
		sandboxes          []*v1alpha1.Sandbox
		orphanedRoutes     []proxy.Route
		expectDeletedCount int
		expectRemainingIDs []string
	}{
		{
			name: "no orphaned routes",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandboxWithDefaults("sandbox-1", "default"),
				createTestSandboxWithDefaults("sandbox-2", "default"),
			},
			orphanedRoutes:     []proxy.Route{},
			expectDeletedCount: 0,
			expectRemainingIDs: []string{"default--sandbox-1", "default--sandbox-2"},
		},
		{
			name: "one orphaned route",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandboxWithDefaults("sandbox-1", "default"),
			},
			orphanedRoutes: []proxy.Route{
				{ID: "default--orphaned-sandbox", IP: "10.0.0.99", State: v1alpha1.SandboxStateRunning},
			},
			expectDeletedCount: 1,
			expectRemainingIDs: []string{"default--sandbox-1"},
		},
		{
			name: "multiple orphaned routes",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandboxWithDefaults("sandbox-1", "default"),
			},
			orphanedRoutes: []proxy.Route{
				{ID: "default--orphaned-1", IP: "10.0.0.98", State: v1alpha1.SandboxStateRunning},
				{ID: "default--orphaned-2", IP: "10.0.0.99", State: v1alpha1.SandboxStateRunning},
			},
			expectDeletedCount: 2,
			expectRemainingIDs: []string{"default--sandbox-1"},
		},
		{
			name:      "all routes are orphaned",
			sandboxes: []*v1alpha1.Sandbox{},
			orphanedRoutes: []proxy.Route{
				{ID: "default--orphaned-1", IP: "10.0.0.98", State: v1alpha1.SandboxStateRunning},
				{ID: "default--orphaned-2", IP: "10.0.0.99", State: v1alpha1.SandboxStateRunning},
			},
			expectDeletedCount: 2,
			expectRemainingIDs: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, c := NewTestInfra(t)

			// Create sandboxes in cache
			for _, sbx := range tt.sandboxes {
				CreateSandboxWithStatus(t, c, sbx)
				// Also add their routes to proxy
				id := stateutils.GetSandboxID(sbx)
				infraInstance.Proxy.SetRoute(t.Context(), proxy.Route{
					ID:    id,
					IP:    sbx.Status.PodInfo.PodIP,
					State: v1alpha1.SandboxStateRunning,
				})
			}

			// Add orphaned routes to proxy
			for _, route := range tt.orphanedRoutes {
				infraInstance.Proxy.SetRoute(t.Context(), route)
			}

			time.Sleep(50 * time.Millisecond)

			// Run reconciliation
			infraInstance.reconcileRoutes(t.Context())

			// Verify remaining routes
			remainingRoutes := infraInstance.Proxy.ListRoutes()
			assert.Len(t, remainingRoutes, len(tt.expectRemainingIDs), "expected %d routes remaining", len(tt.expectRemainingIDs))

			// Verify orphaned routes are deleted
			for _, route := range tt.orphanedRoutes {
				_, ok := infraInstance.Proxy.LoadRoute(route.ID)
				assert.False(t, ok, "orphaned route %s should be deleted", route.ID)
			}

			// Verify expected routes still exist
			for _, expectedID := range tt.expectRemainingIDs {
				_, ok := infraInstance.Proxy.LoadRoute(expectedID)
				assert.True(t, ok, "route %s should still exist", expectedID)
			}
		})
	}
}

func TestInfra_CloneSandbox(t *testing.T) {
	utils.InitLogOutput()
	runtimeOpts := testutils.TestRuntimeServerOptions{
		RunCommandResult: runtime.RunCommandResult{
			PID:    1,
			Exited: true,
		},
		RunCommandImmediately: true,
	}
	server := testutils.NewTestRuntimeServer(runtimeOpts)
	defer server.Close()

	infraInstance, fc := NewTestInfra(t)

	checkpointID := "test-checkpoint-123"
	user := "test-user"

	// Define context key types for sandbox override
	type infraSbxOverrideKey struct{}
	type infraSbxOverride struct {
		Name       string
		RuntimeURL string
	}

	// Decorator: DefaultCreateSandbox - set sandbox ready after creation
	origCreateSandbox := DefaultCreateSandbox
	DefaultCreateSandbox = func(ctx context.Context, sbx *v1alpha1.Sandbox, c client.Client) (*v1alpha1.Sandbox, error) {
		if override, ok := ctx.Value(infraSbxOverrideKey{}).(infraSbxOverride); ok {
			if override.Name != "" {
				sbx.Name = override.Name
			}
			if override.RuntimeURL != "" {
				if sbx.Annotations == nil {
					sbx.Annotations = map[string]string{}
				}
				sbx.Annotations[v1alpha1.AnnotationRuntimeURL] = override.RuntimeURL
			}
		}
		created, err := origCreateSandbox(ctx, sbx, c)
		if err != nil {
			return nil, err
		}
		// Update Sandbox status to Ready
		created.Status = v1alpha1.SandboxStatus{
			Phase:              v1alpha1.SandboxRunning,
			ObservedGeneration: created.Generation,
			Conditions: []metav1.Condition{
				{
					Type:   string(v1alpha1.SandboxConditionReady),
					Status: metav1.ConditionTrue,
					Reason: v1alpha1.SandboxReadyReasonPodReady,
				},
			},
			PodInfo: v1alpha1.PodInfo{
				PodIP: "1.2.3.4",
			},
		}
		err = c.Status().Update(ctx, created)
		if err != nil {
			return nil, err
		}
		return created, nil
	}
	t.Cleanup(func() { DefaultCreateSandbox = origCreateSandbox })

	// Create SandboxTemplate with same name as checkpoint
	sbt := &v1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointID, // Same name as checkpoint
			Namespace: "default",
		},
		Spec: v1alpha1.SandboxTemplateSpec{
			Template: &corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "main",
							Image: "test-image",
						},
					},
				},
			},
		},
	}
	err := fc.Create(t.Context(), sbt)
	require.NoError(t, err)

	// Create Checkpoint with same name as SandboxTemplate
	cp := &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      checkpointID,
			Namespace: "default",
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

	// Wait for checkpoint to be cached
	require.Eventually(t, func() bool {
		_, err := infraInstance.Cache.GetCheckpoint(t.Context(), infracache.GetCheckpointOptions{CheckpointID: checkpointID})
		return err == nil
	}, time.Second, 10*time.Millisecond)

	// Build context with sbxOverride
	ctx := context.WithValue(t.Context(), infraSbxOverrideKey{}, infraSbxOverride{
		Name:       "test-sandbox-clone-infra",
		RuntimeURL: server.URL,
	})

	// Call CloneSandbox
	opts := infra.CloneSandboxOptions{
		User:             user,
		CheckPointID:     checkpointID,
		WaitReadyTimeout: 30 * time.Second,
	}
	sbx, metrics, err := infraInstance.CloneSandbox(ctx, opts)

	// Verify results
	require.NoError(t, err)
	require.NotNil(t, sbx)
	assert.Equal(t, user, sbx.GetAnnotations()[v1alpha1.AnnotationOwner])
	assert.Equal(t, checkpointID, sbx.GetLabels()[v1alpha1.LabelSandboxTemplate])
	assert.Equal(t, "true", sbx.GetLabels()[v1alpha1.LabelSandboxIsClaimed])
	assert.NotEmpty(t, sbx.GetAnnotations()[v1alpha1.AnnotationClaimTime])

	// Verify metrics are recorded
	assert.GreaterOrEqual(t, metrics.GetTemplate, time.Duration(0))
	assert.GreaterOrEqual(t, metrics.CreateSandbox, time.Duration(0))
	assert.GreaterOrEqual(t, metrics.WaitReady, time.Duration(0))
	assert.GreaterOrEqual(t, metrics.Total, time.Duration(0))
}

func createTestCheckpoint(name, user string, namespace string, phase v1alpha1.CheckpointPhase) *v1alpha1.Checkpoint {
	return &v1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			UID:       types.UID(uuid.NewString()),
			Annotations: map[string]string{
				v1alpha1.AnnotationOwner: user,
			},
		},
		Status: v1alpha1.CheckpointStatus{
			Phase:        phase,
			CheckpointId: name + "-id",
		},
	}
}

func CreateCheckpointWithStatus(t *testing.T, c client.Client, cp *v1alpha1.Checkpoint) {
	require.NoError(t, c.Create(t.Context(), cp))
	// Update status
	require.NoError(t, c.Update(t.Context(), cp))
}

func EnsureCheckpointInCache(t *testing.T, cache infracache.Provider, cp *v1alpha1.Checkpoint) {
	require.Eventually(t, func() bool {
		_, err := cache.GetCheckpoint(t.Context(), infracache.GetCheckpointOptions{CheckpointID: cp.Status.CheckpointId})
		return err == nil
	}, time.Second, 10*time.Millisecond, "get checkpoint from cache timeout")
}

func TestInfra_SelectSucceededCheckpoints(t *testing.T) {
	utils.InitLogOutput()

	tests := []struct {
		name                string
		checkpoints         []*v1alpha1.Checkpoint
		user                string
		expectCheckpointIDs []string
	}{
		{
			name: "only return succeeded checkpoints for user",
			checkpoints: []*v1alpha1.Checkpoint{
				createTestCheckpoint("cp-succeeded-1", "user1", "default", v1alpha1.CheckpointSucceeded),
				createTestCheckpoint("cp-succeeded-2", "user1", "default", v1alpha1.CheckpointSucceeded),
				createTestCheckpoint("cp-pending", "user1", "default", v1alpha1.CheckpointPending),
				createTestCheckpoint("cp-failed", "user1", "default", v1alpha1.CheckpointFailed),
				createTestCheckpoint("cp-creating", "user1", "default", v1alpha1.CheckpointCreating),
			},
			user:                "user1",
			expectCheckpointIDs: []string{"cp-succeeded-1-id", "cp-succeeded-2-id"},
		},
		{
			name: "return empty list when user has no succeeded checkpoints",
			checkpoints: []*v1alpha1.Checkpoint{
				createTestCheckpoint("cp-pending", "user1", "default", v1alpha1.CheckpointPending),
				createTestCheckpoint("cp-failed", "user1", "default", v1alpha1.CheckpointFailed),
				createTestCheckpoint("cp-creating", "user1", "default", v1alpha1.CheckpointCreating),
			},
			user:                "user1",
			expectCheckpointIDs: []string{},
		},
		{
			name:                "return empty list when user has no checkpoints",
			checkpoints:         []*v1alpha1.Checkpoint{},
			user:                "user1",
			expectCheckpointIDs: []string{},
		},
		{
			name: "filter checkpoints by user",
			checkpoints: []*v1alpha1.Checkpoint{
				createTestCheckpoint("cp-user1-succeeded", "user1", "default", v1alpha1.CheckpointSucceeded),
				createTestCheckpoint("cp-user2-succeeded", "user2", "default", v1alpha1.CheckpointSucceeded),
				createTestCheckpoint("cp-user3-succeeded", "user3", "default", v1alpha1.CheckpointSucceeded),
			},
			user:                "user1",
			expectCheckpointIDs: []string{"cp-user1-succeeded-id"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, fc := NewTestInfra(t)

			// Create checkpoints
			for _, cp := range tt.checkpoints {
				CreateCheckpointWithStatus(t, fc, cp)
			}

			// Wait for all checkpoints to be cached
			for _, cp := range tt.checkpoints {
				if cp.Status.CheckpointId != "" {
					EnsureCheckpointInCache(t, infraInstance.Cache, cp)
				}
			}

			// Test SelectSucceededCheckpoints
			results, err := infraInstance.SelectSucceededCheckpoints(t.Context(), infra.SelectSucceededCheckpointsOptions{User: tt.user})
			assert.NoError(t, err)
			assert.Len(t, results, len(tt.expectCheckpointIDs))

			// Verify the returned checkpoint IDs
			var gotIDs []string
			for _, result := range results {
				gotIDs = append(gotIDs, result.CheckpointID)
			}
			assert.ElementsMatch(t, tt.expectCheckpointIDs, gotIDs)
		})
	}
}

func TestInfra_startRouteReconciler(t *testing.T) {
	tests := []struct {
		name              string
		sandboxes         []*v1alpha1.Sandbox
		orphanedRoutes    []proxy.Route
		reconcileInterval time.Duration
		waitTime          time.Duration
		expectReconciled  bool
	}{
		{
			name: "reconciler cleans up orphaned routes periodically",
			sandboxes: []*v1alpha1.Sandbox{
				createTestSandboxWithDefaults("sandbox-1", "default"),
			},
			orphanedRoutes: []proxy.Route{
				{ID: "default--orphaned-sandbox", IP: "10.0.0.99", State: v1alpha1.SandboxStateRunning},
			},
			reconcileInterval: 100 * time.Millisecond,
			waitTime:          200 * time.Millisecond,
			expectReconciled:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			infraInstance, fc := NewTestInfra(t)

			// Create sandboxes
			var createdSandboxes []string
			for _, sbx := range tt.sandboxes {
				CreateSandboxWithStatus(t, fc, sbx)
				id := stateutils.GetSandboxID(sbx)
				infraInstance.Proxy.SetRoute(t.Context(), proxy.Route{
					ID:    id,
					IP:    sbx.Status.PodInfo.PodIP,
					State: v1alpha1.SandboxStateRunning,
				})
				createdSandboxes = append(createdSandboxes, id)
			}

			require.Eventually(t, func() bool {
				for _, id := range createdSandboxes {
					_, err := infraInstance.Cache.GetClaimedSandbox(t.Context(), infracache.GetClaimedSandboxOptions{SandboxID: id})
					if err != nil {
						return false
					}
				}
				return true
			}, time.Second, 10*time.Millisecond)

			// Add orphaned routes
			for _, route := range tt.orphanedRoutes {
				infraInstance.Proxy.SetRoute(t.Context(), route)
			}

			time.Sleep(50 * time.Millisecond)

			go infraInstance.startRouteReconciler(tt.reconcileInterval)

			// Wait for reconciliation to happen (or not)
			time.Sleep(tt.waitTime)

			// Stop the reconciler
			infraInstance.Stop(t.Context())

			// Verify orphaned routes are cleaned up (or not)
			for _, route := range tt.orphanedRoutes {
				_, ok := infraInstance.Proxy.LoadRoute(route.ID)
				if tt.expectReconciled {
					assert.False(t, ok, "orphaned route %s should be deleted after reconciliation", route.ID)
				} else {
					assert.True(t, ok, "orphaned route %s should still exist (reconciler stopped early)", route.ID)
				}
			}

			// Verify valid routes still exist
			for _, sbx := range tt.sandboxes {
				id := stateutils.GetSandboxID(sbx)
				_, ok := infraInstance.Proxy.LoadRoute(id)
				assert.True(t, ok, "valid route %s should always exist", id)
			}
		})
	}
}
