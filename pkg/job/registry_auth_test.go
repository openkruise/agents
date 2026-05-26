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

func TestSetupRegistryAuthFrom_NoSecretFile(t *testing.T) {
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "nonexistent", "config.json")
	configDir := filepath.Join(tmpDir, "docker")

	err := setupRegistryAuthFrom(secretPath, configDir)
	if err != nil {
		t.Errorf("should not error when secret file is missing: %v", err)
	}
}

func TestSetupRegistryAuthFrom_InvalidJSON(t *testing.T) {
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "config.json")
	os.WriteFile(secretPath, []byte("not-json"), 0600)
	configDir := filepath.Join(tmpDir, "docker")

	err := setupRegistryAuthFrom(secretPath, configDir)
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestSetupRegistryAuthFrom_EmptyCredentials(t *testing.T) {
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "config.json")
	data, _ := json.Marshal(k8sDockerConfigJSON{Auths: map[string]k8sAuthEntry{"empty.io": {}}})
	os.WriteFile(secretPath, data, 0600)
	configDir := filepath.Join(tmpDir, "docker")

	err := setupRegistryAuthFrom(secretPath, configDir)
	if err != nil {
		t.Errorf("should not error for empty credentials: %v", err)
	}
	// config.json should NOT be written since no valid credentials
	if _, err := os.Stat(filepath.Join(configDir, "config.json")); !os.IsNotExist(err) {
		t.Error("config.json should not be created for empty credentials")
	}
}

func TestSetupRegistryAuthFrom_WritesConfig(t *testing.T) {
	tmpDir := t.TempDir()
	secretPath := filepath.Join(tmpDir, "config.json")
	k8sCfg := k8sDockerConfigJSON{
		Auths: map[string]k8sAuthEntry{
			"registry.io": {Username: "user", Password: "pass"},
		},
	}
	data, _ := json.Marshal(k8sCfg)
	os.WriteFile(secretPath, data, 0600)
	configDir := filepath.Join(tmpDir, "docker")

	err := setupRegistryAuthFrom(secretPath, configDir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	written, err := os.ReadFile(filepath.Join(configDir, "config.json"))
	if err != nil {
		t.Fatalf("config.json not written: %v", err)
	}
	var cfg dockerConfigJSON
	json.Unmarshal(written, &cfg)
	if _, ok := cfg.Auths["registry.io"]; !ok {
		t.Error("expected registry.io in written config")
	}
}

func TestConvertK8sDockerConfig_TableDriven(t *testing.T) {
	tests := []struct {
		name          string
		input         k8sDockerConfigJSON
		expectEntries int
		expectAuth    map[string]string // server -> expected auth value
	}{
		{
			name: "auth field takes priority over username/password",
			input: k8sDockerConfigJSON{
				Auths: map[string]k8sAuthEntry{
					"registry.io": {Auth: "precomputed-auth", Username: "user", Password: "pass"},
				},
			},
			expectEntries: 1,
			expectAuth:    map[string]string{"registry.io": "precomputed-auth"},
		},
		{
			name: "only username without password is skipped",
			input: k8sDockerConfigJSON{
				Auths: map[string]k8sAuthEntry{
					"partial.io": {Username: "onlyuser"},
				},
			},
			expectEntries: 0,
		},
		{
			name: "only password without username is skipped",
			input: k8sDockerConfigJSON{
				Auths: map[string]k8sAuthEntry{
					"partial.io": {Password: "onlypass"},
				},
			},
			expectEntries: 0,
		},
		{
			name: "empty auths map",
			input: k8sDockerConfigJSON{
				Auths: map[string]k8sAuthEntry{},
			},
			expectEntries: 0,
		},
		{
			name:          "nil auths map",
			input:         k8sDockerConfigJSON{},
			expectEntries: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertK8sDockerConfig(tt.input)
			if len(result.Auths) != tt.expectEntries {
				t.Errorf("expected %d entries, got %d", tt.expectEntries, len(result.Auths))
			}
			for server, expectedAuth := range tt.expectAuth {
				if result.Auths[server].Auth != expectedAuth {
					t.Errorf("server %s: auth = %s, want %s", server, result.Auths[server].Auth, expectedAuth)
				}
			}
		})
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
