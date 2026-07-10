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
	"os"
	"testing"
)

func TestSetupRegistryAuth_NoSecret(t *testing.T) {
	// registryConfigDir points to a non-existent path in test env,
	// so setupRegistryAuth should skip silently without error.
	t.Setenv("DOCKER_CONFIG", "")
	err := setupRegistryAuth()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	// DOCKER_CONFIG should NOT be set to registryConfigDir when no secret is mounted
	if got := os.Getenv("DOCKER_CONFIG"); got == registryConfigDir {
		t.Errorf("DOCKER_CONFIG should not be set when secret is absent")
	}
}

func TestSetupRegistryAuth_WithSecret(t *testing.T) {
	// Create a temp directory simulating the mounted secret
	tmpDir := t.TempDir()
	configPath := tmpDir + "/config.json"
	if err := os.WriteFile(configPath, []byte(`{"auths":{}}`), 0644); err != nil {
		t.Fatal(err)
	}

	// Temporarily override registryConfigDir by testing the internal logic directly.
	// Since registryConfigDir is a const, we test via setupRegistryAuthFrom helper.
	t.Setenv("DOCKER_CONFIG", "")
	err := setupRegistryAuthFrom(tmpDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	if got := os.Getenv("DOCKER_CONFIG"); got != tmpDir {
		t.Errorf("DOCKER_CONFIG = %q, want %q", got, tmpDir)
	}
}

func TestSetupRegistryAuth_StatError(t *testing.T) {
	configDir := t.TempDir() + "/not-dir"
	if err := os.WriteFile(configDir, []byte("not a dir"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", "")
	err := setupRegistryAuthFrom(configDir)
	if err == nil {
		t.Fatal("expected stat error, got nil")
	}
	if got := os.Getenv("DOCKER_CONFIG"); got != "" {
		t.Errorf("DOCKER_CONFIG should not be set on stat error, got %q", got)
	}
}
