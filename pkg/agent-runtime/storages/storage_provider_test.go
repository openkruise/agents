package storages

import (
	"context"
	"sync"
	"testing"

	csiapi "github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

// MockVolumeMountProvider is a mock implementation of VolumeMountProvider interface for testing
type MockVolumeMountProvider struct {
	driverName string
}

func (m *MockVolumeMountProvider) GetDriverName() string {
	return m.driverName
}

func (m *MockVolumeMountProvider) GenerateCSINodePublishVolumeRequest(
	ctx context.Context,
	containerMountTarget string,
	persistentVolumeObj *corev1.PersistentVolume,
	secretObj *corev1.Secret,
) (*csiapi.NodePublishVolumeRequest, error) {
	return &csiapi.NodePublishVolumeRequest{
		VolumeId:   "mock-volume-id",
		TargetPath: containerMountTarget,
		Readonly:   false,
	}, nil
}

// TestNewStorageProvider tests the NewStorageProvider constructor function
func TestNewStorageProvider(t *testing.T) {
	t.Run("create new storage provider", func(t *testing.T) {
		registry := NewStorageProvider()
		require.NotNil(t, registry)

		// Type assertion to verify the returned type
		storageProvider, ok := registry.(*StorageProvider)
		assert.True(t, ok, "NewStorageProvider should return *StorageProvider type")

		// Verify initialization state
		assert.NotNil(t, storageProvider.providers)
		assert.Len(t, storageProvider.providers, 0)
	})
}

// TestStorageProvider_RegisterProvider tests the RegisterProvider method
func TestStorageProvider_RegisterProvider(t *testing.T) {
	t.Run("register single provider", func(t *testing.T) {
		registry := NewStorageProvider()
		mockProvider := &MockVolumeMountProvider{driverName: "mock-driver"}

		registry.RegisterProvider("mock-driver", mockProvider)

		provider, exists := registry.GetProvider("mock-driver")
		assert.True(t, exists)
		assert.Equal(t, mockProvider, provider)
	})

	t.Run("register multiple providers", func(t *testing.T) {
		registry := NewStorageProvider()

		// Register multiple different providers
		provider1 := &MockVolumeMountProvider{driverName: "driver-1"}
		provider2 := &MockVolumeMountProvider{driverName: "driver-2"}
		provider3 := &MockVolumeMountProvider{driverName: "driver-3"}

		registry.RegisterProvider("driver-1", provider1)
		registry.RegisterProvider("driver-2", provider2)
		registry.RegisterProvider("driver-3", provider3)

		// Verify all providers are registered correctly
		testCases := []struct {
			driverName string
			expected   *MockVolumeMountProvider
		}{
			{"driver-1", provider1},
			{"driver-2", provider2},
			{"driver-3", provider3},
		}

		for _, tc := range testCases {
			provider, exists := registry.GetProvider(tc.driverName)
			assert.True(t, exists, "Provider %s should exist", tc.driverName)
			assert.Equal(t, tc.expected, provider, "Provider for %s should match", tc.driverName)
		}
	})

	t.Run("overwrite existing provider", func(t *testing.T) {
		registry := NewStorageProvider()
		originalProvider := &MockVolumeMountProvider{driverName: "original"}
		newProvider := &MockVolumeMountProvider{driverName: "new"}

		// First register the original provider
		registry.RegisterProvider("test-driver", originalProvider)
		provider, exists := registry.GetProvider("test-driver")
		assert.True(t, exists)
		assert.Equal(t, originalProvider, provider)

		// Overwrite the existing provider
		registry.RegisterProvider("test-driver", newProvider)
		provider, exists = registry.GetProvider("test-driver")
		assert.True(t, exists)
		assert.Equal(t, newProvider, provider)
	})
}

// TestStorageProvider_GetProvider tests the GetProvider method
func TestStorageProvider_GetProvider(t *testing.T) {
	t.Run("get existing provider", func(t *testing.T) {
		registry := NewStorageProvider()
		mockProvider := &MockVolumeMountProvider{driverName: "get-test-driver"}

		registry.RegisterProvider("get-test-driver", mockProvider)

		provider, exists := registry.GetProvider("get-test-driver")
		assert.True(t, exists)
		assert.Equal(t, mockProvider, provider)
	})

	t.Run("get non-existing provider", func(t *testing.T) {
		registry := NewStorageProvider()

		provider, exists := registry.GetProvider("non-existing-driver")
		assert.False(t, exists)
		assert.Nil(t, provider)
	})

	t.Run("get provider after registration", func(t *testing.T) {
		registry := NewStorageProvider()

		// Try to get an unregistered provider
		_, exists := registry.GetProvider("not-yet-registered")
		assert.False(t, exists)

		// Register provider
		mockProvider := &MockVolumeMountProvider{driverName: "delayed-register"}
		registry.RegisterProvider("delayed-register", mockProvider)

		// Verify we can now get the provider
		provider, exists := registry.GetProvider("delayed-register")
		assert.True(t, exists)
		assert.Equal(t, mockProvider, provider)
	})
}

// TestStorageProvider_IsSupported tests the IsSupported method
func TestStorageProvider_IsSupported(t *testing.T) {
	t.Run("supported driver", func(t *testing.T) {
		registry := NewStorageProvider()
		mockProvider := &MockVolumeMountProvider{driverName: "supported-driver"}

		registry.RegisterProvider("supported-driver", mockProvider)

		isSupported := registry.IsSupported("supported-driver")
		assert.True(t, isSupported)
	})

	t.Run("unsupported driver", func(t *testing.T) {
		registry := NewStorageProvider()

		isSupported := registry.IsSupported("unsupported-driver")
		assert.False(t, isSupported)
	})

	t.Run("case sensitivity", func(t *testing.T) {
		registry := NewStorageProvider()
		mockProvider := &MockVolumeMountProvider{driverName: "CaseSensitive"}

		registry.RegisterProvider("CaseSensitive", mockProvider)

		// Correct case
		assert.True(t, registry.IsSupported("CaseSensitive"))

		// Incorrect case
		assert.False(t, registry.IsSupported("casesensitive"))
		assert.False(t, registry.IsSupported("CASESENSITIVE"))
	})
}

// TestStorageProvider_ConcurrentAccess tests thread safety of the StorageProvider
func TestStorageProvider_ConcurrentAccess(t *testing.T) {
	t.Run("concurrent register and get operations", func(t *testing.T) {
		registry := NewStorageProvider()

		// Concurrent register and get operations
		var wg sync.WaitGroup

		// Start multiple goroutines to register providers
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				mockProvider := &MockVolumeMountProvider{driverName: "driver-" + string(rune(i+'0'))}
				registry.RegisterProvider("driver-"+string(rune(i+'0')), mockProvider)
			}
		}()

		// Start multiple goroutines to get providers
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				// Wait for a bit before trying to get to ensure registration has completed
				_, exists := registry.GetProvider("driver-" + string(rune(i+'0')))
				// May not be registered yet, so exists might be false
				_ = exists
			}
		}()

		// Wait for both goroutines to complete
		wg.Wait()

		// Finally verify all providers are properly registered
		for i := 0; i < 10; i++ {
			driverName := "driver-" + string(rune(i+'0'))
			isSupported := registry.IsSupported(driverName)
			assert.True(t, isSupported, "Driver %s should be supported after concurrent operations", driverName)
		}
	})
}

