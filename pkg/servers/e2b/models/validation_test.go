// validation_test.go
package models

import (
	"sort"
	"strings"
	"testing"

	"github.com/openkruise/agents/api/v1alpha1"
)

func TestValidateMountPoint(t *testing.T) {
	tests := []struct {
		name         string
		mountPoint   string
		expectError  bool
		errorMessage string
	}{
		{
			name:        "valid absolute path",
			mountPoint:  "/valid/path",
			expectError: false,
		},
		{
			name:         "empty mount point",
			mountPoint:   "",
			expectError:  true,
			errorMessage: "mount point cannot be empty",
		},
		{
			name:         "not starting with slash",
			mountPoint:   "invalid/path",
			expectError:  true,
			errorMessage: "mount point must start with '/'",
		},
		{
			name:         "contains two dot",
			mountPoint:   "/path/../etc",
			expectError:  true,
			errorMessage: "mount point contains invalid '..' path element",
		},
		{
			name:         "contains two dot at end",
			mountPoint:   "/path/..",
			expectError:  true,
			errorMessage: "mount point contains invalid '..' path element",
		},
		{
			name:         "contains two dot slash",
			mountPoint:   "/path/../",
			expectError:  true,
			errorMessage: "mount point contains invalid '..' path element",
		},
		{
			name:        "root path",
			mountPoint:  "/",
			expectError: false,
		},
		{
			name:        "path with dots but not relative",
			mountPoint:  "/path.with.dots",
			expectError: false,
		},
		{
			name:        "complex valid path",
			mountPoint:  "/var/lib/kubelet/pods",
			expectError: false,
		},
		{
			name:         "path with single dot",
			mountPoint:   "/path/./test",
			expectError:  true,
			errorMessage: "mount point contains invalid path elements like '..' or '.'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMountPoint(tt.mountPoint)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
					return
				}

				if err.Error() != tt.errorMessage {
					t.Errorf("expected error message '%s', but got '%s'", tt.errorMessage, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
				}
			}
		})
	}
}

func TestParseAndValidatePersistentContents(t *testing.T) {
	tests := []struct {
		name           string
		contents       string
		expectError    bool
		errorContains  string
		expectedResult []string
	}{
		{
			name:           "empty string",
			contents:       "",
			expectError:    false,
			expectedResult: nil,
		},
		{
			name:           "single valid value - memory",
			contents:       "memory",
			expectError:    false,
			expectedResult: []string{v1alpha1.CheckpointPersistentContentMemory},
		},
		{
			name:           "single valid value - filesystem",
			contents:       "filesystem",
			expectError:    false,
			expectedResult: []string{v1alpha1.CheckpointPersistentContentFilesystem},
		},
		{
			name:           "two valid values - memory,filesystem",
			contents:       "memory,filesystem",
			expectError:    false,
			expectedResult: []string{v1alpha1.CheckpointPersistentContentMemory, v1alpha1.CheckpointPersistentContentFilesystem},
		},
		{
			name:           "two valid values - filesystem,memory (different order)",
			contents:       "filesystem,memory",
			expectError:    false,
			expectedResult: []string{v1alpha1.CheckpointPersistentContentMemory, v1alpha1.CheckpointPersistentContentFilesystem},
		},
		{
			name:           "values with spaces",
			contents:       " memory , filesystem ",
			expectError:    false,
			expectedResult: []string{v1alpha1.CheckpointPersistentContentMemory, v1alpha1.CheckpointPersistentContentFilesystem},
		},
		{
			name:           "values with empty parts",
			contents:       "memory,,filesystem",
			expectError:    false,
			expectedResult: []string{v1alpha1.CheckpointPersistentContentMemory, v1alpha1.CheckpointPersistentContentFilesystem},
		},
		{
			name:           "duplicate values - memory,memory",
			contents:       "memory,memory",
			expectError:    false,
			expectedResult: []string{v1alpha1.CheckpointPersistentContentMemory},
		},
		{
			name:           "duplicate values - filesystem,filesystem",
			contents:       "filesystem,filesystem",
			expectError:    false,
			expectedResult: []string{v1alpha1.CheckpointPersistentContentFilesystem},
		},
		{
			name:          "invalid value - ip",
			contents:      "ip",
			expectError:   true,
			errorContains: "invalid persistent content",
		},
		{
			name:          "invalid value - unknown",
			contents:      "unknown",
			expectError:   true,
			errorContains: "invalid persistent content",
		},
		{
			name:          "mixed valid and invalid - memory,invalid",
			contents:      "memory,invalid",
			expectError:   true,
			errorContains: "invalid persistent content",
		},
		{
			name:          "mixed valid and invalid - invalid,filesystem",
			contents:      "invalid,filesystem",
			expectError:   true,
			errorContains: "invalid persistent content",
		},
		{
			name:           "only spaces and commas",
			contents:       " , , ",
			expectError:    false,
			expectedResult: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parseAndValidatePersistentContents(tt.contents)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error but got none")
					return
				}
				if tt.errorContains != "" && !strings.Contains(err.Error(), tt.errorContains) {
					t.Errorf("expected error to contain '%s', but got '%s'", tt.errorContains, err.Error())
				}
			} else {
				if err != nil {
					t.Errorf("unexpected error: %v", err)
					return
				}
				if tt.expectedResult == nil {
					if result != nil {
						t.Errorf("expected nil result, but got %v", result)
					}
				} else {
					if len(result) != len(tt.expectedResult) {
						t.Errorf("expected %d elements, but got %d", len(tt.expectedResult), len(result))
						return
					}
					// Sort both slices for comparison since map iteration order is not guaranteed
					sort.Strings(result)
					sort.Strings(tt.expectedResult)
					for i := range tt.expectedResult {
						if result[i] != tt.expectedResult[i] {
							t.Errorf("expected result[%d] to be '%s', but got '%s'", i, tt.expectedResult[i], result[i])
						}
					}
				}
			}
		})
	}
}
