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
	os.Unsetenv("DOCKER_CONFIG")
	err := setupRegistryAuth()
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}
	// DOCKER_CONFIG should NOT be set when no secret is mounted
	if got := os.Getenv("DOCKER_CONFIG"); got == registryConfigDir {
		t.Errorf("DOCKER_CONFIG should not be set when secret is absent")
	}
}
