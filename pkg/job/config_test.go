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

	corev1 "k8s.io/api/core/v1"
)

func TestConfig_Singleton(t *testing.T) {
	c := Config()
	if c == nil {
		t.Fatal("Config() returned nil")
	}
	if c != Config() {
		t.Error("Config() should return the same instance")
	}
}

func TestConfig_ContainerdSock_Default(t *testing.T) {
	os.Unsetenv(EnvContainerdSock)
	if Config().ContainerdSock() != "/run/containerd/containerd.sock" {
		t.Errorf("unexpected default: %s", Config().ContainerdSock())
	}
}

func TestConfig_ContainerdSock_Custom(t *testing.T) {
	os.Setenv(EnvContainerdSock, "/custom/sock")
	defer os.Unsetenv(EnvContainerdSock)
	if Config().ContainerdSock() != "/custom/sock" {
		t.Errorf("unexpected value: %s", Config().ContainerdSock())
	}
}

func TestConfig_ContainerdSockPath_Default(t *testing.T) {
	os.Unsetenv(EnvContainerdSockPath)
	if Config().ContainerdSockPath() != "/run/containerd/" {
		t.Errorf("unexpected default: %s", Config().ContainerdSockPath())
	}
}

func TestConfig_ContainerdSockPath_Custom(t *testing.T) {
	os.Setenv(EnvContainerdSockPath, "/custom/path/")
	defer os.Unsetenv(EnvContainerdSockPath)
	if Config().ContainerdSockPath() != "/custom/path/" {
		t.Errorf("unexpected value: %s", Config().ContainerdSockPath())
	}
}

func TestConfig_ImagePullPolicy_Default(t *testing.T) {
	os.Unsetenv(EnvAgentJobImagePullPolicy)
	if Config().ImagePullPolicy() != corev1.PullIfNotPresent {
		t.Errorf("unexpected default: %s", Config().ImagePullPolicy())
	}
}

func TestConfig_ImagePullPolicy_Custom(t *testing.T) {
	os.Setenv(EnvAgentJobImagePullPolicy, "Always")
	defer os.Unsetenv(EnvAgentJobImagePullPolicy)
	if Config().ImagePullPolicy() != corev1.PullAlways {
		t.Errorf("unexpected value: %s", Config().ImagePullPolicy())
	}
}

func TestConfig_DryRun(t *testing.T) {
	os.Unsetenv(EnvDryRun)
	if Config().DryRun() {
		t.Error("expected false when unset")
	}
	os.Setenv(EnvDryRun, "true")
	defer os.Unsetenv(EnvDryRun)
	if !Config().DryRun() {
		t.Error("expected true when set to 'true'")
	}
}

func TestConfig_EnvGetters(t *testing.T) {
	tests := []struct {
		envKey string
		getter func() string
	}{
		{EnvContainerID, Config().ContainerID},
		{EnvCommitImage, Config().CommitImage},
		{EnvAgentJobImage, Config().AgentJobImage},
	}

	for _, tt := range tests {
		t.Run(tt.envKey, func(t *testing.T) {
			os.Setenv(tt.envKey, "test-value")
			defer os.Unsetenv(tt.envKey)
			got := tt.getter()
			if got != "test-value" {
				t.Errorf("%s getter returned %q, want 'test-value'", tt.envKey, got)
			}
		})
	}
}
