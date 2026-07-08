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
	"errors"
	"os"

	"k8s.io/klog/v2"
)

// registryConfigDir is the directory where the K8s Secret is mounted as config.json.
// The job_template mounts the dockerconfigjson Secret with key mapping:
//
//	.dockerconfigjson -> config.json
//
// This is already a valid Docker config.json that nerdctl can read directly.
const registryConfigDir = "/var/run/secrets/registry"

// setupRegistryAuth sets DOCKER_CONFIG to point at the mounted registry secret directory.
// If no secret is mounted, it does nothing (anonymous push).
func setupRegistryAuth() error {
	return setupRegistryAuthFrom(registryConfigDir)
}

// setupRegistryAuthFrom is the testable implementation that accepts a configDir parameter.
func setupRegistryAuthFrom(configDir string) error {
	configPath := configDir + "/config.json"
	if _, err := os.Stat(configPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			klog.InfoS("No registry secret mounted, skipping auth setup")
			return nil
		}
		return err
	}
	if err := os.Setenv("DOCKER_CONFIG", configDir); err != nil {
		return err
	}
	klog.InfoS("Registry authentication configured", "dir", configDir)
	return nil
}
