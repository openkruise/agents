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
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// testSetupRegistryAuth tests the format conversion logic by overriding
// the package-level constants via a helper that accepts paths.
// Since setupRegistryAuth uses hardcoded paths, we test the conversion
// logic indirectly via convertK8sDockerConfig.

func TestConvertK8sDockerConfig_WithAuthField(t *testing.T) {
	input := k8sDockerConfigJSON{
		Auths: map[string]k8sAuthEntry{
			"registry.example.com": {Auth: "dGVzdDpwYXNz"},
		},
	}
	result := convertK8sDockerConfig(input)
	if len(result.Auths) != 1 {
		t.Fatalf("expected 1 auth entry, got %d", len(result.Auths))
	}
	if result.Auths["registry.example.com"].Auth != "dGVzdDpwYXNz" {
		t.Errorf("unexpected auth value: %s", result.Auths["registry.example.com"].Auth)
	}
}

func TestConvertK8sDockerConfig_WithUsernamePassword(t *testing.T) {
	input := k8sDockerConfigJSON{
		Auths: map[string]k8sAuthEntry{
			"docker.io": {Username: "user", Password: "pass"},
		},
	}
	result := convertK8sDockerConfig(input)
	expected := base64.StdEncoding.EncodeToString([]byte("user:pass"))
	if result.Auths["docker.io"].Auth != expected {
		t.Errorf("auth = %s, want %s", result.Auths["docker.io"].Auth, expected)
	}
}

func TestConvertK8sDockerConfig_NoCredentials(t *testing.T) {
	input := k8sDockerConfigJSON{
		Auths: map[string]k8sAuthEntry{
			"gcr.io": {},
		},
	}
	result := convertK8sDockerConfig(input)
	if len(result.Auths) != 0 {
		t.Errorf("expected 0 entries for empty credentials, got %d", len(result.Auths))
	}
}

func TestConvertK8sDockerConfig_MultipleServers(t *testing.T) {
	input := k8sDockerConfigJSON{
		Auths: map[string]k8sAuthEntry{
			"registry.example.com": {Auth: "abc"},
			"docker.io":            {Username: "u", Password: "p"},
			"empty.io":             {},
		},
	}
	result := convertK8sDockerConfig(input)
	if len(result.Auths) != 2 {
		t.Errorf("expected 2 entries (empty.io skipped), got %d", len(result.Auths))
	}
}

func TestSetupRegistryAuth_NoSecretFile(t *testing.T) {
	// When the secret file doesn't exist, setupRegistryAuth should succeed silently
	tmpDir := t.TempDir()
	origRegistryPath := registrySecretPath
	origDockerDir := dockerConfigDir

	// We can't override package consts, but we can test that setupRegistryAuth
	// handles missing files by checking it doesn't error when the path doesn't exist
	// This is a basic smoke test since setupRegistryAuth uses hardcoded paths
	_ = tmpDir
	_ = origRegistryPath
	_ = origDockerDir
	// The real test is that setupRegistryAuth() doesn't panic when file is missing
	// It reads from /var/run/secrets/registry/config.json which won't exist in tests
	err := setupRegistryAuth()
	if err != nil {
		t.Errorf("setupRegistryAuth should not error when secret file is missing: %v", err)
	}
}

func TestSetupRegistryAuth_EndToEnd(t *testing.T) {
	// Create temp dirs to simulate the mount paths
	tmpDir := t.TempDir()
	secretDir := filepath.Join(tmpDir, "registry")
	configDir := filepath.Join(tmpDir, "docker")
	os.MkdirAll(secretDir, 0700)

	// Write a K8s dockerconfigjson secret
	k8sConfig := map[string]interface{}{
		"auths": map[string]interface{}{
			"registry.example.com": map[string]interface{}{
				"username": "testuser",
				"password": "testpass",
			},
		},
	}
	data, _ := json.Marshal(k8sConfig)
	secretPath := filepath.Join(secretDir, "config.json")
	os.WriteFile(secretPath, data, 0600)

	// We can test convertK8sDockerConfig + write logic separately
	// since setupRegistryAuth uses hardcoded paths
	var k8sCfg k8sDockerConfigJSON
	json.Unmarshal(data, &k8sCfg)
	dockerCfg := convertK8sDockerConfig(k8sCfg)

	// Verify conversion result
	if len(dockerCfg.Auths) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(dockerCfg.Auths))
	}
	entry := dockerCfg.Auths["registry.example.com"]
	decoded, _ := base64.StdEncoding.DecodeString(entry.Auth)
	if string(decoded) != "testuser:testpass" {
		t.Errorf("decoded auth = %s, want testuser:testpass", string(decoded))
	}

	// Write the converted config and verify
	os.MkdirAll(configDir, 0700)
	configData, _ := json.Marshal(dockerCfg)
	configPath := filepath.Join(configDir, "config.json")
	os.WriteFile(configPath, configData, 0600)

	readBack, _ := os.ReadFile(configPath)
	var readCfg dockerConfigJSON
	json.Unmarshal(readBack, &readCfg)
	if readCfg.Auths["registry.example.com"].Auth != entry.Auth {
		t.Error("written config does not match expected")
	}
}
