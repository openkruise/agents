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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/proxy"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
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

func TestSandboxManagerHandleRouteSandboxEvent(t *testing.T) {
	manager := newRouteTestManager(t)
	sandbox := newManagerRouteTestSandbox("team-a", "sandbox")
	id := "team-a--sandbox"

	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Sandbox: sandboxcr.AsSandbox(sandbox, nil),
	})
	route, present := manager.proxy.LoadRoute(id)
	require.True(t, present)
	assert.Equal(t, sandbox.ResourceVersion, route.ResourceVersion)

	excluded := sandbox.DeepCopy()
	excluded.ResourceVersion = "11"
	excluded.Labels["env"] = "dev"
	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Sandbox: sandboxcr.AsSandbox(excluded, nil),
	})
	_, present = manager.proxy.LoadRoute(id)
	assert.False(t, present)

	excluded.Labels["env"] = "prod"
	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Sandbox: sandboxcr.AsSandbox(excluded, nil),
	})
	_, present = manager.proxy.LoadRoute(id)
	assert.False(t, present, "equal RV remains behind the policy-exclusion fence")

	excluded.ResourceVersion = "12"
	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Sandbox: sandboxcr.AsSandbox(excluded, nil),
	})
	_, present = manager.proxy.LoadRoute(id)
	assert.True(t, present)

	terminating := excluded.DeepCopy()
	terminating.ResourceVersion = "13"
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Sandbox: sandboxcr.AsSandbox(terminating, nil),
	})
	_, present = manager.proxy.LoadRoute(id)
	assert.False(t, present)

	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Delete: &sandboxroute.Delete{
			ObjectKey:       types.NamespacedName{Namespace: sandbox.Namespace, Name: sandbox.Name},
			ResourceVersion: "14",
		},
	})
	terminating.ResourceVersion = "14"
	terminating.DeletionTimestamp = nil
	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Sandbox: sandboxcr.AsSandbox(terminating, nil),
	})
	_, present = manager.proxy.LoadRoute(id)
	assert.False(t, present)
}

func TestSandboxManagerUnseenExclusionDoesNotFence(t *testing.T) {
	manager := newRouteTestManager(t)
	sandbox := newManagerRouteTestSandbox("team-a", "unseen")
	sandbox.Labels["env"] = "dev"

	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Sandbox: sandboxcr.AsSandbox(sandbox, nil),
	})
	sandbox.Labels["env"] = "prod"
	manager.handleRouteSandboxEvent(t.Context(), infra.RouteSandboxEvent{
		Sandbox: sandboxcr.AsSandbox(sandbox, nil),
	})
	_, present := manager.proxy.LoadRoute("team-a--unseen")
	assert.True(t, present)
}

type managerRouteSubscription struct {
	removed bool
}

func (s *managerRouteSubscription) Remove() error {
	s.removed = true
	return nil
}
