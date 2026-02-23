package storages

import (
	"context"
	"testing"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMountProvider_GenerateNodePublishVolumeRequest(t *testing.T) {
	tests := []struct {
		name                 string
		containerMountTarget string
		persistentVolumeObj  *corev1.PersistentVolume
		secretObj            *corev1.Secret
		expectError          bool
		validateResult       func(*testing.T, *csiapi.NodePublishVolumeRequest)
	}{
		{
			name:                 "basic node publish volume request",
			containerMountTarget: "/var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/pvc-123/mount",
			persistentVolumeObj: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pv"},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:       "diskplugin.csi.alibabacloud.com",
							VolumeHandle: "d-2zeaxxxxxxxx",
							FSType:       "ext4",
							VolumeAttributes: map[string]string{
								"type":                             "ext4",
								"csi.storage.k8s.io/pv/name":       "pvc-123",
								"csi.storage.k8s.io/pvc/name":      "test-pvc",
								"csi.storage.k8s.io/pvc/namespace": "default",
							},
						},
					},
					MountOptions: []string{"rw", "noatime"},
					AccessModes:  []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			},
			secretObj:   nil,
			expectError: false,
			validateResult: func(t *testing.T, result *csiapi.NodePublishVolumeRequest) {
				assert.Contains(t, result.VolumeId, "test-pv")
				assert.Equal(t, "/var/lib/kubelet/pods/abc/volumes/kubernetes.io~csi/pvc-123/mount", result.TargetPath)
				assert.NotNil(t, result.VolumeCapability)
				assert.False(t, result.Readonly)
				assert.NotNil(t, result.VolumeContext)
				assert.Equal(t, "ext4", result.VolumeContext["type"])
			},
		},
		{
			name:                 "read-only volume",
			containerMountTarget: "/var/lib/kubelet/pods/def/volumes/kubernetes.io~csi/pvc-456/mount",
			persistentVolumeObj: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "ro-test-pv"},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:       "nfsplugin.csi.alibabacloud.com",
							VolumeHandle: "nfs-server.example.com:/export/data",
							FSType:       "nfs",
							VolumeAttributes: map[string]string{
								"type": "nfs",
							},
						},
					},
					MountOptions: []string{"ro"},
					AccessModes:  []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
				},
			},
			secretObj:   nil,
			expectError: false,
			validateResult: func(t *testing.T, result *csiapi.NodePublishVolumeRequest) {
				assert.Contains(t, result.VolumeId, "ro-test-pv")
				assert.Equal(t, "/var/lib/kubelet/pods/def/volumes/kubernetes.io~csi/pvc-456/mount", result.TargetPath)
				assert.True(t, result.Readonly)
			},
		},
		{
			name:                 "with secret",
			containerMountTarget: "/data/secret-mounted",
			persistentVolumeObj: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "secret-pv"},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						CSI: &corev1.CSIPersistentVolumeSource{
							Driver:       "diskplugin.csi.alibabacloud.com",
							VolumeHandle: "d-2zebxxxxxxxx",
							FSType:       "xfs",
							VolumeAttributes: map[string]string{
								"fsType":                    "xfs",
								"csi.storage.k8s.io/fstype": "xfs",
								"mountOptions":              "rw,noatime",
								"blockSize":                 "4096",
								"capacity":                  "100Gi",
							},
						},
					},
					MountOptions: []string{"rw"},
					AccessModes:  []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				},
			},
			secretObj: &corev1.Secret{
				Data: map[string][]byte{
					"accessKeyId":     []byte("test-access-key"),
					"accessKeySecret": []byte("test-secret-key"),
				},
			},
			expectError: false,
			validateResult: func(t *testing.T, result *csiapi.NodePublishVolumeRequest) {
				assert.Contains(t, result.VolumeId, "secret-pv")
				assert.Equal(t, "/data/secret-mounted", result.TargetPath)
				assert.Contains(t, result.Secrets, "accessKeyId")
				assert.Contains(t, result.Secrets, "accessKeySecret")
				assert.Equal(t, "test-access-key", result.Secrets["accessKeyId"])
				assert.Equal(t, "test-secret-key", result.Secrets["accessKeySecret"])
			},
		},
		{
			name:                 "pv without CSI spec",
			containerMountTarget: "/invalid/path",
			persistentVolumeObj: &corev1.PersistentVolume{
				ObjectMeta: metav1.ObjectMeta{Name: "invalid-pv"},
				Spec: corev1.PersistentVolumeSpec{
					PersistentVolumeSource: corev1.PersistentVolumeSource{
						HostPath: &corev1.HostPathVolumeSource{
							Path: "/host/path",
						},
					},
				},
			},
			secretObj:   nil,
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &MountProvider{}
			result, err := m.GenerateCSINodePublishVolumeRequest(
				context.Background(),
				tt.containerMountTarget,
				tt.persistentVolumeObj,
				tt.secretObj,
			)

			if tt.expectError {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.NotNil(t, result)

			if tt.validateResult != nil {
				tt.validateResult(t, result)
			}
		})
	}
}

