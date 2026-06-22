package configuration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// TestGetSandboxResumePodPersistentContent tests the retrieval of sandbox resume configuration
func TestGetSandboxResumePodPersistentContent(t *testing.T) {
	// Create temporary directory for testing
	tempDir := t.TempDir()

	// Reset global state before each test
	sandboxConfigurations = make(map[string]interface{})

	// Define test case structure
	type testCase struct {
		name          string
		configContent *SandboxResumePodPersistentContent
		expectNil     bool
		description   string
	}

	// Define test cases
	testCases := []testCase{
		{
			name: "valid configuration with all fields",
			configContent: &SandboxResumePodPersistentContent{
				AnnotationKeys: []string{
					"ProviderCreate",
					"alibabacloud.com/cpu-vendors",
					"alibabacloud.com/instance-id",
					"alibabacloud.com/pod-ephemeral-storage",
				},
				LabelKeys: []string{
					"alibabacloud.com/acs",
					"alibabacloud.com/compute-class",
					"alibabacloud.com/compute-qos",
				},
			},
			expectNil:   false,
			description: "Test retrieval of valid configuration with all fields",
		},
		{
			name:          "non-existent configuration file",
			configContent: nil,
			expectNil:     true,
			description:   "Test behavior when configuration file does not exist",
		},
		{
			name: "configuration with empty slices",
			configContent: &SandboxResumePodPersistentContent{
				AnnotationKeys: []string{},
				LabelKeys:      []string{},
			},
			expectNil:   false,
			description: "Test configuration with empty annotation and label keys",
		},
	}

	// Execute test cases
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// If we want to test with a valid config file, create it
			if tc.configContent != nil {
				configPath := filepath.Join(tempDir, SandboxResumePodPersistentContentKey)
				jsonData, err := json.Marshal(tc.configContent)
				if err != nil {
					t.Fatalf("Failed to marshal test configuration: %v", err)
				}

				err = os.WriteFile(configPath, jsonData, 0644)
				if err != nil {
					t.Fatalf("Failed to write configuration file: %v", err)
				}
			} else {
				// Ensure the config file doesn't exist
				configPath := filepath.Join(tempDir, SandboxResumePodPersistentContentKey)
				os.Remove(configPath)
			}

			// Re-initialize configuration loading process using test directory
			loadConfigurationFromDir(tempDir)

			// Call the function under test
			result := GetSandboxResumePodPersistentContent()

			// Validate results based on expectations
			if tc.expectNil {
				if result != nil {
					t.Errorf("Expected nil result, but got: %+v", *result)
				}
			} else {
				if result == nil {
					t.Errorf("Expected non-nil result, but got nil")
				} else if tc.configContent != nil && !reflect.DeepEqual(result, tc.configContent) {
					t.Errorf("Expected: %+v, Got: %+v", *tc.configContent, *result)
				}
			}
		})
	}
}

// TestInvalidJSONInConfiguration tests behavior when configuration file contains invalid JSON
func TestInvalidJSONInConfiguration(t *testing.T) {
	// Create temporary directory for testing
	tempDir := t.TempDir()

	// Reset global state before test
	sandboxConfigurations = make(map[string]interface{})

	// Create a configuration file with truly invalid JSON
	configPath := filepath.Join(tempDir, SandboxResumePodPersistentContentKey)
	invalidJSON := []byte(`{ "annotationKeys": "invalid", "labelKeys": "invalid" }`) // Type mismatch
	err := os.WriteFile(configPath, invalidJSON, 0644)
	if err != nil {
		t.Fatalf("Failed to write invalid JSON configuration file: %v", err)
	}

	// Initialize configuration loading (should fail gracefully)
	loadConfigurationFromDir(tempDir)

	// Function should return nil since JSON is invalid
	result := GetSandboxResumePodPersistentContent()
	if result != nil {
		t.Errorf("Expected nil result due to invalid JSON, but got: %+v", *result)
	}
}

// TestMultipleConfigurationFiles tests the scenario with multiple configuration objects
func TestMultipleConfigurationFiles(t *testing.T) {
	// Create temporary directory for testing
	tempDir := t.TempDir()

	// Reset global state before test
	sandboxConfigurations = make(map[string]interface{})

	// Create valid configuration
	configContent := &SandboxResumePodPersistentContent{
		AnnotationKeys: []string{"test.annotation"},
		LabelKeys:      []string{"test.label"},
	}

	configPath := filepath.Join(tempDir, SandboxResumePodPersistentContentKey)
	jsonData, err := json.Marshal(configContent)
	if err != nil {
		t.Fatalf("Failed to marshal test configuration: %v", err)
	}

	err = os.WriteFile(configPath, jsonData, 0644)
	if err != nil {
		t.Fatalf("Failed to write configuration file: %v", err)
	}

	// Initialize configuration loading with test directory
	loadConfigurationFromDir(tempDir)

	// Verify that the configuration was loaded correctly
	result := GetSandboxResumePodPersistentContent()
	if result == nil {
		t.Error("Expected non-nil result, but got nil")
	} else if !reflect.DeepEqual(result, configContent) {
		t.Errorf("Expected: %+v, Got: %+v", *configContent, *result)
	}
}

// loadConfigurationFromDir loads configuration from specified directory for testing
// This mimics the logic in the init function but allows for controlled testing
func loadConfigurationFromDir(configDir string) {
	logger := logf.FromContext(context.TODO())

	// Clear the configurations map before initializing
	sandboxConfigurations = make(map[string]interface{})

	for i := range objs {
		obj := objs[i]

		// Create a new instance of the object type for each initialization
		switch obj.Key {
		case SandboxResumePodPersistentContentKey:
			newObj := &SandboxResumePodPersistentContent{}
			obj.Object = newObj
		}

		filePath := filepath.Join(configDir, obj.Key)
		data, err := os.ReadFile(filePath)
		if err != nil {
			continue // File might not exist, which is OK
		}

		err = json.Unmarshal(data, obj.Object)
		if err != nil {
			logger.Error(err, "Unmarshal failed", "file", filePath, "data", string(data))
			continue
		}

		// Store in the global map
		sandboxConfigurations[obj.Key] = obj.Object
		logger.Info("read configuration file success", "file", filePath)
	}
}
