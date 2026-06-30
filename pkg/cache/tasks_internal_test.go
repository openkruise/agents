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

package cache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
)

type checkpointUpdateFuncTestCase struct {
	name               string
	cacheObjects       []ctrlclient.Object
	apiObjects         []ctrlclient.Object
	expectCheckpointID string
	expectError        string
}

func TestCheckpointUpdateFunc_FallsBackToAPIReader(t *testing.T) {
	tests := []checkpointUpdateFuncTestCase{
		{
			name: "cache hit returns cached checkpoint",
			cacheObjects: []ctrlclient.Object{
				newCheckpointUpdateFuncTestCheckpoint("cp-cache-hit", "cache-id"),
			},
			apiObjects: []ctrlclient.Object{
				newCheckpointUpdateFuncTestCheckpoint("cp-cache-hit", "api-id"),
			},
			expectCheckpointID: "cache-id",
		},
		{
			name: "cache miss falls back to api reader",
			apiObjects: []ctrlclient.Object{
				newCheckpointUpdateFuncTestCheckpoint("cp-api-hit", "api-id"),
			},
			expectCheckpointID: "api-id",
		},
		{
			name:        "cache and api reader miss returns not found",
			expectError: "not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cacheClient := newCheckpointUpdateFuncTestClient(t, tt.cacheObjects...)
			apiReader := newCheckpointUpdateFuncTestClient(t, tt.apiObjects...)
			c := &Cache{client: cacheClient, reader: apiReader}

			cp := &agentsv1alpha1.Checkpoint{
				ObjectMeta: metav1.ObjectMeta{Name: checkpointUpdateFuncTestCheckpointName(tt), Namespace: "default"},
			}
			got, err := c.CheckpointUpdateFunc(context.Background())(cp)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.expectCheckpointID, got.Status.CheckpointId)
		})
	}
}

func checkpointUpdateFuncTestCheckpointName(tt checkpointUpdateFuncTestCase) string {
	if len(tt.cacheObjects) > 0 {
		return tt.cacheObjects[0].GetName()
	}
	if len(tt.apiObjects) > 0 {
		return tt.apiObjects[0].GetName()
	}
	return "cp-missing"
}

func newCheckpointUpdateFuncTestCheckpoint(name, checkpointID string) *agentsv1alpha1.Checkpoint {
	return &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Status:     agentsv1alpha1.CheckpointStatus{CheckpointId: checkpointID},
	}
}

func newCheckpointUpdateFuncTestClient(t *testing.T, objects ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, agentsv1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}
