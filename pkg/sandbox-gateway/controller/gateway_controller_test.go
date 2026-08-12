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
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
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
	handler, routeRegistry := newHandler()

	sandbox := testSandbox("ns", "sandbox", "uid", "10", "")
	handler.onObject(ctx, sandbox)
	route, found := routeRegistry.Get("ns--sandbox")
	require.True(t, found)
	assert.Equal(t, "10", route.ResourceVersion)

	updated := sandbox.DeepCopy()
	updated.ResourceVersion = "11"
	updated.Status.Phase = agentsv1alpha1.SandboxPaused
	handler.onObject(ctx, updated)
	route, found = routeRegistry.Get("ns--sandbox")
	require.True(t, found)
	assert.Equal(t, "11", route.ResourceVersion)
	assert.NotEqual(t, string(agentsv1alpha1.SandboxRunning), route.State)

	authSandbox := testSandbox("ns", "auth", "uid-auth", "1", "short-auth")
	authSandbox.Annotations[identity.AnnotationEnableJwtAuth] = agentsv1alpha1.True
	handler.onObject(ctx, authSandbox)
	route, found = routeRegistry.Get("short-auth")
	require.True(t, found)
	assert.True(t, route.RequireTrafficAuth)
}

func TestRouteEventHandlerDeletionSignals(t *testing.T) {
	tests := []struct {
		name      string
		currentRV string
		signal    func(handler *routeEventHandler, ctx context.Context, current *agentsv1alpha1.Sandbox)
		// replayRV re-observes the object at the expected fence RV and must stay fenced.
		replayRV string
		newerRV  string
	}{
		{
			name:      "terminating update fences its resource version",
			currentRV: "20",
			signal: func(handler *routeEventHandler, ctx context.Context, current *agentsv1alpha1.Sandbox) {
				terminating := current.DeepCopy()
				terminating.ResourceVersion = "21"
				now := metav1.Now()
				terminating.DeletionTimestamp = &now
				handler.onObject(ctx, terminating)
			},
			replayRV: "21",
			newerRV:  "22",
		},
		{
			name:      "typed delete object fences its resource version",
			currentRV: "20",
			signal: func(handler *routeEventHandler, ctx context.Context, current *agentsv1alpha1.Sandbox) {
				deleted := current.DeepCopy()
				deleted.ResourceVersion = "21"
				handler.onDelete(ctx, deleted)
			},
			replayRV: "21",
			newerRV:  "22",
		},
		{
			name:      "key-only tombstone fences the current record RV",
			currentRV: "30",
			signal: func(handler *routeEventHandler, ctx context.Context, _ *agentsv1alpha1.Sandbox) {
				handler.onDelete(ctx, toolscache.DeletedFinalStateUnknown{Key: "ns/sandbox"})
			},
			replayRV: "30",
			newerRV:  "31",
		},
		{
			name:      "stale embedded tombstone still fences the current record RV",
			currentRV: "42",
			signal: func(handler *routeEventHandler, ctx context.Context, current *agentsv1alpha1.Sandbox) {
				embedded := current.DeepCopy()
				embedded.ResourceVersion = "41"
				handler.onDelete(ctx, toolscache.DeletedFinalStateUnknown{
					Key: "ns/sandbox",
					Obj: embedded,
				})
			},
			replayRV: "42",
			newerRV:  "43",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			handler, routeRegistry := newHandler()
			current := testSandbox("ns", "sandbox", "uid", tt.currentRV, "")
			handler.onObject(ctx, current)
			_, found := routeRegistry.Get("ns--sandbox")
			require.True(t, found)

			tt.signal(handler, ctx, current)
			_, found = routeRegistry.Get("ns--sandbox")
			assert.False(t, found, "the deletion signal must remove the current record")

			replay := current.DeepCopy()
			replay.ResourceVersion = tt.replayRV
			handler.onObject(ctx, replay)
			_, found = routeRegistry.Get("ns--sandbox")
			assert.False(t, found, "an equal replay must stay fenced")

			newer := current.DeepCopy()
			newer.ResourceVersion = tt.newerRV
			handler.onObject(ctx, newer)
			_, found = routeRegistry.Get("ns--sandbox")
			assert.True(t, found, "a newer observation must cross the fence")
		})
	}
}

