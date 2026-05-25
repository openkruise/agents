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

func TestDoCommit_DryRunMode(t *testing.T) {
	os.Setenv(EnvDryRun, "true")
	defer os.Unsetenv(EnvDryRun)
	os.Setenv(EnvContainerID, "test-container-id")
	defer os.Unsetenv(EnvContainerID)
	os.Setenv(EnvCommitImage, "registry.example.com/img:v1")
	defer os.Unsetenv(EnvCommitImage)

	result := DoCommit(context.Background())
	if result != ExitCodeSuccess {
		t.Errorf("DoCommit in dry-run mode should return ExitCodeSuccess, got %d", result)
	}
}

func TestDoCommit_NerdctlNotFound(t *testing.T) {
	// Ensure dry-run is off so we actually attempt nerdctl commit
	os.Unsetenv(EnvDryRun)
	os.Setenv(EnvContainerID, "test-container-id")
	defer os.Unsetenv(EnvContainerID)
	os.Setenv(EnvCommitImage, "registry.example.com/img:v1")
	defer os.Unsetenv(EnvCommitImage)
	// Use a non-existent socket to avoid connecting to real containerd
	os.Setenv(EnvContainerdSock, "/tmp/nonexistent-test.sock")
	defer os.Unsetenv(EnvContainerdSock)
	// Override PATH to ensure nerdctl is not found
	origPath := os.Getenv("PATH")
	os.Setenv("PATH", "/nonexistent-path-for-test")
	defer os.Setenv("PATH", origPath)

	result := DoCommit(context.Background())
	// nerdctl commit will fail because the binary is not found
	if result != ExitCodeCommitFailed {
		t.Errorf("DoCommit should return ExitCodeCommitFailed when nerdctl not found, got %d", result)
	}
}

func TestDoCommit_CommitFailsWithInvalidSocket(t *testing.T) {
	os.Unsetenv(EnvDryRun)
	os.Setenv(EnvContainerID, "fake-container-id")
	defer os.Unsetenv(EnvContainerID)
	os.Setenv(EnvCommitImage, "test.io/img:latest")
	defer os.Unsetenv(EnvCommitImage)
	os.Setenv(EnvContainerdSock, "/tmp/nonexistent-commit-test.sock")
	defer os.Unsetenv(EnvContainerdSock)

	// If nerdctl binary exists on this system, it will fail due to invalid socket.
	// If nerdctl doesn't exist, it will fail due to binary not found.
	// Either way, it should NOT return success.
	result := DoCommit(context.Background())
	if result == ExitCodeSuccess {
		t.Error("DoCommit should not succeed with invalid containerd socket")
	}
}
