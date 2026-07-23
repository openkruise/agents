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

package controller

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-gateway/jwtauth"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
)

func TestAddJWTAuthManager(t *testing.T) {
	reader := fake.NewClientBuilder().Build()
	tests := []struct {
		name        string
		manager     func() *jwtauth.Manager
		addError    error
		expectAdded bool
		expectError string
	}{
		{name: "nil manager"},
		{
			name:        "manager added",
			manager:     jwtauth.NewManager,
			expectAdded: true,
		},
		{
			name: "different reader already configured",
			manager: func() *jwtauth.Manager {
				jwtManager := jwtauth.NewManager()
				require.NoError(t, jwtManager.SetReader(fake.NewClientBuilder().Build()))
				return jwtManager
			},
			expectError: "reader is already set",
		},
		{
			name:        "add failure",
			manager:     jwtauth.NewManager,
			addError:    errors.New("add failed"),
			expectAdded: true,
			expectError: "unable to add JWT authentication manager",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var jwtManager *jwtauth.Manager
			if tt.manager != nil {
				jwtManager = tt.manager()
			}
			added := false
			err := addJWTAuthManager(reader, func(runnable manager.Runnable) error {
				assert.Same(t, jwtManager, runnable)
				added = true
				return tt.addError
			}, jwtManager)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tt.expectAdded, added)
		})
	}
}

func TestRouteEventHandlerObjectLifecycle(t *testing.T) {
	ctx := context.Background()
	routeRegistry := newTestRegistry(t)
	handler := &routeEventHandler{
		registry: routeRegistry,
		policy: SandboxPolicy{
			Selector: labels.SelectorFromSet(labels.Set{"route": "true"}),
			Include:  func(*agentsv1alpha1.Sandbox, string) bool { return true },
		},
	}

	t.Run("unseen excluded object does not create fence", func(t *testing.T) {
		sandbox := testSandbox("ns", "unseen", "uid", "5", "")
		handler.onObject(ctx, sandbox)

		sandbox.Labels["route"] = "true"
		handler.onObject(ctx, sandbox)
		_, found := routeRegistry.Get("ns--unseen")
		assert.True(t, found, "same-RV upsert proves exclusion did not create a fence")
	})

	t.Run("tracked exclusion creates fence and newer inclusion crosses it", func(t *testing.T) {
		sandbox := testSandbox("ns", "tracked", "uid", "10", "")
		sandbox.Labels["route"] = "true"
		handler.onObject(ctx, sandbox)
		_, found := routeRegistry.Get("ns--tracked")
		require.True(t, found)

		sandbox = sandbox.DeepCopy()
		sandbox.ResourceVersion = "11"
		delete(sandbox.Labels, "route")
		handler.onObject(ctx, sandbox)
		_, found = routeRegistry.Get("ns--tracked")
		assert.False(t, found)

		sandbox.Labels["route"] = "true"
		handler.onObject(ctx, sandbox)
		_, found = routeRegistry.Get("ns--tracked")
		assert.False(t, found, "equal-RV upsert remains behind the exclusion fence")

		sandbox.ResourceVersion = "12"
		handler.onObject(ctx, sandbox)
		_, found = routeRegistry.Get("ns--tracked")
		assert.True(t, found)
	})
}

func TestRouteEventHandlerPreservesTrafficAuth(t *testing.T) {
	ctx := context.Background()
	routeRegistry := newTestRegistry(t)
	handler := &routeEventHandler{
		registry: routeRegistry,
		policy: SandboxPolicy{
			Selector: labels.Everything(),
			Include:  func(*agentsv1alpha1.Sandbox, string) bool { return true },
		},
	}
	sandbox := testSandbox("ns", "auth", "uid-auth", "1", "short-auth")
	sandbox.Annotations[identity.AnnotationEnableJwtAuth] = agentsv1alpha1.True

	handler.onObject(ctx, sandbox)

	route, found := routeRegistry.Get("short-auth")
	require.True(t, found)
	assert.True(t, route.RequireTrafficAuth)
}

