package e2b

import (
	"context"
	"fmt"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/sandbox-manager/clients"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
)

func TestController_generateNodePublishVolumeRequest(t *testing.T) {
	tests := []struct {
		name                   string
		containerMountPoint    string
		persistentVolumeName   string
		subPath                string
		readOnly               bool
		setupCache             func() infra.CacheProvider
		setupClient            func() *clients.ClientSet
		setupStorageRegistry   func() storages.VolumeMountProviderRegistry
		expectDriverName       string
		expectError            bool
		expectedErrorSubstring string
	}{
		{
			name:                   "empty persistent volume name",
			containerMountPoint:    "/container/mount/target",
			persistentVolumeName:   "",
			setupCache:             func() infra.CacheProvider { return &mockCacheProvider{} },
			setupClient:            func() *clients.ClientSet { return clients.NewFakeClientSet() },
			setupStorageRegistry:   func() storages.VolumeMountProviderRegistry { return &mockStorageProviderRegistry{} },
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "no found persistent volume name",
		},
		{
			name:                 "persistent volume not found in cache or client",
			persistentVolumeName: "non-existent-pv",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				return &mockCacheProvider{pv: nil}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry:   func() storages.VolumeMountProviderRegistry { return &mockStorageProviderRegistry{} },
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "failed to get persistent volume object",
		},
		{
			name:                 "persistent volume has no CSI spec",
			persistentVolumeName: "pv-without-csi",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-without-csi",
					},
					Spec: corev1.PersistentVolumeSpec{
						// No CSI spec
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry:   func() storages.VolumeMountProviderRegistry { return &mockStorageProviderRegistry{} },
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "no found csi object in persistent volume",
		},
		{
			name:                 "driver not supported",
			persistentVolumeName: "unsupported-driver-pv",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "unsupported-driver-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "unsupported-driver",
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{},
				}
				return registry
			},
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "driver unsupported-driver is not supported",
		},
		{
			name:                 "no provider found for driver",
			persistentVolumeName: "no-provider-pv",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "no-provider-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "test-driver",
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"test-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{},
				}
				return registry
			},
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "no provider found for driver: test-driver",
		},
		{
			name:                 "provider returns error",
			persistentVolumeName: "error-test-pv",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "error-test-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "error-driver",
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"error-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"error-driver": &mockVolumeMountProvider{generateError: fmt.Errorf("some error from provider")},
					},
				}
				return registry
			},
			expectDriverName:       "error-driver",
			expectError:            true,
			expectedErrorSubstring: "some error from provider",
		},
		{
			name:                 "NAS storage type success",
			persistentVolumeName: "nas-test-pv",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "nas-test-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "OSS storage type without secret ref",
			persistentVolumeName: "oss-no-secret-pv",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "oss-no-secret-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:               "oss-driver",
								NodePublishSecretRef: nil, // No secret ref
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"oss-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"oss-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "oss secret is required when mount oss volume",
		},
		// namespace test case
		{
			name:                 "secret ref with empty namespace should use default namespace",
			persistentVolumeName: "pv-with-empty-namespace-secret",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-empty-namespace-secret",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "test-driver",
								NodePublishSecretRef: &corev1.SecretReference{
									Name:      "test-secret",
									Namespace: "", // empty namespace
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"test-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"test-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "test-driver",
			expectError:      false,
		},
		// invalid namespace test case
		{
			name:                 "secret ref with invalid namespace should fail",
			persistentVolumeName: "pv-with-invalid-namespace-secret",
			containerMountPoint:  "/container/mount/target",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-invalid-namespace-secret",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "test-driver",
								NodePublishSecretRef: &corev1.SecretReference{
									Name:      "test-secret",
									Namespace: "invalid-namespace", // invalid namespace
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"test-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"test-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "invalid node publish secret ref namespace",
		},
		{
			name:                 "secret ref with invalid namespace should fail",
			persistentVolumeName: "pv-with-invalid-namespace-secret",
			containerMountPoint:  "/container/mount/target",
			subPath:              "",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-invalid-namespace-secret",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "test-driver",
								NodePublishSecretRef: &corev1.SecretReference{
									Name:      "test-secret",
									Namespace: "invalid-namespace", // invalid namespace
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"test-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"test-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "invalid node publish secret ref namespace",
		},
		{
			name:                 "access point sub path with valid base path should succeed",
			persistentVolumeName: "pv-with-access-point",
			containerMountPoint:  "/container/mount/target",
			subPath:              "subdir/data",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-access-point",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/share",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "access point sub path with trailing slash base path should succeed",
			persistentVolumeName: "pv-with-access-point-trailing-slash",
			containerMountPoint:  "/container/mount/target",
			subPath:              "subdir/data",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-access-point-trailing-slash",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/share/",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "access point sub path without base path should use validated sub path",
			persistentVolumeName: "pv-without-base-path",
			containerMountPoint:  "/container/mount/target",
			subPath:              "valid/subpath",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-without-base-path",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"other": "value",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "access point sub path with path traversal should fail",
			persistentVolumeName: "pv-with-malicious-access-point",
			containerMountPoint:  "/container/mount/target",
			subPath:              "../etc/passwd",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-malicious-access-point",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/share",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "failed to merge and validate paths",
		},
		{
			name:                 "access point sub path with null byte should fail",
			persistentVolumeName: "pv-with-null-byte-access-point",
			containerMountPoint:  "/container/mount/target",
			subPath:              "subdir\x00file",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-null-byte-access-point",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/share",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName:       "",
			expectError:            true,
			expectedErrorSubstring: "failed to merge and validate paths",
		},
		{
			name:                 "access point sub path only without volume attributes path",
			persistentVolumeName: "pv-subpath-only",
			containerMountPoint:  "/container/mount/target",
			subPath:              "standalone/path",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-subpath-only",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver:           "nas-driver",
								VolumeAttributes: make(map[string]string),
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "empty access point sub path should not modify path",
			persistentVolumeName: "pv-empty-subpath",
			containerMountPoint:  "/container/mount/target",
			subPath:              "",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-empty-subpath",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/original/path",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "access point sub path with leading slash",
			persistentVolumeName: "pv-leading-slash-subpath",
			containerMountPoint:  "/container/mount/target",
			subPath:              "/data/files",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-leading-slash-subpath",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/share",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "access point sub path with complex nested path",
			persistentVolumeName: "pv-complex-subpath",
			containerMountPoint:  "/container/mount/target",
			subPath:              "user/projects/2024/data",
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-complex-subpath",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/storage",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "provider returns error with readOnly false",
			persistentVolumeName: "error-test-pv",
			containerMountPoint:  "/container/mount/target",
			readOnly:             false,
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "error-test-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "error-driver",
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"error-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"error-driver": &mockVolumeMountProvider{generateError: fmt.Errorf("some error from provider")},
					},
				}
				return registry
			},
			expectDriverName:       "error-driver",
			expectError:            true,
			expectedErrorSubstring: "some error from provider",
		},
		{
			name:                 "read only mount with readOnly true",
			persistentVolumeName: "readonly-test-pv",
			containerMountPoint:  "/container/mount/target",
			readOnly:             true,
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "readonly-test-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "read write mount with readOnly false",
			persistentVolumeName: "readwrite-test-pv",
			containerMountPoint:  "/container/mount/target",
			readOnly:             false,
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "readwrite-test-pv",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "read only mount with access point sub path",
			persistentVolumeName: "pv-with-access-point",
			containerMountPoint:  "/container/mount/target",
			subPath:              "subdir/data",
			readOnly:             true,
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-access-point",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/share",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
		{
			name:                 "read write mount with access point sub path",
			persistentVolumeName: "pv-with-access-point-rw",
			containerMountPoint:  "/container/mount/target",
			subPath:              "subdir/data",
			readOnly:             false,
			setupCache: func() infra.CacheProvider {
				pv := &corev1.PersistentVolume{
					ObjectMeta: metav1.ObjectMeta{
						Name: "pv-with-access-point-rw",
					},
					Spec: corev1.PersistentVolumeSpec{
						PersistentVolumeSource: corev1.PersistentVolumeSource{
							CSI: &corev1.CSIPersistentVolumeSource{
								Driver: "nas-driver",
								VolumeAttributes: map[string]string{
									"path": "/share",
								},
							},
						},
					},
				}
				return &mockCacheProvider{pv: pv}
			},
			setupClient: func() *clients.ClientSet {
				return clients.NewFakeClientSet()
			},
			setupStorageRegistry: func() storages.VolumeMountProviderRegistry {
				registry := &mockStorageProviderRegistry{
					supportedDrivers: map[string]bool{
						"nas-driver": true,
					},
					providers: map[string]storages.VolumeMountProvider{
						"nas-driver": &mockVolumeMountProvider{},
					},
				}
				return registry
			},
			expectDriverName: "nas-driver",
			expectError:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create controller with mocked dependencies
			ctrl := &Controller{
				cache:           tt.setupCache(),
				client:          tt.setupClient(),
				storageRegistry: tt.setupStorageRegistry(),
				systemNamespace: utils.DefaultSandboxDeployNamespace,
			}

			ctx := context.Background()
			driverName, csiRequest, err := ctrl.generateNodePublishVolumeRequest(ctx, tt.containerMountPoint, tt.persistentVolumeName, tt.subPath, tt.readOnly)

			if tt.expectError {
				assert.Error(t, err)
				if tt.expectedErrorSubstring != "" {
					assert.Contains(t, err.Error(), tt.expectedErrorSubstring)
				}
				assert.Empty(t, driverName)
				assert.Nil(t, csiRequest)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectDriverName, driverName)
				assert.NotNil(t, csiRequest)
			}
		})
	}
}

