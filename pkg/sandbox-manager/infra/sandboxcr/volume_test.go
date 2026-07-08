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

package sandboxcr

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache/cachetest"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestCreateVolume(t *testing.T) {
	c, fakeClient, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	// Create a valid StorageClass
	immediateBinding := storagev1.VolumeBindingImmediate
	sc := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "standard"},
		Provisioner:       "kubernetes.io/no-provisioner",
		VolumeBindingMode: &immediateBinding,
	}
	require.NoError(t, fakeClient.Create(context.Background(), sc))

	i := &Infra{Cache: c}

	t.Run("PVC already exists owned by different user", func(t *testing.T) {
		existingPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "existing-volume",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "other-user",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), existingPVC))

		opts := infra.CreateVolumeOptions{
			Namespace:        "sandbox-system",
			Name:             "existing-volume",
			UserID:           "user-1",
			StorageSize:      resource.MustParse("1Gi"),
			StorageClass:     "standard",
			AccessMode:       "ReadWriteOnce",
			WaitBoundTimeout: 1 * time.Second,
		}
		_, err := i.CreateVolume(context.Background(), opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "owned by another user")
	})

	t.Run("PVC already exists and bound by same user (idempotent)", func(t *testing.T) {
		boundPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "bound-volume",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-1",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-bound-volume",
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimBound,
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), boundPVC))

		opts := infra.CreateVolumeOptions{
			Namespace:        "sandbox-system",
			Name:             "bound-volume",
			UserID:           "user-1",
			StorageSize:      resource.MustParse("1Gi"),
			StorageClass:     "standard",
			AccessMode:       "ReadWriteOnce",
			WaitBoundTimeout: 1 * time.Second,
		}
		info, err := i.CreateVolume(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, "bound-volume", info.Name)
		assert.Equal(t, "bound-volume", info.VolumeID)
		assert.Equal(t, "pv-bound-volume", info.PvName)
	})
}

func TestListVolumes(t *testing.T) {
	c, fakeClient, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	i := &Infra{Cache: c}

	t.Run("list volumes for a user", func(t *testing.T) {
		pvc1 := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vol-1",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-1",
				},
			},
		}
		pvc2 := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vol-2",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-1",
				},
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), pvc1))
		require.NoError(t, fakeClient.Create(context.Background(), pvc2))

		opts := infra.ListVolumesOptions{
			Namespace: "sandbox-system",
			UserID:    "user-1",
		}
		volumes, err := i.ListVolumes(context.Background(), opts)
		require.NoError(t, err)
		assert.Len(t, volumes, 2)
		names := make(map[string]bool)
		for _, v := range volumes {
			names[v.Name] = true
		}
		assert.True(t, names["vol-1"])
		assert.True(t, names["vol-2"])
	})
}

func TestGetVolume(t *testing.T) {
	c, fakeClient, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	i := &Infra{Cache: c}

	t.Run("volume not found", func(t *testing.T) {
		opts := infra.GetVolumeOptions{
			Namespace: "sandbox-system",
			VolumeID:  "non-existent-pv",
			UserID:    "user-1",
		}
		_, err := i.GetVolume(context.Background(), opts)
		assert.Error(t, err)
	})

	t.Run("volume found", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvc-get-ok",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-1",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-get-ok",
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), pvc))

		opts := infra.GetVolumeOptions{
			Namespace: "sandbox-system",
			VolumeID:  "pvc-get-ok",
			UserID:    "user-1",
		}
		info, err := i.GetVolume(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, "pvc-get-ok", info.Name)
		assert.Equal(t, "pvc-get-ok", info.VolumeID)
		assert.Equal(t, "pv-get-ok", info.PvName)
	})
}

func TestDeleteVolume(t *testing.T) {
	c, fakeClient, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	i := &Infra{Cache: c, APIReader: fakeClient}

	t.Run("volume not found", func(t *testing.T) {
		opts := infra.DeleteVolumeOptions{
			Namespace: "sandbox-system",
			VolumeID:  "non-existent-pvc",
			UserID:    "user-1",
		}
		_, err := i.DeleteVolume(context.Background(), opts)
		assert.Error(t, err)
	})

	t.Run("volume deleted successfully", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvc-del-ok",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-1",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-del-ok",
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), pvc))

		opts := infra.DeleteVolumeOptions{
			Namespace: "sandbox-system",
			VolumeID:  "pvc-del-ok",
			UserID:    "user-1",
		}
		result, err := i.DeleteVolume(context.Background(), opts)
		require.NoError(t, err)
		assert.Empty(t, result.AffectedSandboxIDs)
	})

	t.Run("delete volume blocked by SandboxClaim", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvc-del-blocked",
				Namespace: "ns1",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-1",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-del-blocked",
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), pvc))

		// Create claim and sandbox mounting this PV
		claim := &agentsv1alpha1.SandboxClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "claim1", Namespace: "ns1"},
			Spec: agentsv1alpha1.SandboxClaimSpec{
				TemplateName: "test-template",
				DynamicVolumesMount: []agentsv1alpha1.CSIMountConfig{
					{PvName: "pv-del-blocked", MountPath: "/data"},
				},
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), claim))

		sbx := &agentsv1alpha1.Sandbox{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "sbx1",
				Namespace: "ns1",
				Labels: map[string]string{
					agentsv1alpha1.LabelSandboxIsClaimed: agentsv1alpha1.True,
					agentsv1alpha1.LabelSandboxClaimName: "claim1",
				},
			},
			Status: agentsv1alpha1.SandboxStatus{Phase: agentsv1alpha1.SandboxRunning},
		}
		CreateSandboxWithStatus(t, fakeClient, sbx)

		opts := infra.DeleteVolumeOptions{
			Namespace: "ns1",
			VolumeID:  "pvc-del-blocked",
			UserID:    "user-1",
			Force:     false,
		}
		_, err := i.DeleteVolume(context.Background(), opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "volume is mounted by")

		// Deleting with Force=true should succeed
		opts.Force = true
		res, err := i.DeleteVolume(context.Background(), opts)
		require.NoError(t, err)
		assert.Contains(t, res.AffectedSandboxIDs, "ns1--sbx1")
		assert.True(t, res.ForcedDelete)
	})
}
