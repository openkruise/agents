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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandbox-gateway/jwtauth"
	"github.com/openkruise/agents/pkg/sandbox-gateway/registry"
	"github.com/openkruise/agents/pkg/sandboxroute"
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

func TestSandboxReconcilerReconcile(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name          string
		object        *agentsv1alpha1.Sandbox
		key           types.NamespacedName
		policy        SandboxPolicy
		setup         func(*registry.Registry)
		configure     func(*SandboxReconciler)
		expectID      string
		expectPresent bool
		expectState   string
		expectAuth    bool
		expectError   string
	}{
		{
			name:          "present legacy Sandbox projects a full route",
			object:        testSandbox("ns", "legacy", "uid-a", "1", ""),
			key:           types.NamespacedName{Namespace: "ns", Name: "legacy"},
			expectID:      "ns--legacy",
			expectPresent: true,
			expectState:   agentsv1alpha1.SandboxStateRunning,
		},
		{
			name: "persisted short label and traffic auth are authoritative",
			object: func() *agentsv1alpha1.Sandbox {
				sandbox := testSandbox("ns", "short", "uid-b", "2", "short-id")
				sandbox.Annotations[identity.AnnotationEnableJwtAuth] = agentsv1alpha1.True
				return sandbox
			}(),
			key:           types.NamespacedName{Namespace: "ns", Name: "short"},
			expectID:      "short-id",
			expectPresent: true,
			expectState:   agentsv1alpha1.SandboxStateRunning,
			expectAuth:    true,
		},
		{
			name:   "lifecycle teardown returns error for controller retry",
			object: testSandbox("ns", "teardown", "uid-teardown", "3", "short-teardown"),
			key:    types.NamespacedName{Namespace: "ns", Name: "teardown"},
			configure: func(reconciler *SandboxReconciler) {
				reconciler.Registry.SetRepairEnqueuer(nil)
			},
			expectID:    "short-teardown",
			expectError: registry.ErrNotReady.Error(),
		},
		{
			name: "NotFound authoritatively deletes full current route",
			key:  types.NamespacedName{Namespace: "ns", Name: "gone"},
			setup: func(registry *registry.Registry) {
				registry.Upsert(testFullRoute("short-gone", "ns", "gone", "uid-c", "3"))
			},
			expectID: "short-gone",
		},
		{
			name: "NotFound uses injected fallback only for ID-only compatibility",
			key:  types.NamespacedName{Namespace: "ns", Name: "old"},
			setup: func(registry *registry.Registry) {
				registry.Upsert(sandboxroute.Route{ID: "ns--old", UID: "uid-d", ResourceVersion: "4"})
			},
			expectID: "ns--old",
		},
		{
			name: "deleting Sandbox is authoritative absence",
			object: func() *agentsv1alpha1.Sandbox {
				sandbox := testSandbox("ns", "deleting", "uid-e", "5", "short-deleting")
				sandbox.DeletionTimestamp = &now
				sandbox.Finalizers = []string{"test"}
				return sandbox
			}(),
			key: types.NamespacedName{Namespace: "ns", Name: "deleting"},
			setup: func(registry *registry.Registry) {
				registry.Upsert(testFullRoute("short-deleting", "ns", "deleting", "uid-e", "4"))
			},
			expectID: "short-deleting",
		},
		{
			name:   "namespace exclusion is authoritative absence",
			object: testSandbox("other", "excluded", "uid-f", "6", "short-excluded"),
			key:    types.NamespacedName{Namespace: "other", Name: "excluded"},
			policy: SandboxPolicy{
				Namespace: "visible",
				Selector:  labels.Everything(),
				Include:   func(*agentsv1alpha1.Sandbox, string) bool { return true },
			},
			expectID: "short-excluded",
		},
		{
			name:   "label exclusion is authoritative absence",
			object: testSandbox("ns", "excluded", "uid-g", "7", "short-label-excluded"),
			key:    types.NamespacedName{Namespace: "ns", Name: "excluded"},
			policy: SandboxPolicy{
				Selector: labels.SelectorFromSet(labels.Set{"included": "true"}),
				Include:  func(*agentsv1alpha1.Sandbox, string) bool { return true },
			},
			expectID: "short-label-excluded",
		},
		{
			name:   "state inclusion exclusion is authoritative absence",
			object: testSandbox("ns", "excluded", "uid-h", "8", "short-state-excluded"),
			key:    types.NamespacedName{Namespace: "ns", Name: "excluded"},
			policy: SandboxPolicy{
				Selector: labels.Everything(),
				Include:  func(*agentsv1alpha1.Sandbox, string) bool { return false },
			},
			expectID: "short-state-excluded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{}
			if tt.object != nil {
				objects = append(objects, tt.object)
			}
			reconciler, routeRegistry := testReconciler(t, objects, tt.policy)
			if tt.configure != nil {
				tt.configure(reconciler)
			}
			if tt.setup != nil {
				tt.setup(routeRegistry)
			}

			result, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: tt.key})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.False(t, result.Requeue)
			route, present := routeRegistry.Get(tt.expectID)
			assert.Equal(t, tt.expectPresent, present)
			if tt.expectPresent {
				assert.Equal(t, tt.key.Namespace, route.Namespace)
				assert.Equal(t, tt.key.Name, route.Name)
				assert.Equal(t, tt.expectState, route.State)
				assert.Equal(t, tt.expectAuth, route.RequireTrafficAuth)
			}
		})
	}
}