type mockCacheProvider struct {
	pv *corev1.PersistentVolume
}

func (m *mockCacheProvider) GetPersistentVolume(name string) (*corev1.PersistentVolume, error) {
	if m.pv == nil {
		return nil, fmt.Errorf("not found")
	}
	return m.pv, nil
}

func (m *mockCacheProvider) GetSecret(namespace, name string) (*corev1.Secret, error) {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"akId":     []byte("test-ak-id"),
			"akSecret": []byte("test-ak-secret"),
		},
	}, nil
}

func (m *mockCacheProvider) GetClaimedSandbox(sandboxID string) (*agentsv1alpha1.Sandbox, error) {
	return nil, fmt.Errorf("not implemented for PV cache mock")
}

func (m *mockCacheProvider) ListSandboxWithUser(user string) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, fmt.Errorf("not implemented for PV cache mock")
}

func (m *mockCacheProvider) ListSandboxesInPool(pool string) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, fmt.Errorf("not implemented for PV cache mock")
}

type mockStorageProviderRegistry struct {
	supportedDrivers map[string]bool
	providers        map[string]storages.VolumeMountProvider
}

func (m *mockStorageProviderRegistry) IsSupported(driverName string) bool {
	if m.supportedDrivers != nil {
		return m.supportedDrivers[driverName]
	}
	return false
}

