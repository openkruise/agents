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

package cache

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr/cache/controllers"
	cacheutils "github.com/openkruise/agents/pkg/sandbox-manager/infra/sandboxcr/cache/utils"
	"github.com/openkruise/agents/pkg/utils"
	"github.com/openkruise/agents/pkg/utils/sandboxutils"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestCacheLocal(t *testing.T, objs ...ctrlclient.Object) (*CacheV2, ctrlclient.Client, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(agentsv1alpha1.AddToScheme(scheme))

	builder := fake.NewClientBuilder().WithScheme(scheme)
	for _, idx := range GetIndexFuncs() {
		builder = builder.WithIndex(idx.Obj, idx.FieldName, idx.Extract)
	}
	builder = builder.WithStatusSubresource(&agentsv1alpha1.Sandbox{}, &agentsv1alpha1.Checkpoint{}, &agentsv1alpha1.SandboxClaim{})
	builder = builder.WithInterceptorFuncs(cacheutils.ResourceVersionInterceptorFuncs())
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	fakeClient := builder.Build()
	mgrBuilder, err := controllers.NewMockManagerBuilder(t)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create mock manager builder: %w", err)
	}
	mgr := mgrBuilder.
		WithScheme(scheme).
		WithClient(fakeClient).
		WithWaitSimulation().
		Build()
	c, err := NewCacheV2(mgr)
	if err != nil {
		return nil, nil, err
	}
	mgr.SetWaitHooks(c.GetWaitHooks())
	return c, fakeClient, nil
}

// --- Get method tests ---

func TestCacheV2_GetPersistentVolume(t *testing.T) {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pv"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("10Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/test-pv"},
			},
		},
	}

	t.Run("existing PV", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t, pv)
		require.NoError(t, err)
		got, err := c.GetPersistentVolume("test-pv")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "test-pv", got.Name)
		assert.Equal(t, pv.Spec.Capacity, got.Spec.Capacity)
	})

	t.Run("non-existing PV", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t, pv)
		require.NoError(t, err)
		got, err := c.GetPersistentVolume("non-existing-pv")
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "not found in cache")
	})
}

func TestCacheV2_GetPersistentVolume_FromSync(t *testing.T) {
	c, fc, err := newTestCacheLocal(t)
	require.NoError(t, err)

	// PV not there yet
	_, err = c.GetPersistentVolume("test-pv-sync")
	require.Error(t, err)

	// Create PV via fake client
	testPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pv-sync"},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("10Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				HostPath: &corev1.HostPathVolumeSource{Path: "/tmp/test-pv"},
			},
		},
	}
	require.NoError(t, fc.Create(context.Background(), testPV))

	// Now it should be found
	got, err := c.GetPersistentVolume("test-pv-sync")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "test-pv-sync", got.Name)
	assert.Equal(t, testPV.Spec.Capacity, got.Spec.Capacity)
}

func TestCacheV2_GetSecret_FromSync(t *testing.T) {
	c, fc, err := newTestCacheLocal(t)
	require.NoError(t, err)

	ns := utils.DefaultSandboxDeployNamespace

	// Not found initially
	_, err = c.GetSecret(ns, "test-secret")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in cache")

	// Create secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "test-secret", Namespace: ns},
		Data:       map[string][]byte{"username": []byte("admin"), "password": []byte("pass123")},
		Type:       corev1.SecretTypeOpaque,
	}
	require.NoError(t, fc.Create(context.Background(), secret))

	got, err := c.GetSecret(ns, "test-secret")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "test-secret", got.Name)
	assert.Equal(t, ns, got.Namespace)
	assert.Equal(t, secret.Data, got.Data)
	assert.Equal(t, secret.Type, got.Type)
}

func TestCacheV2_GetConfigmap_FromSync(t *testing.T) {
	c, fc, err := newTestCacheLocal(t)
	require.NoError(t, err)

	ns := utils.DefaultSandboxDeployNamespace

	// Not found returns (nil, nil)
	got, err := c.GetConfigmap(ns, "test-cm")
	require.NoError(t, err)
	assert.Nil(t, got)

	// Create configmap
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cm", Namespace: ns},
		Data:       map[string]string{"key1": "value1", "key2": "value2"},
	}
	require.NoError(t, fc.Create(context.Background(), cm))

	got, err = c.GetConfigmap(ns, "test-cm")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "test-cm", got.Name)
	assert.Equal(t, cm.Data, got.Data)
}

