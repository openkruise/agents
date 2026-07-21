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
)

type projectionTestSource struct {
	metav1.ObjectMeta
	podIP       string
	state       string
	id          string
	accessToken string
	requireAuth bool
}

func (s *projectionTestSource) GetIP() string {
	return s.podIP
}

func (s *projectionTestSource) GetState() (string, string) {
	return s.state, ""
}

func (s *projectionTestSource) GetID() string {
	return s.id
}

func (s *projectionTestSource) GetAccessToken() string {
	return s.accessToken
}

func (s *projectionTestSource) RequiresTrafficAuth() bool {
	return s.requireAuth
}

func TestRouteShapeAndValidation(t *testing.T) {
	tests := []struct {
		name        string
		route       Route
		expectShape Shape
		expectError string
	}{
		{name: "full", route: fullRoute("id", "ns", "name", "uid", "1"), expectShape: ShapeFull},
		{name: "id only", route: idOnlyRoute("id", "uid", "1"), expectShape: ShapeIDOnly},
		{name: "partial namespace", route: Route{ID: "id", Namespace: "ns", UID: "uid", ResourceVersion: "1"}, expectError: "both be set"},
		{name: "partial name", route: Route{ID: "id", Name: "name", UID: "uid", ResourceVersion: "1"}, expectError: "both be set"},
		{name: "missing ID", route: Route{UID: "uid", ResourceVersion: "1"}, expectError: "ID must not be empty"},
		{name: "missing UID", route: Route{ID: "id", ResourceVersion: "1"}, expectError: "UID must not be empty"},
		{name: "missing resource version", route: Route{ID: "id", UID: "uid"}, expectError: "resource version must not be empty"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shape, shapeErr := tt.route.Shape()
			err := tt.route.Validate()
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				if strings.Contains(tt.expectError, "both be set") {
					require.Error(t, shapeErr)
				}
				return
			}
			require.NoError(t, shapeErr)
			require.NoError(t, err)
			assert.Equal(t, tt.expectShape, shape)
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

func TestProjectRoute(t *testing.T) {
	source := &projectionTestSource{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns", Name: "name", UID: "uid", ResourceVersion: "7",
			Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "owner"},
		},
		podIP:       "10.0.0.1",
		state:       agentsv1alpha1.SandboxStateRunning,
		id:          "opaque-id",
		accessToken: "runtime-token",
		requireAuth: true,
	}
	emptyID := *source
	emptyID.id = ""
	emptyIP := *source
	emptyIP.podIP = ""
	tests := []struct {
		name        string
		source      ProjectionSource
		expectIP    string
		expectState string
		expectToken string
		expectError string
	}{
		{
			name:        "projection",
			source:      source,
			expectIP:    "10.0.0.1",
			expectState: agentsv1alpha1.SandboxStateRunning,
			expectToken: "runtime-token",
		},
		{
			name:        "empty IP normalizes to creating",
			source:      &emptyIP,
			expectState: agentsv1alpha1.SandboxStateCreating,
			expectToken: "runtime-token",
		},
		{name: "nil source", expectError: "source is nil"},
		{name: "empty resolution", source: &emptyID, expectError: "empty sandbox ID"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, err := ProjectRoute(tt.source)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, fullRoute("opaque-id", "ns", "name", "uid", "7"), Route{
				ID: route.ID, Namespace: route.Namespace, Name: route.Name, UID: route.UID, ResourceVersion: route.ResourceVersion,
			})
			assert.Equal(t, tt.expectIP, route.IP)
			assert.Equal(t, tt.expectState, route.State)
			assert.Equal(t, "owner", route.Owner)
			assert.Equal(t, tt.expectToken, route.AccessToken)
			assert.True(t, route.RequireTrafficAuth)
		})
	}
}

func TestCompareResourceVersions(t *testing.T) {
	tests := []struct {
		name     string
		current  string
		incoming string
		expect   ResourceVersionComparison
	}{
		{name: "identical empty", expect: ResourceVersionEqual},
		{name: "identical malformed", current: "abc", incoming: "abc", expect: ResourceVersionEqual},
		{name: "numeric equal", current: "01", incoming: "1", expect: ResourceVersionEqual},
		{name: "older", current: "2", incoming: "1", expect: ResourceVersionOlder},
		{name: "newer", current: "1", incoming: "2", expect: ResourceVersionNewer},
		{name: "malformed current", current: "x", incoming: "2", expect: ResourceVersionUnorderable},
		{name: "malformed incoming", current: "2", incoming: "x", expect: ResourceVersionUnorderable},
		{name: "overflow", current: "2", incoming: "18446744073709551616", expect: ResourceVersionUnorderable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expect, CompareResourceVersions(tt.current, tt.incoming))
		})
	}
}

func fullRoute(id, namespace, name string, uid types.UID, resourceVersion string) Route {
	return Route{ID: id, Namespace: namespace, Name: name, UID: uid, ResourceVersion: resourceVersion}
}

func idOnlyRoute(id string, uid types.UID, resourceVersion string) Route {
	return Route{ID: id, UID: uid, ResourceVersion: resourceVersion}
}
