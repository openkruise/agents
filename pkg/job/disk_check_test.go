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
