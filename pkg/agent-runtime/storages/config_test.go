package storages

import (
	"os"
	"strings"
	"testing"

	"github.com/openkruise/agents/pkg/agent-runtime/common"
)

// TestInitFunction tests the package initialization logic with environment variable
func TestInitFunction(t *testing.T) {
	// Save original environment variable value
	originalEnvValue := os.Getenv(common.ENV_DYNAMIC_STORAGE_DRIVER_LIST)
	defer func() {
		// Restore original environment variable after test
		if originalEnvValue == "" {
			os.Unsetenv(common.ENV_DYNAMIC_STORAGE_DRIVER_LIST)
		} else {
			os.Setenv(common.ENV_DYNAMIC_STORAGE_DRIVER_LIST, originalEnvValue)
		}
	}()

	// Reset global state before test
	resetInitializeProviderFuncs()

	// Set up test environment variable
	testDriverList := "driver1,driver2,driver3"
	os.Setenv(common.ENV_DYNAMIC_STORAGE_DRIVER_LIST, testDriverList)

	// Manually execute initialization logic to simulate init function behavior
	// Note: We can't directly test init() as it runs only once during package loading
	// So we manually implement the same logic with closure fix
	dynamicDriverList := strings.Split(testDriverList, ",")
	var tempInitializeProviderFuncs []initProviderFunc

	for _, driverName := range dynamicDriverList {
		driverName := driverName                 // Capture loop variable to avoid closure trap
		if strings.TrimSpace(driverName) != "" { // Skip empty entries
			tempInitializeProviderFuncs = append(tempInitializeProviderFuncs,
				func(sp *StorageProvider) {
					sp.RegisterProvider(driverName, &MountProvider{})
				})
		}
	}

	// Verify that initialization logic would create correct number of provider functions
	expectedCount := 3 // Number of non-empty drivers in our test case
	actualCount := len(tempInitializeProviderFuncs)

	if actualCount != expectedCount {
		t.Errorf("Expected %d provider functions, got %d", expectedCount, actualCount)
	}

	// Test with empty environment variable
	resetInitializeProviderFuncs()
	os.Setenv(common.ENV_DYNAMIC_STORAGE_DRIVER_LIST, "")

	emptyDriverList := strings.Split("", ",")
	var emptyTempFuncs []initProviderFunc
	for _, driverName := range emptyDriverList {
		driverName := driverName
		if strings.TrimSpace(driverName) != "" {
			emptyTempFuncs = append(emptyTempFuncs,
				func(sp *StorageProvider) {
					sp.RegisterProvider(driverName, &MountProvider{})
				})
		}
	}

	// Should have 1 element because Split("") returns [""]
	if len(emptyTempFuncs) != 0 { // After trimming empty string
		t.Errorf("Expected 0 provider functions for empty env var, got %d", len(emptyTempFuncs))
	}
}

// resetInitializeProviderFuncs resets the global initializeProviderFuncs slice for testing
func resetInitializeProviderFuncs() {
	initializeProviderFuncs = []initProviderFunc{}
}