func TestMountProvider_GenerateNodePublishVolumeRequest_EdgeCases(t *testing.T) {
	m := &MountProvider{}

	t.Run("nil persistent volume", func(t *testing.T) {
		_, err := m.GenerateCSINodePublishVolumeRequest(
			context.Background(),
			"/test/path",
			nil, // nil PV
			nil,
		)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "persistent volume object is nil")
	})

	t.Run("empty target path", func(t *testing.T) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "empty-target-pv"},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "diskplugin.csi.alibabacloud.com",
						VolumeHandle: "d-2zeaxxxxxxxx",
						FSType:       "ext4",
					},
				},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		}

		result, err := m.GenerateCSINodePublishVolumeRequest(
			context.Background(),
			"", // empty target path
			pv,
			nil,
		)

		require.NoError(t, err)
		assert.Equal(t, "", result.TargetPath)
		assert.Contains(t, result.VolumeId, "empty-target-pv")
	})

	t.Run("nil secret", func(t *testing.T) {
		pv := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: "nil-secret-pv"},
			Spec: corev1.PersistentVolumeSpec{
				PersistentVolumeSource: corev1.PersistentVolumeSource{
					CSI: &corev1.CSIPersistentVolumeSource{
						Driver:       "diskplugin.csi.alibabacloud.com",
						VolumeHandle: "d-2zeaxxxxxxxx",
						FSType:       "ext4",
						VolumeAttributes: map[string]string{
							"type": "ext4",
						},
					},
				},
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			},
		}

		result, err := m.GenerateCSINodePublishVolumeRequest(
			context.Background(),
			"/test/nil-secret",
			pv,
			nil, // nil secret
		)

		require.NoError(t, err)
		assert.Equal(t, "/test/nil-secret", result.TargetPath)
		assert.Empty(t, result.Secrets) // Secrets should be empty
	})
}

func TestMountProvider_GenerateNodePublishVolumeRequest_Idempotency(t *testing.T) {
	m := &MountProvider{}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "idempotent-pv"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "diskplugin.csi.alibabacloud.com",
					VolumeHandle: "d-2zeaxxxxxxxx",
					FSType:       "ext4",
					VolumeAttributes: map[string]string{
						"type": "ext4",
					},
				},
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}

	secret := &corev1.Secret{
		Data: map[string][]byte{
			"key": []byte("value"),
		},
	}

	ctx := context.Background()
	targetPath := "/test/idempotency"

	firstResult, err := m.GenerateCSINodePublishVolumeRequest(ctx, targetPath, pv, secret)
	require.NoError(t, err)

	secondResult, err := m.GenerateCSINodePublishVolumeRequest(ctx, targetPath, pv, secret)
	require.NoError(t, err)

	assert.Equal(t, firstResult.TargetPath, secondResult.TargetPath)
	assert.Equal(t, firstResult.Readonly, secondResult.Readonly)
	assert.Equal(t, firstResult.VolumeContext, secondResult.VolumeContext)
	assert.Equal(t, firstResult.Secrets, secondResult.Secrets)
	assert.Equal(t, firstResult.VolumeCapability, secondResult.VolumeCapability)
}

func BenchmarkMountProvider_GenerateNodePublishVolumeRequest(b *testing.B) {
	b.ReportAllocs()

	m := &MountProvider{}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "benchmark-pv"},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				CSI: &corev1.CSIPersistentVolumeSource{
					Driver:       "diskplugin.csi.alibabacloud.com",
					VolumeHandle: "d-2zeaxxxxxxxx",
					FSType:       "ext4",
					VolumeAttributes: map[string]string{
						"fsType":                     "ext4",
						"csi.storage.k8s.io/pv/name": "benchmark-pvc",
					},
				},
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		},
	}
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"devicePath": []byte("/dev/benchmark"),
		},
	}

	ctx := context.Background()
	targetPath := "/benchmark/path"

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.GenerateCSINodePublishVolumeRequest(ctx, targetPath, pv, secret)
	}
}
