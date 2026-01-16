package storages

import (
	"sync"
)

type StorageProvider struct {
	providers map[string]VolumeMountProvider // driverName -> provider
	mutex     sync.RWMutex
}

func NewStorageProvider() VolumeMountProviderRegistry {
	registry := &StorageProvider{
		providers: make(map[string]VolumeMountProvider),
	}
	registry.initializeProviders()
	return registry
}

func (isp *StorageProvider) initializeProviders() {
	for _, fn := range initializeProviderFuncs {
		fn(isp)
	}
}

func (isp *StorageProvider) RegisterProvider(driverName string, provider VolumeMountProvider) {
	isp.mutex.Lock()
	defer isp.mutex.Unlock()
	isp.providers[driverName] = provider
}

func (isp *StorageProvider) GetProvider(driverName string) (VolumeMountProvider, bool) {
	isp.mutex.RLock()
	defer isp.mutex.RUnlock()
	// when provider not found return nil
	provider, exists := isp.providers[driverName]
	if !exists {
		return nil, false
	}
	return provider, true
}

func (isp *StorageProvider) IsSupported(driverName string) bool {
	_, exists := isp.GetProvider(driverName)
	return exists
}
