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

	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr"
	"github.com/openkruise/agents/pkg/sandboxid"
)

func TestManagerRouteProjectionObservability(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expectID string
	}{
		{name: "legacy projection", expectID: "team-a--sandbox-a"},
		{name: "short projection", labels: map[string]string{sandboxid.LabelKey: "short-id"}, expectID: "short-id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := newManagerRouteTestSandbox("team-a", "sandbox-a")
			object.Labels = tt.labels
			route, err := (&SandboxManager{}).projectInfraSandbox(sandboxcr.AsSandbox(object, nil))
			require.NoError(t, err)
			assert.Equal(t, tt.expectID, route.ID)
		})
	}
}

func TestResolveSandboxID(t *testing.T) {
	tests := []struct {
		name     string
		labels   map[string]string
		expectID string
	}{
		{name: "legacy resolution", expectID: "team-a--sandbox-a"},
		{name: "short resolution", labels: map[string]string{sandboxid.LabelKey: "short-id"}, expectID: "short-id"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			object := &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "sandbox-a", Labels: tt.labels}}

			id := (&SandboxManager{}).ResolveSandboxID(object)

			assert.Equal(t, tt.expectID, id)
		})
	}
}