func TestCacheV2_GetSandboxTemplate(t *testing.T) {
	tmpl := &agentsv1alpha1.SandboxTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "my-tmpl", Namespace: "default"},
	}

	t.Run("existing template", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t, tmpl)
		require.NoError(t, err)
		got, err := c.GetSandboxTemplate("default", "my-tmpl")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "my-tmpl", got.Name)
	})

	t.Run("not found", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t)
		require.NoError(t, err)
		_, err = c.GetSandboxTemplate("default", "no-tmpl")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in cache")
	})
}

// --- Index query tests ---

func TestCacheV2_GetClaimedSandbox(t *testing.T) {
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
		c, _, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)
		sandboxID := sandboxutils.GetSandboxID(sbx)
		got, err := c.GetClaimedSandbox(sandboxID)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "test-sbx", got.Name)
	})

	t.Run("not found", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t)
		require.NoError(t, err)
		_, err = c.GetClaimedSandbox("nonexistent-id")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in cache")
	})
}

func TestCacheV2_GetCheckpoint(t *testing.T) {
	cp := &agentsv1alpha1.Checkpoint{
		ObjectMeta: metav1.ObjectMeta{Name: "test-cp", Namespace: "default"},
		Status:     agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-id-123"},
	}

	t.Run("found", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t, cp)
		require.NoError(t, err)
		got, err := c.GetCheckpoint("cp-id-123")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "test-cp", got.Name)
	})

	t.Run("not found", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t)
		require.NoError(t, err)
		_, err = c.GetCheckpoint("nonexistent-cp")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in cache")
	})
}

func TestCacheV2_GetSandboxSet(t *testing.T) {
	sbs := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-1", Namespace: "team-a"},
	}

	t.Run("found by name index", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t, sbs)
		require.NoError(t, err)
		got, err := c.GetSandboxSet("tmpl-1")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "tmpl-1", got.Name)
		assert.Equal(t, "team-a", got.Namespace)
	})

	t.Run("not found", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t)
		require.NoError(t, err)
		_, err = c.GetSandboxSet("nonexistent")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found in cache")
	})
}

func TestCacheV2_GetSandboxSet_MultipleTemplates(t *testing.T) {
	sbs1 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-1", Namespace: "team-a"},
	}
	sbs2 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-2", Namespace: "team-a"},
	}
	sbs3 := &agentsv1alpha1.SandboxSet{
		ObjectMeta: metav1.ObjectMeta{Name: "tmpl-3", Namespace: "team-b"},
	}

	c, _, err := newTestCacheLocal(t, sbs1, sbs2, sbs3)
	require.NoError(t, err)

	got, err := c.GetSandboxSet("tmpl-3")
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, "tmpl-3", got.Name)
	assert.Equal(t, "team-b", got.Namespace)

	_, err = c.GetSandboxSet("nonexistent")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found in cache")
}

// --- List tests ---

func TestCacheV2_ListSandboxWithUser(t *testing.T) {
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

	c, _, err := newTestCacheLocal(t, sbx1, sbx2, sbx3)
	require.NoError(t, err)

	list, err := c.ListSandboxWithUser("user-1")
	require.NoError(t, err)
	assert.Len(t, list, 2)

	list, err = c.ListSandboxWithUser("user-2")
	require.NoError(t, err)
	assert.Len(t, list, 1)

	list, err = c.ListSandboxWithUser("user-nobody")
	require.NoError(t, err)
	assert.Len(t, list, 0)
}

func TestCacheV2_ListCheckpointsWithUser(t *testing.T) {
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

	c, _, err := newTestCacheLocal(t, cp1, cp2)
	require.NoError(t, err)

	list, err := c.ListCheckpointsWithUser("user-1")
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "cp-1", list[0].Name)

	list, err = c.ListCheckpointsWithUser("user-nobody")
	require.NoError(t, err)
	assert.Len(t, list, 0)
}

func TestCacheV2_ListSandboxesInPool(t *testing.T) {
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

	c, _, err := newTestCacheLocal(t, sbx)
	require.NoError(t, err)

	list, err := c.ListSandboxesInPool("tmpl-a")
	require.NoError(t, err)
	assert.Len(t, list, 1)
	assert.Equal(t, "pool-sbx-1", list[0].Name)

	list, err = c.ListSandboxesInPool("tmpl-nonexistent")
	require.NoError(t, err)
	assert.Len(t, list, 0)
}

func TestCacheV2_ListAllSandboxes(t *testing.T) {
	sbx1 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "all-1", Namespace: "default"},
	}
	sbx2 := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{Name: "all-2", Namespace: "default"},
	}

	c, _, err := newTestCacheLocal(t, sbx1, sbx2)
	require.NoError(t, err)

	list := c.ListAllSandboxes()
	assert.Len(t, list, 2)
}