func TestRouteEventHandlerDeletionSignals(t *testing.T) {
	ctx := context.Background()
	routeRegistry := newTestRegistry(t)
	handler := &routeEventHandler{
		registry: routeRegistry,
		policy: SandboxPolicy{
			Selector: labels.Everything(),
			Include:  func(*agentsv1alpha1.Sandbox, string) bool { return true },
		},
	}

	sandbox := testSandbox("ns", "sandbox", "uid", "20", "")
	handler.onObject(ctx, sandbox)
	_, found := routeRegistry.Get("ns--sandbox")
	require.True(t, found)

	terminating := sandbox.DeepCopy()
	terminating.ResourceVersion = "21"
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	handler.onObject(ctx, terminating)
	_, found = routeRegistry.Get("ns--sandbox")
	assert.False(t, found)

	finalizerUpdate := terminating.DeepCopy()
	finalizerUpdate.ResourceVersion = "22"
	handler.onObject(ctx, finalizerUpdate)
	handler.onDelete(ctx, finalizerUpdate.DeepCopy())

	equal := sandbox.DeepCopy()
	equal.ResourceVersion = "22"
	handler.onObject(ctx, equal)
	_, found = routeRegistry.Get("ns--sandbox")
	assert.False(t, found)

	handler.onDelete(ctx, toolscache.DeletedFinalStateUnknown{
		Key: "ns/sandbox",
	})
	newer := sandbox.DeepCopy()
	newer.ResourceVersion = "23"
	handler.onObject(ctx, newer)
	_, found = routeRegistry.Get("ns--sandbox")
	assert.True(t, found)
}

func TestRouteEventHandlerTombstoneUsesCurrentRecordRV(t *testing.T) {
	tests := []struct {
		name       string
		currentRV  string
		embeddedRV string
		newerRV    string
	}{
		{
			name:      "key only",
			currentRV: "30",
		},
		{
			name:       "stale embedded object",
			currentRV:  "42",
			embeddedRV: "41",
			newerRV:    "43",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			routeRegistry := newTestRegistry(t)
			handler := &routeEventHandler{
				registry: routeRegistry,
				policy: SandboxPolicy{
					Selector: labels.Everything(),
					Include:  func(*agentsv1alpha1.Sandbox, string) bool { return true },
				},
			}
			current := testSandbox("ns", "sandbox", "uid", tt.currentRV, "")
			handler.onObject(ctx, current)

			tombstone := toolscache.DeletedFinalStateUnknown{Key: "ns/sandbox"}
			if tt.embeddedRV != "" {
				embedded := current.DeepCopy()
				embedded.ResourceVersion = tt.embeddedRV
				tombstone.Obj = embedded
			}
			handler.onDelete(ctx, tombstone)

			_, found := routeRegistry.Get("ns--sandbox")
			assert.False(t, found, "the tombstone must remove the current record")

			handler.onObject(ctx, current.DeepCopy())
			_, found = routeRegistry.Get("ns--sandbox")
			assert.False(t, found, "the removed record RV becomes the fence")

			if tt.newerRV != "" {
				newer := current.DeepCopy()
				newer.ResourceVersion = tt.newerRV
				handler.onObject(ctx, newer)
				_, found = routeRegistry.Get("ns--sandbox")
				assert.True(t, found)
			}
		})
	}
}

func TestNewSandboxPolicy(t *testing.T) {
	policy, err := NewSandboxPolicy("ns", "included=true", nil)
	require.NoError(t, err)
	assert.True(t, policy.Selector.Matches(labels.Set{"included": "true"}))
	assert.False(t, policy.Selector.Matches(labels.Set{"included": "false"}))
	assert.True(t, policy.Include(testSandbox("ns", "a", "uid", "1", ""), "state"))

	_, err = NewSandboxPolicy("", "bad in (", nil)
	require.Error(t, err)
}

func TestStartManagerDependencyValidation(t *testing.T) {
	require.Error(t, StartManager(context.Background(), ManagerOptions{}))

	routeRegistry := newTestRegistry(t)
	err := StartManager(context.Background(), ManagerOptions{
		Registry:      routeRegistry,
		LabelSelector: "bad in (",
	})
	require.Error(t, err)
	assert.False(t, routeRegistry.Ready())
}

func newTestRegistry(t *testing.T) *registry.Registry {
	t.Helper()
	return registry.NewRegistry()
}

func testSandbox(namespace, name, uid, resourceVersion, id string) *agentsv1alpha1.Sandbox {
	objectLabels := map[string]string{}
	if id != "" {
		objectLabels[agentsv1alpha1.LabelSandboxID] = id
	}
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       namespace,
			Name:            name,
			UID:             types.UID(uid),
			ResourceVersion: resourceVersion,
			Labels:          objectLabels,
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner:              "owner",
				agentsv1alpha1.AnnotationRuntimeAccessToken: "token",
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			PodInfo: agentsv1alpha1.PodInfo{
				PodIP: "10.0.0.1",
			},
			Conditions: []metav1.Condition{{
				Type:   string(agentsv1alpha1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
			}},
		},
	}
}
