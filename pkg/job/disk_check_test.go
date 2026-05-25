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

package job

import (
	"context"
	"os"
	"testing"
)

func TestCheckDiskSpace_Disabled(t *testing.T) {
	os.Unsetenv(EnvDiskSpaceCheckEnabled)

	result := CheckDiskSpace(context.TODO(), "abc123")
	if result != ExitCodeSuccess {
		t.Errorf("expected ExitCodeSuccess when disabled, got %d", result)
	}
}

func TestCheckDiskSpace_EnabledButCRIUnavailable(t *testing.T) {
	os.Setenv(EnvDiskSpaceCheckEnabled, "true")
	defer os.Unsetenv(EnvDiskSpaceCheckEnabled)
	// Use a non-existent socket so CRI connection fails gracefully
	os.Setenv(EnvContainerdSock, "/tmp/nonexistent-test.sock")
	defer os.Unsetenv(EnvContainerdSock)

	result := CheckDiskSpace(context.TODO(), "abc123")
	// Should return success because it can't connect to CRI (graceful degradation)
	if result != ExitCodeSuccess {
		t.Errorf("expected ExitCodeSuccess on CRI error (graceful), got %d", result)
	}
}

func TestGetAvailableBytes(t *testing.T) {
	// Test with a known valid path
	bytes, err := getAvailableBytes("/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bytes <= 0 {
		t.Errorf("expected positive available bytes, got %d", bytes)
	}
}

func TestGetAvailableBytes_InvalidPath(t *testing.T) {
	_, err := getAvailableBytes("/nonexistent/path/that/does/not/exist")
	if err == nil {
		t.Error("expected error for invalid path")
	}
}

func TestCheckDiskSpace_TableDriven(t *testing.T) {
	tests := []struct {
		name        string
		enabled     string
		sock        string
		containerID string
		expectCode  int
	}{
		{
			name:        "disabled returns success",
			enabled:     "",
			containerID: "any-container",
			expectCode:  ExitCodeSuccess,
		},
		{
			name:        "disabled explicitly false returns success",
			enabled:     "false",
			containerID: "any-container",
			expectCode:  ExitCodeSuccess,
		},
		{
			name:        "enabled with invalid socket returns success (graceful)",
			enabled:     "true",
			sock:        "/tmp/nonexistent-disk-check-test.sock",
			containerID: "abc123",
			expectCode:  ExitCodeSuccess,
		},
		{
			name:        "enabled with empty container ID still graceful",
			enabled:     "true",
			sock:        "/tmp/nonexistent-disk-check-test.sock",
			containerID: "",
			expectCode:  ExitCodeSuccess,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.enabled != "" {
				os.Setenv(EnvDiskSpaceCheckEnabled, tt.enabled)
			} else {
				os.Unsetenv(EnvDiskSpaceCheckEnabled)
			}
			defer os.Unsetenv(EnvDiskSpaceCheckEnabled)

			if tt.sock != "" {
				os.Setenv(EnvContainerdSock, tt.sock)
			} else {
				os.Unsetenv(EnvContainerdSock)
			}
			defer os.Unsetenv(EnvContainerdSock)

			result := CheckDiskSpace(context.TODO(), tt.containerID)
			if result != tt.expectCode {
				t.Errorf("expected exit code %d, got %d", tt.expectCode, result)
			}
		})
	}
}

func TestGetWritableLayerSize_InvalidSocket(t *testing.T) {
	os.Setenv(EnvContainerdSock, "/tmp/nonexistent-writable-layer-test.sock")
	defer os.Unsetenv(EnvContainerdSock)

	_, err := getWritableLayerSize(context.TODO(), "test-container-id")
	if err == nil {
		t.Fatal("expected error when connecting to non-existent socket")
	}
}

func TestGetAvailableBytes_ValidPaths(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{"tmp dir", "/tmp"},
		{"root dir", "/"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bytes, err := getAvailableBytes(tt.path)
			if err != nil {
				t.Fatalf("unexpected error for path %s: %v", tt.path, err)
			}
			if bytes <= 0 {
				t.Errorf("expected positive available bytes for %s, got %d", tt.path, bytes)
			}
		})
	}
}