// --- Wait tests ---

func TestCacheV2_WaitForSandboxSatisfied(t *testing.T) {
	t.Run("already satisfied", func(t *testing.T) {
		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name: "wait-sbx-satisfied", Namespace: "default",
				Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
		}
		c, _, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)
		err = c.WaitForSandboxSatisfied(t.Context(), sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return true, nil
		}, time.Second)
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
		c, _, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)
		err = c.WaitForSandboxSatisfied(t.Context(), sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return false, assert.AnError
		}, time.Second)
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
		c, _, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)
		err = c.WaitForSandboxSatisfied(t.Context(), sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return false, nil
		}, 100*time.Millisecond)
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
		c, _, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)

		// Start a long-running wait with action "another"
		go func() {
			_ = c.WaitForSandboxSatisfied(t.Context(), sbx, "another", func(s *agentsv1alpha1.Sandbox) (bool, error) {
				return false, nil // never satisfied
			}, time.Hour)
		}()
		// Give the goroutine time to register the wait hook
		time.Sleep(50 * time.Millisecond)

		// Try a different action on same sandbox - should conflict
		err = c.WaitForSandboxSatisfied(t.Context(), sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return false, nil
		}, time.Second)
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
		c, fc, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)

		// Update sandbox in background to satisfy condition, and close waitHook
		go func() {
			time.Sleep(50 * time.Millisecond)
			// Update the sandbox to Running
			fresh := &agentsv1alpha1.Sandbox{}
			_ = fc.Get(context.Background(), ctrlclient.ObjectKeyFromObject(sbx), fresh)
			fresh.Status.Phase = agentsv1alpha1.SandboxRunning
			_ = fc.Status().Update(context.Background(), fresh)

			// Manually trigger waitHook (fake client has no informer)
			key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
			if val, ok := c.waitHooks.Load(key); ok {
				entry := val.(*cacheutils.WaitEntry[*agentsv1alpha1.Sandbox])
				entry.Close()
			}
		}()

		err = c.WaitForSandboxSatisfied(t.Context(), sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return s.Status.Phase == agentsv1alpha1.SandboxRunning, nil
		}, 2*time.Second)
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
		c, fc, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)

		// Update and close waitHook so double-check runs
		go func() {
			time.Sleep(50 * time.Millisecond)
			fresh := &agentsv1alpha1.Sandbox{}
			_ = fc.Get(context.Background(), ctrlclient.ObjectKeyFromObject(sbx), fresh)
			fresh.Status.Phase = agentsv1alpha1.SandboxRunning
			_ = fc.Status().Update(context.Background(), fresh)

			key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
			if val, ok := c.waitHooks.Load(key); ok {
				entry := val.(*cacheutils.WaitEntry[*agentsv1alpha1.Sandbox])
				entry.Close()
			}
		}()

		err = c.WaitForSandboxSatisfied(t.Context(), sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			if s.Status.Phase == agentsv1alpha1.SandboxRunning {
				return false, assert.AnError
			}
			return false, nil
		}, 2*time.Second)
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
		c, fc, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)

		// Update sandbox to Running via fake client after a short delay
		go func() {
			time.Sleep(50 * time.Millisecond)
			fresh := &agentsv1alpha1.Sandbox{}
			_ = fc.Get(context.Background(), ctrlclient.ObjectKeyFromObject(sbx), fresh)
			fresh.Status.Phase = agentsv1alpha1.SandboxRunning
			fresh.Status.Conditions = []metav1.Condition{
				{Type: string(agentsv1alpha1.SandboxConditionReady), Status: metav1.ConditionTrue},
			}
			_ = fc.Status().Update(context.Background(), fresh)

			// Close waitHook to trigger double-check
			key := cacheutils.WaitHookKey[*agentsv1alpha1.Sandbox](sbx)
			if val, ok := c.waitHooks.Load(key); ok {
				entry := val.(*cacheutils.WaitEntry[*agentsv1alpha1.Sandbox])
				entry.Close()
			}
		}()

		err = c.WaitForSandboxSatisfied(t.Context(), sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return s.Status.Phase == agentsv1alpha1.SandboxRunning &&
				sandboxutils.IsSandboxReady(s), nil
		}, 2*time.Second)
		require.NoError(t, err)
	})
}