func TestSandboxReconcilerValidate(t *testing.T) {
	valid, _ := testReconciler(t, nil, SandboxPolicy{})
	tests := []struct {
		name        string
		configure   func(*SandboxReconciler)
		expectError string
	}{
		{name: "valid dependencies"},
		{
			name:        "nil client rejected",
			configure:   func(reconciler *SandboxReconciler) { reconciler.Client = nil },
			expectError: "client must not be nil",
		},
		{
			name:        "nil Registry rejected",
			configure:   func(reconciler *SandboxReconciler) { reconciler.Registry = nil },
			expectError: "Registry must not be nil",
		},
		{
			name:        "nil legacy fallback rejected",
			configure:   func(reconciler *SandboxReconciler) { reconciler.LegacyFallback = nil },
			expectError: "legacy fallback must not be nil",
		},
		{
			name: "uninitialized policy rejected",
			configure: func(reconciler *SandboxReconciler) {
				reconciler.Policy = SandboxPolicy{}
			},
			expectError: "policy must be initialized",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reconciler := *valid
			if tt.configure != nil {
				tt.configure(&reconciler)
			}
			err := reconciler.validate()
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestStartManagerDependencyFailureTearsDownReadiness(t *testing.T) {
	tests := []struct {
		name        string
		options     func(*registry.Registry) ManagerOptions
		expectError string
	}{
		{
			name: "missing legacy fallback keeps gateway unavailable",
			options: func(routeRegistry *registry.Registry) ManagerOptions {
				return ManagerOptions{Registry: routeRegistry}
			},
			expectError: "route dependencies must not be nil",
		},
		{
			name: "invalid selector keeps gateway unavailable",
			options: func(routeRegistry *registry.Registry) ManagerOptions {
				return ManagerOptions{
					Registry:       routeRegistry,
					LegacyFallback: func(string, string) string { return "legacy" },
					LabelSelector:  "bad in (",
				}
			},
			expectError: "parse sandbox label selector",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := sandboxroute.NewStore(sandboxroute.StoreOptions{})
			routeRegistry, err := registry.NewRegistry(store)
			require.NoError(t, err)
			routeRegistry.SetRepairEnqueuer(func(sandboxroute.MutationResult) {})
			require.True(t, routeRegistry.Ready())

			err = StartManager(context.Background(), tt.options(routeRegistry))

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectError)
			assert.False(t, routeRegistry.Ready())
		})
	}
}

func TestSandboxReconcilerDirectObservationUsesSamePolicy(t *testing.T) {
	tests := []struct {
		name          string
		object        *agentsv1alpha1.Sandbox
		key           types.NamespacedName
		policy        SandboxPolicy
		expectPresent bool
		expectError   string
	}{
		{
			name:          "direct reader projects included object",
			object:        testSandbox("ns", "included", "uid-a", "1", "short-a"),
			key:           types.NamespacedName{Namespace: "ns", Name: "included"},
			expectPresent: true,
		},
		{
			name:   "direct reader treats excluded object as absent",
			object: testSandbox("ns", "excluded", "uid-b", "2", "short-b"),
			key:    types.NamespacedName{Namespace: "ns", Name: "excluded"},
			policy: SandboxPolicy{
				Selector: labels.Everything(),
				Include:  func(*agentsv1alpha1.Sandbox, string) bool { return false },
			},
		},
		{
			name: "direct reader treats NotFound as absent",
			key:  types.NamespacedName{Namespace: "ns", Name: "missing"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objects := []client.Object{}
			if tt.object != nil {
				objects = append(objects, tt.object)
			}
			reconciler, _ := testReconciler(t, objects, tt.policy)
			observation, err := reconciler.observe(context.Background(), reconciler.Client, tt.key)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectPresent, observation.Present)
		})
	}
}

func TestNewSandboxPolicy(t *testing.T) {
	tests := []struct {
		name          string
		selector      string
		expectMatches bool
		expectError   string
	}{
		{name: "empty selector matches all", expectMatches: true},
		{name: "valid selector is applied", selector: "included=true", expectMatches: true},
		{name: "nonmatching selector is applied", selector: "included=false"},
		{name: "invalid selector rejected", selector: "bad in (", expectError: "parse sandbox label selector"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			policy, err := NewSandboxPolicy("", tt.selector, nil)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectMatches, policy.Selector.Matches(labels.Set{"included": "true"}))
			assert.True(t, policy.Include(testSandbox("ns", "a", "uid", "1", ""), "state"))
		})
	}
}

func testReconciler(
	t *testing.T,
	objects []client.Object,
	policy SandboxPolicy,
) (*SandboxReconciler, *registry.Registry) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	store := sandboxroute.NewStore(sandboxroute.StoreOptions{})
	routeRegistry, err := registry.NewRegistry(store)
	require.NoError(t, err)
	routeRegistry.SetRepairEnqueuer(func(sandboxroute.MutationResult) {})
	if policy.Selector == nil {
		policy.Selector = labels.Everything()
	}
	if policy.Include == nil {
		policy.Include = func(*agentsv1alpha1.Sandbox, string) bool { return true }
	}
	return &SandboxReconciler{
		Client:         fakeClient,
		Registry:       routeRegistry,
		LegacyFallback: func(namespace, name string) string { return fmt.Sprintf("%s--%s", namespace, name) },
		Policy:         policy,
	}, routeRegistry
}

func testSandbox(namespace, name, uid, resourceVersion, id string) *agentsv1alpha1.Sandbox {
	labels := map[string]string{}
	if id != "" {
		labels[agentsv1alpha1.LabelSandboxID] = id
	}
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       namespace,
			Name:            name,
			UID:             types.UID(uid),
			ResourceVersion: resourceVersion,
			Labels:          labels,
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

func testFullRoute(id, namespace, name, uid, resourceVersion string) sandboxroute.Route {
	return sandboxroute.Route{
		ID:              id,
		Namespace:       namespace,
		Name:            name,
		UID:             types.UID(uid),
		ResourceVersion: resourceVersion,
	}
}