func (m *mockStorageProviderRegistry) GetProvider(driverName string) (storages.VolumeMountProvider, bool) {
	if m.providers != nil {
		provider, exists := m.providers[driverName]
		return provider, exists
	}
	return nil, false
}

func (m *mockStorageProviderRegistry) RegisterProvider(driverName string, provider storages.VolumeMountProvider) {
	if m.supportedDrivers == nil {
		m.supportedDrivers = make(map[string]bool)
	}
	if m.providers == nil {
		m.providers = make(map[string]storages.VolumeMountProvider)
	}
	m.supportedDrivers[driverName] = true
	m.providers[driverName] = provider
}

type mockVolumeMountProvider struct {
	generateError error
}

func (m *mockVolumeMountProvider) GenerateCSINodePublishVolumeRequest(
	ctx context.Context,
	containerMountTarget string,
	persistentVolumeObj *corev1.PersistentVolume, readOnlyExpect bool,
	secretObj *corev1.Secret,
) (*csi.NodePublishVolumeRequest, error) {
	if m.generateError != nil {
		return nil, m.generateError
	}

	// just oss driver need secret for oss driver
	driverName := persistentVolumeObj.Spec.CSI.Driver
	if driverName == "oss-driver" && secretObj == nil {
		return nil, fmt.Errorf("oss secret is required when mount oss volume")
	}

	return &csi.NodePublishVolumeRequest{
		VolumeId:   persistentVolumeObj.Name,
		TargetPath: containerMountTarget,
	}, nil
}

