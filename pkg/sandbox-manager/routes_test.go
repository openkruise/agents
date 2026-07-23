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

package sandbox_manager

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxid"
	"github.com/openkruise/agents/pkg/sandboxroute"
)

func newManagerRouteTestSandbox(namespace, name string) *agentsv1alpha1.Sandbox {
	return &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:       namespace,
			Name:            name,
			UID:             types.UID("uid-" + name),
			ResourceVersion: "10",
			Labels:          map[string]string{"env": "prod"},
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner:              "owner-a",
				agentsv1alpha1.AnnotationRuntimeAccessToken: "secret-token",
			},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase:   agentsv1alpha1.SandboxRunning,
			PodInfo: agentsv1alpha1.PodInfo{PodIP: "10.0.0.1"},
			Conditions: []metav1.Condition{{
				Type:   string(agentsv1alpha1.SandboxConditionReady),
				Status: metav1.ConditionTrue,
			}},
		},
	}
}

func newRouteTestManager(t *testing.T) *SandboxManager {
	t.Helper()
	selector, err := labels.Parse("env=prod")
	require.NoError(t, err)
	return &SandboxManager{
		proxy:          proxy.NewServer(config.InitOptions(config.SandboxManagerOptions{})),
		routeNamespace: "team-a",
		routeSelector:  selector,
	}
}

func TestManagerProjectionAccessTokenCompatibility(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expectToken string
	}{
		{
			name: "runtime token",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-token",
			},
			expectToken: "runtime-token",
		},
		{
			name: "legacy envd token is not used",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationEnvdAccessToken: "legacy-token",
			},
		},
		{
			name: "runtime token wins over legacy envd token",
			annotations: map[string]string{
				agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-token",
				agentsv1alpha1.AnnotationEnvdAccessToken:    "legacy-token",
			},
			expectToken: "runtime-token",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sandbox := newManagerRouteTestSandbox("team-a", "token")
			sandbox.Annotations = tt.annotations

			route, err := (&SandboxManager{}).projectInfraSandbox(sandboxcr.AsSandbox(sandbox, nil))

			require.NoError(t, err)
			assert.Equal(t, tt.expectToken, route.AccessToken)
		})
	}
}

func TestSandboxManagerReconcileSandboxRoute(t *testing.T) {
	tests := []struct {
		name          string
		sandbox       *agentsv1alpha1.Sandbox
		notFound      bool
		seed          func(t *testing.T, manager *SandboxManager, sandbox *agentsv1alpha1.Sandbox)
		expectID      string
		expectPresent bool
	}{
		{
			name:          "present legacy route",
			sandbox:       newManagerRouteTestSandbox("team-a", "legacy"),
			expectID:      "team-a--legacy",
			expectPresent: true,
		},
		{
			name: "present short route",
			sandbox: func() *agentsv1alpha1.Sandbox {
				sandbox := newManagerRouteTestSandbox("team-a", "short")
				sandbox.Labels[agentsv1alpha1.LabelSandboxID] = "opaque-short-id"
				return sandbox
			}(),
			expectID:      "opaque-short-id",
			expectPresent: true,
		},
		{
			name: "deleting object is authoritative absence",
			sandbox: func() *agentsv1alpha1.Sandbox {
				sandbox := newManagerRouteTestSandbox("team-a", "deleting")
				now := metav1.Now()
				sandbox.DeletionTimestamp = &now
				return sandbox
			}(),
			seed: func(t *testing.T, manager *SandboxManager, sandbox *agentsv1alpha1.Sandbox) {
				copy := sandbox.DeepCopy()
				copy.DeletionTimestamp = nil
				route, err := manager.projectInfraSandbox(sandboxcr.AsSandbox(copy, nil))
				require.NoError(t, err)
				assert.Equal(t, sandboxroute.EventResultApplied, manager.proxy.SetRoute(t.Context(), route).Result)
			},
			expectID: "team-a--deleting",
		},
		{
			name: "selector exclusion is authoritative absence",
			sandbox: func() *agentsv1alpha1.Sandbox {
				sandbox := newManagerRouteTestSandbox("team-a", "excluded")
				sandbox.Labels["env"] = "dev"
				return sandbox
			}(),
			seed: func(t *testing.T, manager *SandboxManager, sandbox *agentsv1alpha1.Sandbox) {
				copy := sandbox.DeepCopy()
				copy.Labels["env"] = "prod"
				route, err := manager.projectInfraSandbox(sandboxcr.AsSandbox(copy, nil))
				require.NoError(t, err)
				assert.Equal(t, sandboxroute.EventResultApplied, manager.proxy.SetRoute(t.Context(), route).Result)
			},
			expectID: "team-a--excluded",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newRouteTestManager(t)
			if tt.seed != nil {
				tt.seed(t, manager, tt.sandbox)
			}
			var sandbox infra.Sandbox
			if !tt.notFound {
				sandbox = sandboxcr.AsSandbox(tt.sandbox, nil)
			}
			err := manager.reconcileSandboxRoute(
				t.Context(),
				types.NamespacedName{Namespace: tt.sandbox.Namespace, Name: tt.sandbox.Name},
				sandbox,
			)
			require.NoError(t, err)

			route, present := manager.proxy.LoadRoute(tt.expectID)
			assert.Equal(t, tt.expectPresent, present)
			if tt.expectPresent {
				assert.Equal(t, tt.sandbox.Namespace, route.Namespace)
				assert.Equal(t, tt.sandbox.Name, route.Name)
				assert.Equal(t, tt.sandbox.UID, route.UID)
				assert.Equal(t, "owner-a", route.Owner)
				assert.Equal(t, "secret-token", route.AccessToken)
			}
		})
	}
}

