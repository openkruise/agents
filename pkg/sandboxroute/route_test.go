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

package sandboxroute

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/identity"
	"github.com/openkruise/agents/pkg/sandboxid"
)

func TestRouteValidation(t *testing.T) {
	tests := []struct {
		name        string
		route       Route
		expectError string
	}{
		{name: "full", route: fullRoute("id", "ns", "name", "uid", "1")},
		{name: "ID only", route: idOnlyRoute("id", "uid", "1"), expectError: "namespace and name must not be empty"},
		{name: "partial namespace", route: Route{ID: "id", Namespace: "ns", UID: "uid", ResourceVersion: "1"}, expectError: "namespace and name must not be empty"},
		{name: "partial name", route: Route{ID: "id", Name: "name", UID: "uid", ResourceVersion: "1"}, expectError: "namespace and name must not be empty"},
		{name: "missing ID", route: Route{Namespace: "ns", Name: "name", UID: "uid", ResourceVersion: "1"}, expectError: "ID must not be empty"},
		{name: "missing UID", route: Route{ID: "id", Namespace: "ns", Name: "name", ResourceVersion: "1"}, expectError: "UID must not be empty"},
		{name: "missing resource version", route: Route{ID: "id", Namespace: "ns", Name: "name", UID: "uid"}, expectError: "resource version must not be empty"},
		{name: "zero resource version", route: fullRoute("id", "ns", "name", "uid", "0"), expectError: "resource version is invalid"},
		{name: "leading-zero resource version", route: fullRoute("id", "ns", "name", "uid", "01"), expectError: "resource version is invalid"},
		{name: "non-numeric resource version", route: fullRoute("id", "ns", "name", "uid", "rv"), expectError: "resource version is invalid"},
		{
			name:  "arbitrary-precision resource version",
			route: fullRoute("id", "ns", "name", "uid", "18446744073709551616"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.route.validate()
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestRouteSecurityAndJSONCompatibility(t *testing.T) {
	tests := []struct {
		name          string
		route         Route
		expectObject  bool
		expectJSONKey bool
		expectAuth    bool
	}{
		{name: "full token", route: fullRoute("id", "ns", "name", "uid", "1"), expectObject: true, expectJSONKey: true, expectAuth: true},
		{name: "id only empty token", route: idOnlyRoute("id", "uid", "1")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.route.AccessToken = "secret-value"
			tt.route.RequireTrafficAuth = tt.expectAuth
			if strings.Contains(tt.name, "empty token") {
				tt.route.AccessToken = ""
			}
			rendered := tt.route.String()
			assert.Contains(t, rendered, "AccessToken:***")
			assert.Contains(t, rendered, fmt.Sprintf("RequireTrafficAuth:%t", tt.expectAuth))
			assert.NotContains(t, rendered, "secret-value")

			key, ok := tt.route.ObjectKey()
			assert.Equal(t, tt.expectObject, ok)
			if ok {
				assert.Equal(t, types.NamespacedName{Namespace: "ns", Name: "name"}, key)
			}
			payload, err := json.Marshal(tt.route)
			require.NoError(t, err)
			assert.Equal(t, tt.expectJSONKey, strings.Contains(string(payload), `"namespace"`))
			assert.Equal(t, tt.expectJSONKey, strings.Contains(string(payload), `"name"`))
		})
	}
}

func TestRouteFromSandboxDerivation(t *testing.T) {
	newSandbox := func(labels, annotations map[string]string) *agentsv1alpha1.Sandbox {
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[agentsv1alpha1.AnnotationOwner] = "owner"
		return &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "ns", Name: "name", UID: "uid", ResourceVersion: "7",
				Labels: labels, Annotations: annotations,
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
	tests := []struct {
		name        string
		sandbox     *agentsv1alpha1.Sandbox
		expectID    string
		expectToken string
		expectAuth  bool
		expectState string
		expectIP    string
		expectError string
	}{
		{
			name:     "legacy ID resolution",
			sandbox:  newSandbox(nil, nil),
			expectID: "ns--name",
		},
		{
			name:     "short label ID wins",
			sandbox:  newSandbox(map[string]string{sandboxid.LabelKey: "short-id"}, nil),
			expectID: "short-id",
		},
		{
			name: "runtime token wins over legacy envd token",
			sandbox: newSandbox(nil, map[string]string{
				agentsv1alpha1.AnnotationRuntimeAccessToken: "runtime-token",
				agentsv1alpha1.AnnotationEnvdAccessToken:    "legacy-token",
			}),
			expectID:    "ns--name",
			expectToken: "runtime-token",
		},
		{
			name: "legacy envd token fallback",
			sandbox: newSandbox(nil, map[string]string{
				agentsv1alpha1.AnnotationEnvdAccessToken: "legacy-token",
			}),
			expectID:    "ns--name",
			expectToken: "legacy-token",
		},
		{
			name: "jwt auth requires exact true",
			sandbox: newSandbox(nil, map[string]string{
				identity.AnnotationEnableJwtAuth: agentsv1alpha1.True,
			}),
			expectID:   "ns--name",
			expectAuth: true,
		},
		{
			name: "jwt auth non-true value is ignored",
			sandbox: newSandbox(nil, map[string]string{
				identity.AnnotationEnableJwtAuth: "True",
			}),
			expectID: "ns--name",
		},
		{
			name: "empty IP normalizes to creating",
			sandbox: func() *agentsv1alpha1.Sandbox {
				sandbox := newSandbox(nil, nil)
				sandbox.Status.PodInfo.PodIP = ""
				return sandbox
			}(),
			expectID:    "ns--name",
			expectState: agentsv1alpha1.SandboxStateCreating,
			expectIP:    "",
		},
		{
			name: "paused sandbox projects paused state",
			sandbox: func() *agentsv1alpha1.Sandbox {
				sandbox := newSandbox(nil, nil)
				sandbox.Spec.Paused = true
				return sandbox
			}(),
			expectID:    "ns--name",
			expectState: agentsv1alpha1.SandboxStatePaused,
			expectIP:    "10.0.0.1",
		},
		{
			name: "failed sandbox projects dead state",
			sandbox: func() *agentsv1alpha1.Sandbox {
				sandbox := newSandbox(nil, nil)
				sandbox.Status.Phase = agentsv1alpha1.SandboxFailed
				return sandbox
			}(),
			expectID:    "ns--name",
			expectState: agentsv1alpha1.SandboxStateDead,
			expectIP:    "10.0.0.1",
		},
		{
			name:        "nil sandbox rejected",
			expectError: "sandbox is nil",
		},
		{
			name:        "empty metadata rejected",
			sandbox:     &agentsv1alpha1.Sandbox{},
			expectError: "route namespace and name must not be empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, err := RouteFromSandbox(tt.sandbox)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, fullRoute(tt.expectID, "ns", "name", "uid", "7"), Route{
				ID: route.ID, Namespace: route.Namespace, Name: route.Name, UID: route.UID, ResourceVersion: route.ResourceVersion,
			})
			expectState := tt.expectState
			expectIP := tt.expectIP
			if expectState == "" {
				expectState = agentsv1alpha1.SandboxStateRunning
				expectIP = "10.0.0.1"
			}
			assert.Equal(t, expectIP, route.IP)
			assert.Equal(t, expectState, route.State)
			assert.Equal(t, "owner", route.Owner)
			assert.Equal(t, tt.expectToken, route.AccessToken)
			assert.Equal(t, tt.expectAuth, route.RequireTrafficAuth)
		})
	}
}

func fullRoute(id, namespace, name string, uid types.UID, resourceVersion string) Route {
	return Route{ID: id, Namespace: namespace, Name: name, UID: uid, ResourceVersion: resourceVersion}
}

func idOnlyRoute(id string, uid types.UID, resourceVersion string) Route {
	return Route{ID: id, UID: uid, ResourceVersion: resourceVersion}
}