func TestMergeAndValidatePaths(t *testing.T) {
	tests := []struct {
		name                   string
		basePath               string
		subPath                string
		expectedMergedPath     string
		expectError            bool
		expectedErrorSubstring string
	}{
		{
			name:                   "empty base path should fail",
			basePath:               "",
			subPath:                "subdir",
			expectError:            true,
			expectedErrorSubstring: "base path cannot be empty",
		},
		{
			name:                   "relative base path should fail",
			basePath:               "share",
			subPath:                "subdir",
			expectError:            true,
			expectedErrorSubstring: "base path must be an absolute path starting with /",
		},
		{
			name:                   "empty sub path should fail",
			basePath:               "/share",
			subPath:                "",
			expectError:            true,
			expectedErrorSubstring: "sub path cannot be empty",
		},
		{
			name:                   "sub path with dot only should fail",
			basePath:               "/share",
			subPath:                ".",
			expectError:            true,
			expectedErrorSubstring: "sub path cannot be . or ..",
		},
		{
			name:                   "sub path with double dots only should fail",
			basePath:               "/share",
			subPath:                "..",
			expectError:            true,
			expectedErrorSubstring: "sub path cannot be . or ..",
		},
		{
			name:                   "sub path traversing to parent should fail",
			basePath:               "/share",
			subPath:                "../etc/passwd",
			expectError:            true,
			expectedErrorSubstring: "sub path must not traverse to parent directory",
		},
		{
			name:                   "sub path with deep parent traversal should fail",
			basePath:               "/share",
			subPath:                "subdir/../../etc/passwd",
			expectError:            true,
			expectedErrorSubstring: "sub path must not traverse to parent directory",
		},
		{
			name:                   "sub path with null byte should fail",
			basePath:               "/share",
			subPath:                "subdir\x00file",
			expectError:            true,
			expectedErrorSubstring: "sub path contains null byte",
		},
		{
			name:               "simple valid sub path with /share",
			basePath:           "/share",
			subPath:            "subdir",
			expectedMergedPath: "/share/subdir",
			expectError:        false,
		},
		{
			name:               "simple valid sub path with /share/",
			basePath:           "/share/",
			subPath:            "subdir",
			expectedMergedPath: "/share/subdir",
			expectError:        false,
		},
		{
			name:               "valid sub path with leading slash",
			basePath:           "/share",
			subPath:            "/subdir",
			expectedMergedPath: "/share/subdir",
			expectError:        false,
		},
		{
			name:               "valid sub path with multiple leading slashes",
			basePath:           "/share",
			subPath:            "//subdir",
			expectedMergedPath: "/share/subdir",
			expectError:        false,
		},
		{
			name:               "valid nested sub path",
			basePath:           "/share",
			subPath:            "subdir/nested/deep",
			expectedMergedPath: "/share/subdir/nested/deep",
			expectError:        false,
		},
		{
			name:               "valid sub path with mixed slashes",
			basePath:           "/share/",
			subPath:            "/subdir/nested/",
			expectedMergedPath: "/share/subdir/nested",
			expectError:        false,
		},
		{
			name:               "sub path staying within base after clean",
			basePath:           "/share/data",
			subPath:            "user/./files/../docs",
			expectedMergedPath: "/share/data/user/docs",
			expectError:        false,
		},
		{
			name:               "complex valid path normalization",
			basePath:           "/share/",
			subPath:            "a/b/c/./d/../e",
			expectedMergedPath: "/share/a/b/c/e",
			expectError:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mergedPath, err := mergeAndValidatePaths(tt.basePath, tt.subPath)

			if tt.expectError {
				assert.Error(t, err)
				if tt.expectedErrorSubstring != "" {
					assert.Contains(t, err.Error(), tt.expectedErrorSubstring)
				}
				assert.Empty(t, mergedPath)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedMergedPath, mergedPath)
			}
		})
	}
}