func TestCacheV2_WaitForSandboxSatisfied_Cancel(t *testing.T) {
	sbx := &agentsv1alpha1.Sandbox{
		ObjectMeta: metav1.ObjectMeta{
			Name: "wait-cancel-sbx", Namespace: "default",
			Labels: map[string]string{agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True},
		},
		Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxPending},
	}

	t.Run("context already canceled", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err = c.WaitForSandboxSatisfied(ctx, sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return false, nil
		}, time.Hour)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not satisfied")
	})

	t.Run("context timeout", func(t *testing.T) {
		c, _, err := newTestCacheLocal(t, sbx)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		err = c.WaitForSandboxSatisfied(ctx, sbx, "", func(s *agentsv1alpha1.Sandbox) (bool, error) {
			return false, nil
		}, time.Hour)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not satisfied")
	})
}

func TestCacheV2_WaitForCheckpointSatisfied(t *testing.T) {
	t.Run("already satisfied", func(t *testing.T) {
		cp := &agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wait-cp-ok", Namespace: "default"},
			Status:     agentsv1alpha1.CheckpointStatus{CheckpointId: "cp-done"},
		}
		c, _, err := newTestCacheLocal(t, cp)
		require.NoError(t, err)
		got, err := c.WaitForCheckpointSatisfied(t.Context(), cp, "", func(c *agentsv1alpha1.Checkpoint) (bool, error) {
			return true, nil
		}, time.Second)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "wait-cp-ok", got.Name)
	})

	t.Run("satisfied after signal", func(t *testing.T) {
		cp := &agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wait-cp-signal", Namespace: "default"},
			Status:     agentsv1alpha1.CheckpointStatus{Phase: agentsv1alpha1.CheckpointPending},
		}
		c, fc, err := newTestCacheLocal(t, cp)
		require.NoError(t, err)

		go func() {
			time.Sleep(50 * time.Millisecond)
			fresh := &agentsv1alpha1.Checkpoint{}
			_ = fc.Get(context.Background(), ctrlclient.ObjectKeyFromObject(cp), fresh)
			fresh.Status.Phase = agentsv1alpha1.CheckpointSucceeded
			fresh.Status.CheckpointId = "cp-ready-id"
			_ = fc.Status().Update(context.Background(), fresh)

			key := cacheutils.WaitHookKey[*agentsv1alpha1.Checkpoint](cp)
			if val, ok := c.waitHooks.Load(key); ok {
				entry := val.(*cacheutils.WaitEntry[*agentsv1alpha1.Checkpoint])
				entry.Close()
			}
		}()

		got, err := c.WaitForCheckpointSatisfied(t.Context(), cp, "", func(c *agentsv1alpha1.Checkpoint) (bool, error) {
			return c.Status.Phase == agentsv1alpha1.CheckpointSucceeded, nil
		}, 2*time.Second)
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, agentsv1alpha1.CheckpointSucceeded, got.Status.Phase)
	})

	t.Run("timeout returns error", func(t *testing.T) {
		cp := &agentsv1alpha1.Checkpoint{
			ObjectMeta: metav1.ObjectMeta{Name: "wait-cp-timeout", Namespace: "default"},
			Status:     agentsv1alpha1.CheckpointStatus{Phase: agentsv1alpha1.CheckpointPending},
		}
		c, _, err := newTestCacheLocal(t, cp)
		require.NoError(t, err)
		got, err := c.WaitForCheckpointSatisfied(t.Context(), cp, "", func(c *agentsv1alpha1.Checkpoint) (bool, error) {
			return false, nil
		}, 100*time.Millisecond)
		require.Error(t, err)
		assert.Nil(t, got)
		assert.Contains(t, err.Error(), "not satisfied")
	})
}

// --- Extension getter tests ---

func TestCacheV2_GetSandboxController(t *testing.T) {
	c, _, err := newTestCacheLocal(t)
	require.NoError(t, err)
	// Controllers are initialized by SetupCacheControllersWithManager in NewCacheV2
	sbxCtrl := c.GetSandboxController()
	assert.NotNil(t, sbxCtrl, "sandbox controller should be initialized in NewCacheV2")
}

func TestCacheV2_GetSandboxSetController(t *testing.T) {
	c, _, err := newTestCacheLocal(t)
	require.NoError(t, err)
	// Controllers are initialized by SetupCacheControllersWithManager in NewCacheV2
	sbsCtrl := c.GetSandboxSetController()
	assert.NotNil(t, sbsCtrl, "sandboxset controller should be initialized in NewCacheV2")
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
			byObject, err := BuildCacheConfig(tt.opts)

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
		})
	}
}

func boolPtr(b bool) *bool { return &b }