type managerRouteSource struct {
	sandbox infra.Sandbox
	err     error
}

func (managerRouteSource) RegisterEventHandler(infra.RouteSandboxEventHandler) error {
	return nil
}

func (s managerRouteSource) Observe(context.Context, types.NamespacedName) (infra.Sandbox, error) {
	return s.sandbox, s.err
}

func managerRouteSandbox(sandbox *agentsv1alpha1.Sandbox) infra.Sandbox {
	if sandbox == nil {
		return nil
	}
	return sandboxcr.AsSandbox(sandbox, nil)
}

func TestSandboxManagerObserveRoute(t *testing.T) {
	readErr := errors.New("direct read failed")
	tests := []struct {
		name          string
		source        infra.RouteSandboxSource
		expectPresent bool
		expectError   string
		expectCause   error
	}{
		{name: "present included", source: managerRouteSource{sandbox: managerRouteSandbox(newManagerRouteTestSandbox("team-a", "observed"))}, expectPresent: true},
		{name: "not found is absence", source: managerRouteSource{}},
		{name: "deleting is absence", source: managerRouteSource{sandbox: managerRouteSandbox(func() *agentsv1alpha1.Sandbox {
			sandbox := newManagerRouteTestSandbox("team-a", "deleting")
			now := metav1.Now()
			sandbox.DeletionTimestamp = &now
			return sandbox
		}())}},
		{name: "namespace exclusion is absence", source: managerRouteSource{sandbox: managerRouteSandbox(newManagerRouteTestSandbox("team-b", "excluded"))}},
		{name: "selector exclusion is absence", source: managerRouteSource{sandbox: managerRouteSandbox(func() *agentsv1alpha1.Sandbox {
			sandbox := newManagerRouteTestSandbox("team-a", "excluded")
			sandbox.Labels["env"] = "dev"
			return sandbox
		}())}},
		{name: "get error is classified", source: managerRouteSource{err: readErr}, expectError: readErr.Error(), expectCause: readErr},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := newRouteTestManager(t)
			observation, err := manager.observeRoute(tt.source)(t.Context(), types.NamespacedName{Namespace: "team-a", Name: "observed"})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if tt.expectCause != nil {
					assert.ErrorIs(t, err, tt.expectCause)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.expectPresent, observation.Present)
			if tt.expectPresent {
				assert.Equal(t, "team-a", observation.Route.Namespace)
				assert.Equal(t, "observed", observation.Route.Name)
			}
		})
	}
}

func TestIsCurrentRouteDeleteConflict(t *testing.T) {
	tests := []struct {
		name     string
		result   sandboxroute.MutationResult
		conflict bool
	}{
		{name: "stale snapshot", result: sandboxroute.MutationResult{Result: sandboxroute.EventResultIgnored, Reason: sandboxroute.ReasonStaleResourceVersion}, conflict: true},
		{name: "replaced incarnation", result: sandboxroute.MutationResult{Result: sandboxroute.EventResultIgnored, Reason: sandboxroute.ReasonIdentityMismatch}, conflict: true},
		{name: "already absent", result: sandboxroute.MutationResult{Result: sandboxroute.EventResultIgnored, Reason: sandboxroute.ReasonAbsent}},
		{name: "applied", result: sandboxroute.MutationResult{Result: sandboxroute.EventResultApplied}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.conflict, isCurrentRouteDeleteConflict(tt.result))
		})
	}
}

func TestSandboxManagerBuilderForwardsRouteRepairRequests(t *testing.T) {
	opts := config.InitOptions(config.SandboxManagerOptions{})
	managerCache, apiReader, err := cachetest.NewTestCacheWithOptions(t, infracache.Options{SandboxIDResolver: sandboxid.Resolve})
	require.NoError(t, err)

	manager, err := NewSandboxManagerBuilder(opts).
		WithCustomInfra(func() (infra.Builder, error) {
			return sandboxcr.NewInfraBuilder(opts).
				WithCache(managerCache).
				WithAPIReader(apiReader), nil
		}).
		Build()
	require.NoError(t, err)
	require.NotNil(t, manager.routeRepairer)

	route := sandboxroute.Route{
		ID: "old-id", Namespace: "team-a", Name: "sandbox-a", UID: "uid-a", ResourceVersion: "10",
	}
	assert.Equal(t, sandboxroute.EventResultApplied, manager.proxy.SetRoute(t.Context(), route).Result)
	key := types.NamespacedName{Namespace: route.Namespace, Name: route.Name}
	assert.Equal(t, sandboxroute.EventResultApplied, manager.proxy.DeleteCurrentRouteByObjectKey(key).Result)
	assert.Equal(t, sandboxroute.EventResultRepairRequired, manager.proxy.SetRoute(t.Context(), route).Result)
	assert.Equal(t, 1, manager.routeRepairer.Pending())
}
