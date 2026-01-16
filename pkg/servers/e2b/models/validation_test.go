// validation_test.go
package models

import (
	"testing"
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
