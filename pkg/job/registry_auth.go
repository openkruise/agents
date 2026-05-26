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
	"fmt"
	"os"
	"path/filepath"

	"k8s.io/klog/v2"
)

const (
	registrySecretPath = "/var/run/secrets/registry/config.json"
	dockerConfigDir    = "/root/.docker"
)

// k8sDockerConfigJSON represents the Kubernetes dockerconfigjson format.
type k8sDockerConfigJSON struct {
	Auths map[string]k8sAuthEntry `json:"auths"`
}

type k8sAuthEntry struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Auth     string `json:"auth"`
}

// dockerConfigJSON represents the standard Docker config.json format.
type dockerConfigJSON struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Auth string `json:"auth"`
}

// setupRegistryAuth reads the mounted Kubernetes dockerconfigjson secret,
// converts it to standard Docker config format, and writes to /root/.docker/config.json.
// It explicitly sets DOCKER_CONFIG env var to ensure nerdctl subprocess can find the config.
func setupRegistryAuth() error {
	return setupRegistryAuthFrom(registrySecretPath, dockerConfigDir)
}

// setupRegistryAuthFrom is the testable implementation of setupRegistryAuth.
func setupRegistryAuthFrom(secretPath, configDir string) error {
	err := os.Setenv("DOCKER_CONFIG", configDir)
	if err != nil {
		return err
	}

	if _, err := os.Stat(secretPath); os.IsNotExist(err) {
		klog.InfoS("No registry secret mounted, skipping auth setup", "path", secretPath)
		return nil
	}

	data, err := os.ReadFile(secretPath)
	if err != nil {
		return fmt.Errorf("failed to read registry secret: %w", err)
	}

	var k8sConfig k8sDockerConfigJSON
	if err := json.Unmarshal(data, &k8sConfig); err != nil {
		return fmt.Errorf("failed to parse registry secret: %w", err)
	}

	dockerConfig := convertK8sDockerConfig(k8sConfig)
	if len(dockerConfig.Auths) == 0 {
		klog.InfoS("No registry credentials found in mounted secret")
		return nil
	}

	if err := os.MkdirAll(configDir, 0700); err != nil {
		return fmt.Errorf("failed to create docker config dir %s: %w", configDir, err)
	}

	configData, err := json.Marshal(dockerConfig)
	if err != nil {
		return fmt.Errorf("failed to marshal docker config: %w", err)
	}

	configPath := filepath.Join(configDir, "config.json")
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		return fmt.Errorf("failed to write docker config to %s: %w", configPath, err)
	}

	klog.InfoS("Registry authentication configured", "configPath", configPath, "servers", len(dockerConfig.Auths))
	return nil
}

// convertK8sDockerConfig converts Kubernetes dockerconfigjson format to standard Docker config.json format.
func convertK8sDockerConfig(k8sConfig k8sDockerConfigJSON) dockerConfigJSON {
	dockerConfig := dockerConfigJSON{Auths: make(map[string]dockerAuthEntry)}
	for server, entry := range k8sConfig.Auths {
		auth := entry.Auth
		if auth == "" && entry.Username != "" && entry.Password != "" {
			auth = base64.StdEncoding.EncodeToString([]byte(entry.Username + ":" + entry.Password))
		} else if auth == "" {
			continue
		}
		dockerConfig.Auths[server] = dockerAuthEntry{Auth: auth}
	}
	return dockerConfig
}
