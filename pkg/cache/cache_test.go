/*
Copyright 2025.

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

package cache_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
)

// --- Index query tests ---

func TestCache_GetClaimedSandbox(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sbx",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
			},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
	}

	t.Run("found", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)
		sandboxID := sandboxutils.GetSandboxID(sbx)
		got, err := c.GetClaimedSandbox(t.Context(), cache.GetClaimedSandboxOptions{SandboxID: sandboxID})
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "test-sbx", got.Name)
	})

	t.Run("not found", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)
		_, err = c.GetClaimedSandbox(t.Context(), cache.GetClaimedSandboxOptions{SandboxID: "nonexistent-id"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in cache")
	})
}

func TestCache_GetCheckpoint(t *testing.T) {
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cp", Namespace: "default"},
		Status:     agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-id-123"},
	}

	t.Run("found", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t, cp)
		require.NoError(t, err)
		got, err := c.GetCheckpoint(t.Context(), cache.GetCheckpointOptions{CheckpointID: "cp-id-123"})
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "test-cp", got.Name)
	})

	t.Run("not found", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)
		_, err = c.GetCheckpoint(t.Context(), cache.GetCheckpointOptions{CheckpointID: "nonexistent-cp"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in cache")
	})
}

func TestCache_GetCheckpointWithOptions_NamespaceScoped(t *testing.T) {
	checkpoints := []ctrlclient.Object{
		&agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-a", Namespace: "team-a"},
			Status:     agentsv1alpha1.CheckpointStatus{CheckpointId: "shared-cp-id"},
		},
		&agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "cp-b", Namespace: "team-b"},
			Status:     agentsv1alpha1.CheckpointStatus{CheckpointId: "shared-cp-id"},
		},
	}
	c, _, err := cachetest.NewTestCache(t, checkpoints...)
	require.NoError(t, err)

	tests := []struct {
		name        string
		namespace   string
		expectName  string
		expectError string
	}{
		{
			name:       "team-a resolves team-a checkpoint",
			namespace:  "team-a",
			expectName: "cp-a",
		},
		{
			name:       "team-b resolves team-b checkpoint",
			namespace:  "team-b",
			expectName: "cp-b",
		},
		{
			name:        "missing namespace returns not found",
			namespace:   "team-c",
			expectError: "not found in cache",
		},
		{
			name: "empty namespace returns first match with limit",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.GetCheckpoint(t.Context(), cache.GetCheckpointOptions{
				Namespace:    tt.namespace,
				CheckpointID: "shared-cp-id",
			})
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				return
			}
			require.NoError(t, err)
			if tt.expectName != "" {
				assert.Equal(t, tt.expectName, got.Name)
				assert.Equal(t, tt.namespace, got.Namespace)
			} else {
				assert.NotEmpty(t, got.Name)
				assert.NotEmpty(t, got.Namespace)
			}
		})
	}
}

func TestCache_GetSandboxSet(t *testing.T) {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-1", Namespace: "team-a"},
	}

	t.Run("found by name index", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t, sbs)
		require.NoError(t, err)
		got, err := c.PickSandboxSet(t.Context(), cache.PickSandboxSetOptions{Name: "tmpl-1"})
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "tmpl-1", got.Name)
		assert.Equal(t, "team-a", got.Namespace)
	})

	t.Run("not found", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t)
		require.NoError(t, err)
		_, err = c.PickSandboxSet(t.Context(), cache.PickSandboxSetOptions{Name: "nonexistent"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in cache")
	})
}

func TestCache_GetSandboxSet_MultipleTemplates(t *testing.T) {
	sbs1 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-1", Namespace: "team-a"},
	}
	sbs2 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-2", Namespace: "team-a"},
	}
	sbs3 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-3", Namespace: "team-b"},
	}

	c, _, err := cachetest.NewTestCache(t, sbs1, sbs2, sbs3)
	require.NoError(t, err)

	got, err := c.PickSandboxSet(t.Context(), cache.PickSandboxSetOptions{Name: "tmpl-3"})
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "tmpl-3", got.Name)
	assert.Equal(t, "team-b", got.Namespace)

	_, err = c.PickSandboxSet(t.Context(), cache.PickSandboxSetOptions{Name: "nonexistent"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in cache")
}

func TestCache_PickSandboxSetWithOptions_NamespaceScoped(t *testing.T) {
	sbsA := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-template", Namespace: "team-a"},
	}
	sbsB := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "shared-template", Namespace: "team-b"},
	}
	c, _, err := cachetest.NewTestCache(t, sbsA, sbsB)
	require.NoError(t, err)

	tests := []struct {
		name       string
		namespace  string
		expectName string
	}{
		{
			name:       "team-a resolves team-a sandboxset",
			namespace:  "team-a",
			expectName: "team-a",
		},
		{
			name:       "team-b resolves team-b sandboxset",
			namespace:  "team-b",
			expectName: "team-b",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.PickSandboxSet(t.Context(), cache.PickSandboxSetOptions{
				Namespace: tt.namespace,
				Name:      "shared-template",
			})
			require.NoError(t, err)
			assert.Equal(t, "shared-template", got.Name)
			assert.Equal(t, tt.expectName, got.Namespace)
		})
	}
}

func TestCache_ListSandboxSets(t *testing.T) {
	sbsA := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-a", Namespace: "team-a"},
	}
	sbsB := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-b", Namespace: "team-b"},
	}
	sbsC := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-c", Namespace: "team-a"},
	}

	c, _, err := cachetest.NewTestCache(t, sbsA, sbsB, sbsC)
	require.NoError(t, err)

	tests := []struct {
		name           string
		namespace      string
		expectedNames  []string
		expectedLen    int
		unexpectedName string
	}{
		{
			name:           "namespace scoped list returns matching sandboxsets",
			namespace:      "team-a",
			expectedNames:  []string{"tmpl-a", "tmpl-c"},
			expectedLen:    2,
			unexpectedName: "tmpl-b",
		},
		{
			name:          "empty namespace returns all visible sandboxsets",
			namespace:     "",
			expectedNames: []string{"tmpl-a", "tmpl-b", "tmpl-c"},
			expectedLen:   3,
		},
		{
			name:        "namespace with no sandboxsets returns empty list",
			namespace:   "team-c",
			expectedLen: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			list, listErr := c.ListSandboxSets(t.Context(), cache.ListSandboxSetsOptions{
				Namespace: tt.namespace,
			})
			require.NoError(t, listErr)
			assert.Len(t, list, tt.expectedLen)

			gotNames := make([]string, 0, len(list))
			for _, item := range list {
				gotNames = append(gotNames, item.Name)
			}

			for _, expectedName := range tt.expectedNames {
				assert.Contains(t, gotNames, expectedName)
			}
			if tt.unexpectedName != "" {
				assert.NotContains(t, gotNames, tt.unexpectedName)
			}
		})
	}
}

// --- List tests ---

func TestCache_ListSandboxesWithOptions_UserScoped(t *testing.T) {
	sbx1 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sbx-1", Namespace: "default",
			Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-1"},
			Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
		},
	}
	sbx2 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sbx-2", Namespace: "default",
			Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-1"},
			Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
		},
	}
	sbx3 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "sbx-3", Namespace: "default",
			Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-2"},
			Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
		},
	}

	c, _, err := cachetest.NewTestCache(t, sbx1, sbx2, sbx3)
	require.NoError(t, err)

	list, err := c.ListSandboxes(t.Context(), cache.ListSandboxesOptions{User: "user-1"})
	require.NoError(t, err)
	assert.Len(t, list, 2)

	list, err = c.ListSandboxes(t.Context(), cache.ListSandboxesOptions{User: "user-2"})
	require.NoError(t, err)
	assert.Len(t, list, 1)

	list, err = c.ListSandboxes(t.Context(), cache.ListSandboxesOptions{User: "user-nobody"})
	require.NoError(t, err)
	assert.Len(t, list, 0)
}

func TestCache_ListSandboxesWithOptions_NamespaceAndUserScoped(t *testing.T) {
	sandboxes := []ctrlclient.Object{
		&agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sbx-a",
				Namespace:   "team-a",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "same-user"},
				Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
		},
		&agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sbx-b",
				Namespace:   "team-b",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "same-user"},
				Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
		},
	}
	c, _, err := cachetest.NewTestCache(t, sandboxes...)
	require.NoError(t, err)

	list, err := c.ListSandboxes(t.Context(), cache.ListSandboxesOptions{
		Namespace: "team-a",
		User:      "same-user",
	})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "sbx-a", list[0].Name)
	assert.Equal(t, "team-a", list[0].Namespace)
}

func TestCache_ListSandboxesWithOptions_WithoutUserReturnsNamespaceScopedResults(t *testing.T) {
	sandboxes := []ctrlclient.Object{
		&agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sbx-a",
				Namespace:   "team-a",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-a"},
				Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
		},
		&agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sbx-b",
				Namespace:   "team-a",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-b"},
				Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
		},
		&agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "sbx-c",
				Namespace:   "team-b",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-c"},
				Labels:      map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
		},
	}
	c, _, err := cachetest.NewTestCache(t, sandboxes...)
	require.NoError(t, err)

	list, err := c.ListSandboxes(t.Context(), cache.ListSandboxesOptions{
		Namespace: "team-a",
	})
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestCache_ListCheckpointsWithOptions_UserScoped(t *testing.T) {
	cp1 := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cp-1", Namespace: "default",
			Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-1"},
		},
		Status: agentsv1alpha1.CheckpointStatus{CheckpointId: "cpid-1"},
	}
	cp2 := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cp-2", Namespace: "default",
			Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-2"},
		},
		Status: agentsv1alpha1.CheckpointStatus{CheckpointId: "cpid-2"},
	}

	c, _, err := cachetest.NewTestCache(t, cp1, cp2)
	require.NoError(t, err)

	list, err := c.ListCheckpoints(t.Context(), cache.ListCheckpointsOptions{User: "user-1"})
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "cp-1", list[0].Name)

	list, err = c.ListCheckpoints(t.Context(), cache.ListCheckpointsOptions{User: "user-nobody"})
	require.NoError(t, err)
	assert.Len(t, list, 0)
}

func TestCache_ListCheckpointsWithOptions_NamespaceAndUserScoped(t *testing.T) {
	checkpoints := []ctrlclient.Object{
		&agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "cp-a",
				Namespace:   "team-a",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "same-user"},
			},
			Status: agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-a-id"},
		},
		&agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "cp-b",
				Namespace:   "team-b",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "same-user"},
			},
			Status: agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-b-id"},
		},
	}
	c, _, err := cachetest.NewTestCache(t, checkpoints...)
	require.NoError(t, err)

	list, err := c.ListCheckpoints(t.Context(), cache.ListCheckpointsOptions{
		Namespace: "team-b",
		User:      "same-user",
	})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "cp-b", list[0].Name)
	assert.Equal(t, "team-b", list[0].Namespace)
}

func TestCache_ListCheckpointsWithOptions_WithoutUserReturnsNamespaceScopedResults(t *testing.T) {
	checkpoints := []ctrlclient.Object{
		&agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "cp-a",
				Namespace:   "team-a",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-a"},
			},
			Status: agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-a-id"},
		},
		&agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "cp-b",
				Namespace:   "team-a",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-b"},
			},
			Status: agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-b-id"},
		},
		&agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "cp-c",
				Namespace:   "team-b",
				Annotations: map[string]string{agentsv1alpha1.AnnotationOwner: "user-c"},
			},
			Status: agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-c-id"},
		},
	}
	c, _, err := cachetest.NewTestCache(t, checkpoints...)
	require.NoError(t, err)

	list, err := c.ListCheckpoints(t.Context(), cache.ListCheckpointsOptions{
		Namespace: "team-a",
	})
	require.NoError(t, err)
	require.Len(t, list, 2)
}

func TestCache_ListSandboxesInPool(t *testing.T) {
	// To get "available" state, sandbox must be controlled by SandboxSet and be ready
	sbsRef := metav1.OwnerReference{
		APIVersion: agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String(),
		Kind:       agentsv1alpha1.SandboxSetControllerKind.Kind,
		Name:       "test-sbs",
		UID:        "12345",
		Controller: boolPtr(true),
	}
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pool-sbx-1", Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxTemplate: "tmpl-a",
			},
			OwnerReferences: []metav1.OwnerReference{sbsRef},
		},
		Status: agentsv1alpha1.SandboxStatus{
			Phase: agentsv1alpha1.SandboxRunning,
			Conditions: []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			},
		},
	}

	c, _, err := cachetest.NewTestCache(t, sbx)
	require.NoError(t, err)

	list, err := c.ListSandboxesInPool(t.Context(), cache.ListSandboxesInPoolOptions{Pool: "tmpl-a"})
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "pool-sbx-1", list[0].Name)

	list, err = c.ListSandboxesInPool(t.Context(), cache.ListSandboxesInPoolOptions{Pool: "tmpl-nonexistent"})
	require.NoError(t, err)
	assert.Len(t, list, 0)
}

func TestCache_ListSandboxesInPoolWithOptions_NamespaceScoped(t *testing.T) {
	sbsRef := metav1.OwnerReference{
		APIVersion: agentsv1alpha1.SandboxSetControllerKind.GroupVersion().String(),
		Kind:       agentsv1alpha1.SandboxSetControllerKind.Kind,
		Name:       "shared-template",
		UID:        "12345",
		Controller: boolPtr(true),
	}
	sandboxes := []ctrlclient.Object{
		&agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "pool-a",
				Namespace:       "team-a",
				Labels:          map[string]string{agentsv1alpha1.LabelSandboxTemplate: "shared-template"},
				OwnerReferences: []metav1.OwnerReference{sbsRef},
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
		},
		&agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:            "pool-b",
				Namespace:       "team-b",
				Labels:          map[string]string{agentsv1alpha1.LabelSandboxTemplate: "shared-template"},
				OwnerReferences: []metav1.OwnerReference{sbsRef},
			},
			Status: agentsv1alpha1.SandboxStatus{
				Phase:      agentsv1alpha1.SandboxRunning,
				Conditions: []metav1.Condition{{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue}},
			},
		},
	}
	c, _, err := cachetest.NewTestCache(t, sandboxes...)
	require.NoError(t, err)

	list, err := c.ListSandboxesInPool(t.Context(), cache.ListSandboxesInPoolOptions{
		Namespace: "team-b",
		Pool:      "shared-template",
	})
	require.NoError(t, err)
	require.Len(t, list, 1)
	assert.Equal(t, "pool-b", list[0].Name)
	assert.Equal(t, "team-b", list[0].Namespace)
}

// --- Wait tests ---

func TestCache_WaitForSandbox_AdHoc(t *testing.T) {
	t.Run("already satisfied", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wait-sbx-satisfied", Namespace: "default",
				Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
		}
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)
		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			t.Context(), c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(t.Context()),
			func(s *agentsv1alpha1.Sandbox) (bool, error) { return true, nil },
		)
		err = task.Wait(time.Second)
		require.NoError(t, err)
	})

	t.Run("check function returns error immediately", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wait-sbx-err", Namespace: "default",
				Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
		}
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)
		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			t.Context(), c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(t.Context()),
			func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, assert.AnError },
		)
		err = task.Wait(time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), assert.AnError.Error())
	})

	t.Run("unsatisfied condition should timeout", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wait-sbx-timeout", Namespace: "default",
				Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
		}
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)
		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			t.Context(), c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(t.Context()),
			func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		)
		err = task.Wait(100 * time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not satisfied")
	})

	t.Run("wait task conflict", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wait-sbx-conflict", Namespace: "default",
				Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
		}
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		// Start a long-running wait with action "another"
		go func() {
			_ = cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
				t.Context(), c.GetWaitHooks(), "another", sbx,
				c.SandboxUpdateFunc(t.Context()),
				func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
			).Wait(time.Hour)
		}()
		// Give the goroutine time to register the wait hook
		time.Sleep(50 * time.Millisecond)

		// Try a different action on same sandbox - should conflict
		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			t.Context(), c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(t.Context()),
			func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		)
		err = task.Wait(time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "already exists")
	})

	t.Run("sandbox satisfied after update", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wait-sbx-update", Namespace: "default",
				Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
		}
		c, fc, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		// Update sandbox in background to satisfy condition, and close waitHook
		go func() {
			time.Sleep(50 * time.Millisecond)
			// Update the sandbox to Running
			fresh := &agentsv1alpha1.Sandbox{}
			_ = fc.Get(t.Context(), ctrlclient.ObjectKeyFromObject(sbx), fresh)
			fresh.Status.Phase = agentsv1alpha1.SandboxRunning
			_ = fc.Status().Update(t.Context(), fresh)

			// Manually trigger waitHook (fake client has no informer)
			key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
			if val, ok := c.GetWaitHooks().Load(key); ok {
				entry := val.(*cacheutils.WaitEntry[*agentsv1alpha1.Sandbox])
				entry.Close()
			}
		}()

		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			t.Context(), c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(t.Context()),
			func(s *agentsv1alpha1.Sandbox) (bool, error) {
				return s.Status.Phase == agentsv1alpha1.SandboxRunning, nil
			},
		)
		err = task.Wait(2 * time.Second)
		require.NoError(t, err)
	})

	t.Run("check function returns error on update", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wait-sbx-check-err", Namespace: "default",
				Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
		}
		c, fc, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		// Update and close waitHook so double-check runs
		go func() {
			time.Sleep(50 * time.Millisecond)
			fresh := &agentsv1alpha1.Sandbox{}
			_ = fc.Get(t.Context(), ctrlclient.ObjectKeyFromObject(sbx), fresh)
			fresh.Status.Phase = agentsv1alpha1.SandboxRunning
			_ = fc.Status().Update(t.Context(), fresh)

			key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
			if val, ok := c.GetWaitHooks().Load(key); ok {
				entry := val.(*cacheutils.WaitEntry[*agentsv1alpha1.Sandbox])
				entry.Close()
			}
		}()

		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			t.Context(), c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(t.Context()),
			func(s *agentsv1alpha1.Sandbox) (bool, error) {
				if s.Status.Phase == agentsv1alpha1.SandboxRunning {
					return false, assert.AnError
				}
				return false, nil
			},
		)
		err = task.Wait(2 * time.Second)
		require.Error(t, err)
		assert.Contains(t, err.Error(), assert.AnError.Error())
	})

	t.Run("fallback to apiserver when sandbox not in cache index", func(t *testing.T) {
		// Create sandbox with LabelSandboxIsClaimed = "false"
		// This won't be indexed by IndexClaimedSandboxID, so GetClaimedSandbox fails,
		// triggering apiserver fallback
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wait-sbx-fallback", Namespace: "default",
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxIsClaimed: "false",
				},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
		}
		c, fc, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)

		// Update sandbox to Running via fake client after a short delay
		go func() {
			time.Sleep(50 * time.Millisecond)
			fresh := &agentsv1alpha1.Sandbox{}
			_ = fc.Get(t.Context(), ctrlclient.ObjectKeyFromObject(sbx), fresh)
			fresh.Status.Phase = agentsv1alpha1.SandboxRunning
			fresh.Status.Conditions = []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			}
			_ = fc.Status().Update(t.Context(), fresh)

			// Close waitHook to trigger double-check
			key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
			if val, ok := c.GetWaitHooks().Load(key); ok {
				entry := val.(*cacheutils.WaitEntry[*agentsv1alpha1.Sandbox])
				entry.Close()
			}
		}()

		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			t.Context(), c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(t.Context()),
			func(s *agentsv1alpha1.Sandbox) (bool, error) {
				return s.Status.Phase == agentsv1alpha1.SandboxRunning &&
					sandboxutils.IsSandboxReady(s), nil
			},
		)
		err = task.Wait(2 * time.Second)
		require.NoError(t, err)
	})
}

func TestCache_WaitForSandbox_Cancel(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wait-cancel-sbx", Namespace: "default",
			Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
	}

	t.Run("context already canceled", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			ctx, c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(ctx),
			func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		)
		err = task.Wait(time.Hour)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not satisfied")
	})

	t.Run("context timeout", func(t *testing.T) {
		c, _, err := cachetest.NewTestCache(t, sbx)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		task := cachetest.NewAdHocTask[*agentsv1alpha1.Sandbox](
			ctx, c.GetWaitHooks(), "", sbx,
			c.SandboxUpdateFunc(ctx),
			func(s *agentsv1alpha1.Sandbox) (bool, error) { return false, nil },
		)
		err = task.Wait(time.Hour)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not satisfied")
	})
}

func TestCache_WaitForCheckpoint_AdHoc(t *testing.T) {
	t.Run("already satisfied", func(t *testing.T) {
		cp := &agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wait-cp-ok", Namespace: "default"},
			Status:     agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-done"},
		}
		c, _, err := cachetest.NewTestCache(t, cp)
		require.NoError(t, err)
		task := cachetest.NewAdHocTask[*agentsv1alpha1.Checkpoint](
			t.Context(), c.GetWaitHooks(), "", cp,
			c.CheckpointUpdateFunc(t.Context()),
			func(cp *agentsv1alpha1.Checkpoint) (bool, error) { return true, nil },
		)
		err = task.Wait(time.Second)
		require.NoError(t, err)
		got := &agentsv1alpha1.Checkpoint{}
		require.NoError(t, c.GetClient().Get(t.Context(), ctrlclient.ObjectKeyFromObject(cp), got))
		require.NotNil(t, got)
		assert.Equal(t, "wait-cp-ok", got.Name)
	})

	t.Run("satisfied after signal", func(t *testing.T) {
		cp := &agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wait-cp-signal", Namespace: "default"},
			Status:     agentsv1alpha1.CheckpointStatus{Phase: agentsv1alpha1.CheckpointPending},
		}
		c, fc, err := cachetest.NewTestCache(t, cp)
		require.NoError(t, err)

		go func() {
			time.Sleep(50 * time.Millisecond)
			fresh := &agentsv1alpha1.Checkpoint{}
			_ = fc.Get(t.Context(), ctrlclient.ObjectKeyFromObject(cp), fresh)
			fresh.Status.Phase = agentsv1alpha1.CheckpointSucceeded
			fresh.Status.CheckpointId = "cp-ready-id"
			_ = fc.Status().Update(t.Context(), fresh)

			key := cacheutils.WaitHookKey[*agentsv1alpha1.Checkpoint](cp)
			if val, ok := c.GetWaitHooks().Load(key); ok {
				entry := val.(*cacheutils.WaitEntry[*agentsv1alpha1.Checkpoint])
				entry.Close()
			}
		}()

		task := cachetest.NewAdHocTask[*agentsv1alpha1.Checkpoint](
			t.Context(), c.GetWaitHooks(), "", cp,
			c.CheckpointUpdateFunc(t.Context()),
			func(cp *agentsv1alpha1.Checkpoint) (bool, error) {
				return cp.Status.Phase == agentsv1alpha1.CheckpointSucceeded, nil
			},
		)
		err = task.Wait(2 * time.Second)
		require.NoError(t, err)
		got := &agentsv1alpha1.Checkpoint{}
		require.NoError(t, c.GetClient().Get(t.Context(), ctrlclient.ObjectKeyFromObject(cp), got))
		require.NotNil(t, got)
		assert.Equal(t, agentsv1alpha1.CheckpointSucceeded, got.Status.Phase)
	})

	t.Run("timeout returns error", func(t *testing.T) {
		cp := &agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wait-cp-timeout", Namespace: "default"},
			Status:     agentsv1alpha1.CheckpointStatus{Phase: agentsv1alpha1.CheckpointPending},
		}
		c, _, err := cachetest.NewTestCache(t, cp)
		require.NoError(t, err)
		task := cachetest.NewAdHocTask[*agentsv1alpha1.Checkpoint](
			t.Context(), c.GetWaitHooks(), "", cp,
			c.CheckpointUpdateFunc(t.Context()),
			func(cp *agentsv1alpha1.Checkpoint) (bool, error) { return false, nil },
		)
		err = task.Wait(100 * time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not satisfied")
	})
}

func TestRegression_SameActionConcurrentWaitsConverge(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "regression-sbx",
			Namespace: "default",
			Labels: map[string]string{
				agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
			},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
	}
	c, fc, err := cachetest.NewTestCache(t, sbx)
	require.NoError(t, err)

	errCh := make(chan error, 2)
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			<-start
			errCh <- c.NewSandboxPauseTask(t.Context(), sbx).Wait(3 * time.Second)
		}()
	}
	close(start)
	time.Sleep(50 * time.Millisecond)

	fresh := &agentsv1alpha1.Sandbox{}
	require.NoError(t, fc.Get(t.Context(), ctrlclient.ObjectKeyFromObject(sbx), fresh))
	fresh.Status.Conditions = []metav1.Condition{{
		Type:   string(agentsv1alpha1.SandboxConditionPaused),
		Status: metav1.ConditionTrue,
	}}
	require.NoError(t, fc.Status().Update(t.Context(), fresh))

	key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
	if val, ok := c.GetWaitHooks().Load(key); ok {
		val.(*cacheutils.WaitEntry[*agentsv1alpha1.Sandbox]).Close()
	}

	wg.Wait()
	close(errCh)
	for e := range errCh {
		assert.NoError(t, e, "both concurrent waiters must observe satisfied")
	}
}

// --- Extension getter tests ---

func TestCache_GetSandboxController(t *testing.T) {
	c, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	// Controllers are initialized by SetupCacheControllersWithManager in NewCache
	sbxCtrl := c.GetSandboxController()
	assert.NotNil(t, sbxCtrl, "sandbox controller should be initialized in NewCache")
}

func TestCache_GetSandboxSetController(t *testing.T) {
	c, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)
	// Controllers are initialized by SetupCacheControllersWithManager in NewCache
	sbsCtrl := c.GetSandboxSetController()
	assert.NotNil(t, sbsCtrl, "sandboxset controller should be initialized in NewCache")
}

// --- BuildCacheConfig tests ---

// getConfigByType retrieves the ByObject config for a given object type by iterating the map.
// This is needed because map[ctrlclient.Object] uses pointer comparison for keys,
// so direct lookup with a new instance fails even for the same type.
func getConfigByType(byObject map[ctrlclient.Object]ctrlcache.ByObject, obj ctrlclient.Object) (ctrlcache.ByObject, bool) {
	targetType := reflect.TypeOf(obj)
	for key, cfg := range byObject {
		if reflect.TypeOf(key) == targetType {
			return cfg, true
		}
	}
	return ctrlcache.ByObject{}, false
}

// countTypesByCategory counts the number of entries in byObject for each resource category.
func countTypesByCategory(byObject map[ctrlclient.Object]ctrlcache.ByObject) (custom, system, clusterScoped int) {
	customTypes := []reflect.Type{
		reflect.TypeOf(&agentsv1alpha1.Sandbox{}),
		reflect.TypeOf(&agentsv1alpha1.SandboxSet{}),
		reflect.TypeOf(&agentsv1alpha1.Checkpoint{}),
		reflect.TypeOf(&agentsv1alpha1.SandboxTemplate{}),
	}
	systemTypes := []reflect.Type{
		reflect.TypeOf(&corev1.Secret{}),
		reflect.TypeOf(&corev1.ConfigMap{}),
	}
	clusterScopedTypes := []reflect.Type{
		reflect.TypeOf(&corev1.PersistentVolume{}),
	}

	for key := range byObject {
		keyType := reflect.TypeOf(key)
		for _, t := range customTypes {
			if keyType == t {
				custom++
				break
			}
		}
		for _, t := range systemTypes {
			if keyType == t {
				system++
				break
			}
		}
		for _, t := range clusterScopedTypes {
			if keyType == t {
				clusterScoped++
				break
			}
		}
	}
	return custom, system, clusterScoped
}

func TestBuildCacheConfig(t *testing.T) {
	tests := []struct {
		name            string
		opts            config.SandboxManagerOptions
		wantErr         bool
		wantErrMsg      string
		wantCustom      int    // expected count of custom resource types (Sandbox, SandboxSet, Checkpoint, SandboxTemplate)
		wantSystem      int    // expected count of system namespace resource types (Secret, ConfigMap)
		wantPV          int    // expected count of cluster-scoped resource types (PersistentVolume)
		wantCustomNs    string // expected namespace for custom resources (empty means no filter)
		wantCustomLabel string // expected label selector string (empty means no filter)
		wantSystemNs    string // expected namespace for system resources (empty means no entry)
	}{
		{
			name:            "empty options - only custom resources and PersistentVolume",
			opts:            config.SandboxManagerOptions{},
			wantErr:         false,
			wantCustom:      4, // Sandbox, SandboxSet, Checkpoint, SandboxTemplate
			wantSystem:      0,
			wantPV:          1,
			wantCustomNs:    "",
			wantCustomLabel: "",
			wantSystemNs:    "",
		},
		{
			name: "SandboxNamespace only - custom resources filtered by namespace",
			opts: config.SandboxManagerOptions{
				SandboxNamespace: "team-a",
			},
			wantErr:         false,
			wantCustom:      4,
			wantSystem:      0,
			wantPV:          1,
			wantCustomNs:    "team-a",
			wantCustomLabel: "",
			wantSystemNs:    "",
		},
		{
			name: "valid SandboxLabelSelector only - custom resources filtered by label",
			opts: config.SandboxManagerOptions{
				SandboxLabelSelector: "env=prod",
			},
			wantErr:         false,
			wantCustom:      4,
			wantSystem:      0,
			wantPV:          1,
			wantCustomNs:    "",
			wantCustomLabel: "env=prod",
			wantSystemNs:    "",
		},
		{
			name:       "invalid SandboxLabelSelector - returns parse error",
			opts:       config.SandboxManagerOptions{SandboxLabelSelector: "invalid!!!selector"},
			wantErr:    true,
			wantErrMsg: "failed to parse sandbox label selector",
		},
		{
			name: "SandboxNamespace and valid SandboxLabelSelector - combined filtering",
			opts: config.SandboxManagerOptions{
				SandboxNamespace:     "team-a",
				SandboxLabelSelector: "env=prod",
			},
			wantErr:         false,
			wantCustom:      4,
			wantSystem:      0,
			wantPV:          1,
			wantCustomNs:    "team-a",
			wantCustomLabel: "env=prod",
			wantSystemNs:    "",
		},
		{
			name: "SystemNamespace only - adds Secret and ConfigMap filtering",
			opts: config.SandboxManagerOptions{
				SystemNamespace: "sandbox-system",
			},
			wantErr:         false,
			wantCustom:      4,
			wantSystem:      2, // Secret + ConfigMap
			wantPV:          1,
			wantCustomNs:    "",
			wantCustomLabel: "",
			wantSystemNs:    "sandbox-system",
		},
		{
			name: "SystemNamespace with SandboxNamespace - both filtering",
			opts: config.SandboxManagerOptions{
				SystemNamespace:  "sandbox-system",
				SandboxNamespace: "team-a",
			},
			wantErr:         false,
			wantCustom:      4,
			wantSystem:      2,
			wantPV:          1,
			wantCustomNs:    "team-a",
			wantCustomLabel: "",
			wantSystemNs:    "sandbox-system",
		},
		{
			name: "all options configured - full filtering",
			opts: config.SandboxManagerOptions{
				SystemNamespace:      "sandbox-system",
				SandboxNamespace:     "team-a",
				SandboxLabelSelector: "env=prod",
			},
			wantErr:         false,
			wantCustom:      4,
			wantSystem:      2,
			wantPV:          1,
			wantCustomNs:    "team-a",
			wantCustomLabel: "env=prod",
			wantSystemNs:    "sandbox-system",
		},
		{
			name:            "complex valid label selector",
			opts:            config.SandboxManagerOptions{SandboxLabelSelector: "app=myapp,version in (v1,v2),!deprecated"},
			wantErr:         false,
			wantCustom:      4,
			wantSystem:      0,
			wantPV:          1,
			wantCustomLabel: "app=myapp", // Partial match - selector is normalized internally
			wantCustomNs:    "",
			wantSystemNs:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			byObject, err := cache.BuildCacheConfig(tt.opts)

			if tt.wantErr {
				require.Error(t, err)
				if tt.wantErrMsg != "" {
					assert.Contains(t, err.Error(), tt.wantErrMsg)
				}
				return
			}

			require.NoError(t, err)
			require.NotNil(t, byObject)

			// Verify entry counts by category
			custom, system, clusterScoped := countTypesByCategory(byObject)
			assert.Equal(t, tt.wantCustom, custom, "custom resource count mismatch")
			assert.Equal(t, tt.wantSystem, system, "system resource count mismatch")
			assert.Equal(t, tt.wantPV, clusterScoped, "cluster-scoped resource count mismatch")

			// Verify custom resources (Sandbox, SandboxSet, Checkpoint, SandboxTemplate)
			customObjs := []ctrlclient.Object{
				&agentsv1alpha1.Sandbox{},
				&agentsv1alpha1.SandboxSet{},
				&agentsv1alpha1.Checkpoint{},
				&agentsv1alpha1.SandboxTemplate{},
			}
			for _, obj := range customObjs {
				cfg, ok := getConfigByType(byObject, obj)
				require.True(t, ok, "missing config for %T", obj)

				// Verify namespace filter for custom resources
				if tt.wantCustomNs != "" {
					require.NotNil(t, cfg.Namespaces, "Namespaces should be set for %T", obj)
					assert.Len(t, cfg.Namespaces, 1, "Namespaces should have exactly one entry for %T", obj)
					_, nsOk := cfg.Namespaces[tt.wantCustomNs]
					assert.True(t, nsOk, "namespace %s should be in Namespaces for %T", tt.wantCustomNs, obj)
				} else {
					assert.Nil(t, cfg.Namespaces, "Namespaces should be nil for %T when no SandboxNamespace", obj)
				}

				// Verify label selector for custom resources
				if tt.wantCustomLabel != "" {
					require.NotNil(t, cfg.Label, "Label selector should be set for %T", obj)
					// For complex selectors, use Contains because String() normalizes the order
					labelStr := cfg.Label.String()
					assert.Contains(t, labelStr, tt.wantCustomLabel, "Label selector should contain %s for %T", tt.wantCustomLabel, obj)
				} else {
					assert.Nil(t, cfg.Label, "Label selector should be nil for %T when no SandboxLabelSelector", obj)
				}

				// UnsafeDisableDeepCopy is handled globally via DefaultUnsafeDisableDeepCopy,
				// so per-object config should be nil.
				assert.Nil(t, cfg.UnsafeDisableDeepCopy, "UnsafeDisableDeepCopy should be nil for %T (handled by DefaultUnsafeDisableDeepCopy)", obj)
			}

			// Verify system namespace resources (Secret, ConfigMap)
			if tt.wantSystemNs != "" {
				for _, obj := range []ctrlclient.Object{&corev1.Secret{}, &corev1.ConfigMap{}} {
					cfg, ok := getConfigByType(byObject, obj)
					require.True(t, ok, "missing config for %T", obj)
					require.NotNil(t, cfg.Namespaces, "Namespaces should be set for %T", obj)
					assert.Len(t, cfg.Namespaces, 1, "Namespaces should have exactly one entry for %T", obj)
					_, nsOk := cfg.Namespaces[tt.wantSystemNs]
					assert.True(t, nsOk, "namespace %s should be in Namespaces for %T", tt.wantSystemNs, obj)
					assert.Nil(t, cfg.UnsafeDisableDeepCopy, "UnsafeDisableDeepCopy should be nil for %T (handled by DefaultUnsafeDisableDeepCopy)", obj)
				}
			} else {
				// When no SystemNamespace, Secret and ConfigMap should not be present
				_, secretOk := getConfigByType(byObject, &corev1.Secret{})
				assert.False(t, secretOk, "Secret should not be in byObject when no SystemNamespace")
				_, cmOk := getConfigByType(byObject, &corev1.ConfigMap{})
				assert.False(t, cmOk, "ConfigMap should not be in byObject when no SystemNamespace")
			}

			// Verify cluster-scoped resource (PersistentVolume) - always present
			pvCfg, pvOk := getConfigByType(byObject, &corev1.PersistentVolume{})
			require.True(t, pvOk, "PersistentVolume should always be in byObject")
			// PersistentVolume is cluster-scoped, should have no namespace filter
			assert.Nil(t, pvCfg.Namespaces, "PersistentVolume should have no namespace filter")
			assert.Nil(t, pvCfg.Label, "PersistentVolume should have no label filter")
			assert.Nil(t, pvCfg.UnsafeDisableDeepCopy, "UnsafeDisableDeepCopy should be nil for PersistentVolume (handled by DefaultUnsafeDisableDeepCopy)")
		})
	}
}

func boolPtr(b bool) *bool { return &b }
