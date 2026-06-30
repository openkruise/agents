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
		// Pre-create a PVC owned by a different user
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
		// Pre-create a bound PVC owned by the same user
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
			Namespace:    "sandbox-system",
			Name:         "bound-volume",
			UserID:       "user-1",
			StorageSize:  resource.MustParse("1Gi"),
			StorageClass: "standard",
			AccessMode:   "ReadWriteOnce",
		}
		info, err := i.CreateVolume(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, "bound-volume", info.Name)
		assert.Equal(t, "pv-bound-volume", info.VolumeID)
	})

	t.Run("PVC binding timeout returns error with PVC name", func(t *testing.T) {
		// Create a pending PVC that won't bind
		storageClass := "standard"
		pendingPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pending-volume",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-1",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
				},
				StorageClassName: &storageClass,
			},
			Status: corev1.PersistentVolumeClaimStatus{
				Phase: corev1.ClaimPending,
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), pendingPVC))

		opts := infra.CreateVolumeOptions{
			Namespace:        "sandbox-system",
			Name:             "pending-volume",
			UserID:           "user-1",
			StorageSize:      resource.MustParse("1Gi"),
			StorageClass:     "standard",
			AccessMode:       "ReadWriteOnce",
			WaitBoundTimeout: 100 * time.Millisecond,
		}
		_, err := i.CreateVolume(context.Background(), opts)
		assert.Error(t, err)
		// Verify error message contains PVC namespace/name and timeout info
		assert.Contains(t, err.Error(), "sandbox-system/pending-volume")
		assert.Contains(t, err.Error(), "timed out")
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
		assert.Contains(t, err.Error(), "volume not found")
	})

	t.Run("volume found regardless of ownership", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvc-other-owner",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "other-user",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-other",
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
		require.NoError(t, fakeClient.Create(context.Background(), pvc))

		opts := infra.GetVolumeOptions{
			Namespace: "sandbox-system",
			VolumeID:  "pv-other",
			UserID:    "user-1",
		}
		info, err := i.GetVolume(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, "pvc-other-owner", info.Name)
		assert.Equal(t, "pv-other", info.VolumeID)
	})

	t.Run("volume found and owned by requesting user", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvc-owned",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-1",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-owned",
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
		require.NoError(t, fakeClient.Create(context.Background(), pvc))

		opts := infra.GetVolumeOptions{
			Namespace: "sandbox-system",
			VolumeID:  "pv-owned",
			UserID:    "user-1",
		}
		info, err := i.GetVolume(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, "pvc-owned", info.Name)
		assert.Equal(t, "pv-owned", info.VolumeID)
	})

	t.Run("volume found with empty UserID skips ownership check", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvc-no-user-check",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "other-user",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-no-user-check",
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
		require.NoError(t, fakeClient.Create(context.Background(), pvc))

		opts := infra.GetVolumeOptions{
			Namespace: "sandbox-system",
			VolumeID:  "pv-no-user-check",
			UserID:    "",
		}
		info, err := i.GetVolume(context.Background(), opts)
		require.NoError(t, err)
		assert.Equal(t, "pvc-no-user-check", info.Name)
	})
}

func TestDeleteVolume(t *testing.T) {
	c, fakeClient, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	i := &Infra{Cache: c}

	t.Run("volume not found", func(t *testing.T) {
		opts := infra.DeleteVolumeOptions{
			Namespace: "sandbox-system",
			VolumeID:  "non-existent-pv",
			UserID:    "user-1",
		}
		err := i.DeleteVolume(context.Background(), opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "volume not found")
	})

	t.Run("volume deleted regardless of ownership", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvc-del-other",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "other-user",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-del-other",
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
			VolumeID:  "pv-del-other",
			UserID:    "user-1",
		}
		err := i.DeleteVolume(context.Background(), opts)
		assert.NoError(t, err)
	})

	t.Run("volume deleted successfully by owner", func(t *testing.T) {
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
			VolumeID:  "pv-del-ok",
			UserID:    "user-1",
		}
		err := i.DeleteVolume(context.Background(), opts)
		assert.NoError(t, err)
	})

	t.Run("volume deleted with empty UserID skips ownership check", func(t *testing.T) {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pvc-del-no-user",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "other-user",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-del-no-user",
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
			VolumeID:  "pv-del-no-user",
			UserID:    "",
		}
		err := i.DeleteVolume(context.Background(), opts)
		assert.NoError(t, err)
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
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-1",
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("1Gi"),
					},
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
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-2",
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("2Gi"),
					},
				},
			},
		}
		pvc3 := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "vol-3",
				Namespace: "sandbox-system",
				Annotations: map[string]string{
					agentsv1alpha1.AnnotationOwner: "user-2",
				},
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				VolumeName:  "pv-3",
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceStorage: resource.MustParse("3Gi"),
					},
				},
			},
		}
		require.NoError(t, fakeClient.Create(context.Background(), pvc1))
		require.NoError(t, fakeClient.Create(context.Background(), pvc2))
		require.NoError(t, fakeClient.Create(context.Background(), pvc3))

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

	t.Run("list volumes returns empty for user with no volumes", func(t *testing.T) {
		opts := infra.ListVolumesOptions{
			Namespace: "sandbox-system",
			UserID:    "user-no-volumes",
		}
		volumes, err := i.ListVolumes(context.Background(), opts)
		require.NoError(t, err)
		assert.Len(t, volumes, 0)
	})
}