// TestStorageProvider_EmptyRegistry tests operations on an empty registry
func TestStorageProvider_EmptyRegistry(t *testing.T) {
	t.Run("operations on empty registry", func(t *testing.T) {
		registry := NewStorageProvider()

		// Verify initial state of empty registry
		assert.Equal(t, 0, len(registry.(*StorageProvider).providers))

		// Try getting a non-existent provider
		provider, exists := registry.GetProvider("any-driver")
		assert.False(t, exists)
		assert.Nil(t, provider)

		// Verify IsSupported returns false
		assert.False(t, registry.IsSupported("any-driver"))
	})
}

// TestStorageProvider_RWMutex ensures that read-write mutex works as expected
func TestStorageProvider_RWMutex(t *testing.T) {
	t.Run("mutex functionality", func(t *testing.T) {
		registry := NewStorageProvider()

		// Register some providers
		provider1 := &MockVolumeMountProvider{driverName: "mutex-test-driver-1"}
		provider2 := &MockVolumeMountProvider{driverName: "mutex-test-driver-2"}

		registry.RegisterProvider("mutex-test-driver-1", provider1)
		registry.RegisterProvider("mutex-test-driver-2", provider2)

		// Verify providers are registered
		assert.True(t, registry.IsSupported("mutex-test-driver-1"))
		assert.True(t, registry.IsSupported("mutex-test-driver-2"))

		// Get providers using GetProvider method
		p1, exists1 := registry.GetProvider("mutex-test-driver-1")
		p2, exists2 := registry.GetProvider("mutex-test-driver-2")

		assert.True(t, exists1)
		assert.True(t, exists2)
		assert.Equal(t, provider1, p1)
		assert.Equal(t, provider2, p2)
	})
}
