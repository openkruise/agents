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
)

func TestController_generateNodePublishVolumeRequest(t *testing.T) {
	tests := []struct {
		name                   string
		containerMountPoint    string
		persistentVolumeName   string
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
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create controller with mocked dependencies
			ctrl := &Controller{
				cache:           tt.setupCache(),
				client:          tt.setupClient(),
				storageRegistry: tt.setupStorageRegistry(),
			}

			ctx := context.Background()
			driverName, csiRequest, err := ctrl.generateNodePublishVolumeRequest(ctx, tt.containerMountPoint, tt.persistentVolumeName)

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

func (m *mockCacheProvider) GetSandbox(sandboxID string) (*agentsv1alpha1.Sandbox, error) {
	return nil, fmt.Errorf("not implemented for PV cache mock")
}

func (m *mockCacheProvider) ListSandboxWithUser(user string) ([]*agentsv1alpha1.Sandbox, error) {
	return nil, fmt.Errorf("not implemented for PV cache mock")
}

func (m *mockCacheProvider) ListAvailableSandboxes(pool string) ([]*agentsv1alpha1.Sandbox, error) {
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
	persistentVolumeObj *corev1.PersistentVolume,
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