func TestRouteEventHandlerDiscardsInvalidEvents(t *testing.T) {
	tests := []struct {
		name  string
		event func(handler *routeEventHandler, ctx context.Context)
	}{
		{
			name: "onObject discards non-sandbox object",
			event: func(handler *routeEventHandler, ctx context.Context) {
				handler.onObject(ctx, "not-a-sandbox")
			},
		},
		{
			name: "onObject discards sandbox that fails projection",
			event: func(handler *routeEventHandler, ctx context.Context) {
				invalid := testSandbox("ns", "sandbox", "", "11", "")
				handler.onObject(ctx, invalid)
			},
		},
		{
			name: "onDelete discards tombstone with malformed key",
			event: func(handler *routeEventHandler, ctx context.Context) {
				handler.onDelete(ctx, toolscache.DeletedFinalStateUnknown{Key: "ns/sandbox/extra"})
			},
		},
		{
			name: "onDelete discards tombstone key without namespace",
			event: func(handler *routeEventHandler, ctx context.Context) {
				handler.onDelete(ctx, toolscache.DeletedFinalStateUnknown{Key: "sandbox"})
			},
		},
		{
			name: "onDelete discards unknown object type",
			event: func(handler *routeEventHandler, ctx context.Context) {
				handler.onDelete(ctx, "not-a-sandbox")
			},
		},
		{
			name: "terminating sandbox with malformed resource version is rejected",
			event: func(handler *routeEventHandler, ctx context.Context) {
				terminating := testSandbox("ns", "sandbox", "uid", "not-a-number", "")
				now := metav1.Now()
				terminating.DeletionTimestamp = &now
				handler.onObject(ctx, terminating)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			handler, routeRegistry := newHandler()
			handler.onObject(ctx, testSandbox("ns", "sandbox", "uid", "10", ""))

			tt.event(handler, ctx)

			route, found := routeRegistry.Get("ns--sandbox")
			require.True(t, found, "an invalid event must not remove the current record")
			assert.Equal(t, "10", route.ResourceVersion)
		})
	}
}

func TestBuildGatewayCacheOptions(t *testing.T) {
	tests := []struct {
		name          string
		namespace     string
		labelSelector string
		expectError   bool
	}{
		{name: "empty filters"},
		{name: "namespace", namespace: "team-a"},
		{name: "selector", labelSelector: "env=prod"},
		{name: "namespace and selector", namespace: "team-a", labelSelector: "env=prod"},
		{name: "invalid selector", labelSelector: "bad in (", expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			options, err := buildGatewayCacheOptions(tt.namespace, tt.labelSelector)
			if tt.expectError {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Len(t, options.ByObject, 1)

			var sandboxConfig ctrlcache.ByObject
			found := false
			for object, config := range options.ByObject {
				_, isSandbox := object.(*agentsv1alpha1.Sandbox)
				require.True(t, isSandbox)
				sandboxConfig = config
				found = true
			}
			require.True(t, found)
			if tt.namespace == "" {
				assert.Empty(t, sandboxConfig.Namespaces)
			} else {
				assert.Contains(t, sandboxConfig.Namespaces, tt.namespace)
				assert.Len(t, sandboxConfig.Namespaces, 1)
			}
			if tt.labelSelector == "" {
				assert.Nil(t, sandboxConfig.Label)
			} else {
				require.NotNil(t, sandboxConfig.Label)
				assert.True(t, sandboxConfig.Label.Matches(labels.Set{"env": "prod"}))
				assert.False(t, sandboxConfig.Label.Matches(labels.Set{"env": "dev"}))
			}
		})
	}
}

func TestStartManagerDependencyValidation(t *testing.T) {
	require.Error(t, StartManager(context.Background(), ManagerOptions{}))

	routeRegistry := registry.NewRegistry()
	err := StartManager(context.Background(), ManagerOptions{
		Registry:      routeRegistry,
		LabelSelector: "bad in (",
	})
	require.Error(t, err)
	assert.False(t, routeRegistry.Ready())
}

func newHandler() (*routeEventHandler, *registry.Registry) {
	routeRegistry := registry.NewRegistry()
	return &routeEventHandler{registry: routeRegistry}, routeRegistry
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