func TestValidateRelativePath(t *testing.T) {
	tests := []struct {
		name                   string
		subPath                string
		expectedValidatedPath  string
		expectError            bool
		expectedErrorSubstring string
	}{
		{
			name:                   "empty path should fail",
			subPath:                "",
			expectError:            true,
			expectedErrorSubstring: "sub path cannot be empty",
		},
		{
			name:                   "dot only should fail",
			subPath:                ".",
			expectError:            true,
			expectedErrorSubstring: "sub path cannot be . or ..",
		},
		{
			name:                   "double dots only should fail",
			subPath:                "..",
			expectError:            true,
			expectedErrorSubstring: "sub path cannot be . or ..",
		},
		{
			name:                   "parent traversal should fail",
			subPath:                "../etc/passwd",
			expectError:            true,
			expectedErrorSubstring: "sub path must not traverse to parent directory",
		},
		{
			name:                   "hidden parent traversal should fail",
			subPath:                "subdir/../../etc/passwd",
			expectError:            true,
			expectedErrorSubstring: "sub path must not traverse to parent directory",
		},
		{
			name:                   "multiple parent traversal should fail",
			subPath:                "../../../etc/passwd",
			expectError:            true,
			expectedErrorSubstring: "sub path must not traverse to parent directory",
		},
		{
			name:                   "null byte injection should fail",
			subPath:                "valid/path\x00injected",
			expectError:            true,
			expectedErrorSubstring: "sub path contains null byte",
		},
		{
			name:                   "single slash should become empty and fail",
			subPath:                "/",
			expectError:            true,
			expectedErrorSubstring: "sub path cannot be . or ..",
		},
		{
			name:                   "multiple slashes should become empty and fail",
			subPath:                "///",
			expectError:            true,
			expectedErrorSubstring: "sub path cannot be . or ..",
		},
		{
			name:                  "simple relative path without slashes",
			subPath:               "data",
			expectedValidatedPath: "data",
			expectError:           false,
		},
		{
			name:                  "path with leading slash",
			subPath:               "/data",
			expectedValidatedPath: "data",
			expectError:           false,
		},
		{
			name:                  "path with multiple leading slashes",
			subPath:               "//data",
			expectedValidatedPath: "data",
			expectError:           false,
		},
		{
			name:                  "path with trailing slash",
			subPath:               "data/",
			expectedValidatedPath: "data",
			expectError:           false,
		},
		{
			name:                  "nested relative path",
			subPath:               "subdir/nested/path",
			expectedValidatedPath: "subdir/nested/path",
			expectError:           false,
		},
		{
			name:                  "path with current dir reference in middle",
			subPath:               "a/./b",
			expectedValidatedPath: "a/b",
			expectError:           false,
		},
		{
			name:                  "path with safe parent reference",
			subPath:               "a/b/../c",
			expectedValidatedPath: "a/c",
			expectError:           false,
		},
		{
			name:                  "complex path normalization",
			subPath:               "a/b/c/./d/../e/f",
			expectedValidatedPath: "a/b/c/e/f",
			expectError:           false,
		},
		{
			name:                  "path staying at same level",
			subPath:               "a/b/c/../../d",
			expectedValidatedPath: "a/d",
			expectError:           false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validatedPath, err := validateRelativePath(tt.subPath)

			if tt.expectError {
				assert.Error(t, err)
				if tt.expectedErrorSubstring != "" {
					assert.Contains(t, err.Error(), tt.expectedErrorSubstring)
				}
				assert.Empty(t, validatedPath)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.expectedValidatedPath, validatedPath)
			}
		})
	}
}
