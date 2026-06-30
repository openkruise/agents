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
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
)

func TestValidateAccessMode(t *testing.T) {
	tests := []struct {
		name        string
		accessMode  string
		expectError string
	}{
		{name: "ReadWriteOnce is valid", accessMode: "ReadWriteOnce", expectError: ""},
		{name: "ReadOnlyMany is valid", accessMode: "ReadOnlyMany", expectError: ""},
		{name: "ReadWriteMany is valid", accessMode: "ReadWriteMany", expectError: ""},
		{name: "ReadWriteOncePod is valid", accessMode: "ReadWriteOncePod", expectError: ""},
		{name: "invalid access mode", accessMode: "InvalidMode", expectError: "unsupported access mode"},
		{name: "empty access mode", accessMode: "", expectError: "unsupported access mode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAccessMode(tt.accessMode)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

// --- validateCreateVolumeOptions tests (needs Infra with Cache for StorageClass validation) ---

func TestValidateCreateVolumeOptions(t *testing.T) {
	c, _, err := cachetest.NewTestCache(t)
	require.NoError(t, err)

	// Create a valid StorageClass for positive test cases
	immediateBinding := storagev1.VolumeBindingImmediate
	sc := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "standard"},
		Provisioner:       "kubernetes.io/no-provisioner",
		VolumeBindingMode: &immediateBinding,
	}
	require.NoError(t, c.GetClient().Create(context.Background(), sc))

	// Create a WaitForFirstConsumer StorageClass for binding mode test
	waitBinding := storagev1.VolumeBindingWaitForFirstConsumer
	wfscStorageClass := &storagev1.StorageClass{
		ObjectMeta:        metav1.ObjectMeta{Name: "wait-first-consumer"},
		Provisioner:       "kubernetes.io/no-provisioner",
		VolumeBindingMode: &waitBinding,
	}
	require.NoError(t, c.GetClient().Create(context.Background(), wfscStorageClass))

	i := &Infra{Cache: c}

	tests := []struct {
		name        string
		opts        infra.CreateVolumeOptions
		expectError string
	}{
		{
			name: "valid options",
			opts: infra.CreateVolumeOptions{
				Namespace:    "sandbox-system",
				Name:         "test-volume",
				UserID:       "user-1",
				StorageSize:  resource.MustParse("1Gi"),
				StorageClass: "standard",
				AccessMode:   "ReadWriteOnce",
			},
			expectError: "",
		},
		{
			name: "default WaitBoundTimeout is set when zero",
			opts: infra.CreateVolumeOptions{
				Namespace:        "sandbox-system",
				Name:             "test-volume",
				UserID:           "user-1",
				StorageSize:      resource.MustParse("1Gi"),
				StorageClass:     "standard",
				AccessMode:       "ReadWriteOnce",
				WaitBoundTimeout: 0,
			},
			expectError: "",
		},
		{
			name: "invalid volume name uppercase",
			opts: infra.CreateVolumeOptions{
				Namespace:    "sandbox-system",
				Name:         "Test-Volume-UPPERCASE",
				UserID:       "user-1",
				StorageSize:  resource.MustParse("1Gi"),
				StorageClass: "standard",
				AccessMode:   "ReadWriteOnce",
			},
			expectError: "invalid volume name",
		},
		{
			name: "invalid volume name special chars",
			opts: infra.CreateVolumeOptions{
				Namespace:    "sandbox-system",
				Name:         "test@volume#name",
				UserID:       "user-1",
				StorageSize:  resource.MustParse("1Gi"),
				StorageClass: "standard",
				AccessMode:   "ReadWriteOnce",
			},
			expectError: "invalid volume name",
		},
		{
			name: "invalid volume name too long (exceeds 63 chars)",
			opts: infra.CreateVolumeOptions{
				Namespace:    "sandbox-system",
				Name:         "this-name-is-way-too-long-for-a-dns1123-label-because-it-exceeds-sixty-three-characters",
				UserID:       "user-1",
				StorageSize:  resource.MustParse("1Gi"),
				StorageClass: "standard",
				AccessMode:   "ReadWriteOnce",
			},
			expectError: "must be no more than 63 characters",
		},
		{
			name: "empty volume name",
			opts: infra.CreateVolumeOptions{
				Namespace:    "sandbox-system",
				Name:         "",
				UserID:       "user-1",
				StorageSize:  resource.MustParse("1Gi"),
				StorageClass: "standard",
				AccessMode:   "ReadWriteOnce",
			},
			expectError: "invalid volume name",
		},
		{
			name: "invalid access mode",
			opts: infra.CreateVolumeOptions{
				Namespace:    "sandbox-system",
				Name:         "test-volume",
				UserID:       "user-1",
				StorageSize:  resource.MustParse("1Gi"),
				StorageClass: "standard",
				AccessMode:   "InvalidMode",
			},
			expectError: "invalid access mode",
		},
		{
			name: "storage class not found",
			opts: infra.CreateVolumeOptions{
				Namespace:    "sandbox-system",
				Name:         "test-volume",
				UserID:       "user-1",
				StorageSize:  resource.MustParse("1Gi"),
				StorageClass: "non-existent",
				AccessMode:   "ReadWriteOnce",
			},
			expectError: "storage class \"non-existent\" not found",
		},
		{
			name: "storage class with WaitForFirstConsumer binding mode",
			opts: infra.CreateVolumeOptions{
				Namespace:    "sandbox-system",
				Name:         "test-volume",
				UserID:       "user-1",
				StorageSize:  resource.MustParse("1Gi"),
				StorageClass: "wait-first-consumer",
				AccessMode:   "ReadWriteOnce",
			},
			expectError: "WaitForFirstConsumer binding mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := i.validateCreateVolumeOptions(context.Background(), &tt.opts)
			if tt.expectError == "" {
				assert.NoError(t, err)
				// Verify default timeout was applied for zero timeout case
				if tt.opts.WaitBoundTimeout == 0 && tt.name == "default WaitBoundTimeout is set when zero" {
					assert.Equal(t, consts.DefaultWaitBoundPVCTimeout, tt.opts.WaitBoundTimeout)
				}
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

// --- CreateVolume business logic tests ---

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

	t.Run("validation fails for invalid name", func(t *testing.T) {
		opts := infra.CreateVolumeOptions{
			Namespace:    "sandbox-system",
			Name:         "INVALID-NAME",
			UserID:       "user-1",
			StorageSize:  resource.MustParse("1Gi"),
			StorageClass: "standard",
			AccessMode:   "ReadWriteOnce",
		}
		_, err := i.CreateVolume(context.Background(), opts)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "validation failed")
	})

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

// --- GetVolume business logic tests ---

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

// --- DeleteVolume business logic tests ---

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

// --- ListVolumes business logic tests ---

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
