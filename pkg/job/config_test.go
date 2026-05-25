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

func TestConfig_DiskSpaceCheckEnabled(t *testing.T) {
	os.Unsetenv(EnvDiskSpaceCheckEnabled)
	if Config().DiskSpaceCheckEnabled() {
		t.Error("expected false when unset")
	}
	os.Setenv(EnvDiskSpaceCheckEnabled, "true")
	defer os.Unsetenv(EnvDiskSpaceCheckEnabled)
	if !Config().DiskSpaceCheckEnabled() {
		t.Error("expected true")
	}
}

func TestConfig_DiskSpaceSafetyFactor_Default(t *testing.T) {
	os.Unsetenv(EnvDiskSpaceSafetyFactor)
	if Config().DiskSpaceSafetyFactor() != 2.0 {
		t.Errorf("unexpected default: %f", Config().DiskSpaceSafetyFactor())
	}
}

func TestConfig_DiskSpaceSafetyFactor_Custom(t *testing.T) {
	os.Setenv(EnvDiskSpaceSafetyFactor, "3.5")
	defer os.Unsetenv(EnvDiskSpaceSafetyFactor)
	if Config().DiskSpaceSafetyFactor() != 3.5 {
		t.Errorf("unexpected value: %f", Config().DiskSpaceSafetyFactor())
	}
}

func TestConfig_DiskSpaceSafetyFactor_Invalid(t *testing.T) {
	os.Setenv(EnvDiskSpaceSafetyFactor, "notanumber")
	defer os.Unsetenv(EnvDiskSpaceSafetyFactor)
	// Should fall back to default
	if Config().DiskSpaceSafetyFactor() != 2.0 {
		t.Errorf("expected fallback to 2.0, got %f", Config().DiskSpaceSafetyFactor())
	}
}

func TestConfig_ContainerdRootPath_Default(t *testing.T) {
	os.Unsetenv(EnvContainerdRootPath)
	if Config().ContainerdRootPath() != "/var/lib/containerd" {
		t.Errorf("unexpected default: %s", Config().ContainerdRootPath())
	}
}

func TestConfig_ContainerdRootPath_Custom(t *testing.T) {
	os.Setenv(EnvContainerdRootPath, "/data/containerd")
	defer os.Unsetenv(EnvContainerdRootPath)
	if Config().ContainerdRootPath() != "/data/containerd" {
		t.Errorf("unexpected value: %s", Config().ContainerdRootPath())
	}
}

func TestConfig_EnvGetters(t *testing.T) {
	tests := []struct {
		envKey string
		setter func(string)
		getter func() string
		value  string
	}{
		{EnvContainerID, func(v string) { os.Setenv(EnvContainerID, v) }, Config().ContainerID, ""},
		{EnvContainerName, func(v string) { os.Setenv(EnvContainerName, v) }, Config().ContainerName, ""},
		{EnvCommitImage, func(v string) { os.Setenv(EnvCommitImage, v) }, Config().CommitImage, ""},
		{EnvCommitNamespace, func(v string) { os.Setenv(EnvCommitNamespace, v) }, Config().CommitNamespace, ""},
		{EnvCommitName, func(v string) { os.Setenv(EnvCommitName, v) }, Config().CommitName, ""},
		{EnvControllerNamespace, func(v string) { os.Setenv(EnvControllerNamespace, v) }, Config().ControllerNamespace, ""},
		{EnvPushSecretName, func(v string) { os.Setenv(EnvPushSecretName, v) }, Config().PushSecretName, ""},
		{EnvCommitPodName, func(v string) { os.Setenv(EnvCommitPodName, v) }, Config().CommitPodName, ""},
		{EnvCommitPodNamespace, func(v string) { os.Setenv(EnvCommitPodNamespace, v) }, Config().CommitPodNamespace, ""},
		{EnvCommitPodUID, func(v string) { os.Setenv(EnvCommitPodUID, v) }, Config().CommitPodUID, ""},
		{EnvAgentJobImage, func(v string) { os.Setenv(EnvAgentJobImage, v) }, Config().AgentJobImage, ""},
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

func TestConfig_EnvGetters_Unset(t *testing.T) {
	tests := []struct {
		name   string
		envKey string
		getter func() string
	}{
		{"ControllerNamespace unset", EnvControllerNamespace, Config().ControllerNamespace},
		{"PushSecretName unset", EnvPushSecretName, Config().PushSecretName},
		{"CommitPodName unset", EnvCommitPodName, Config().CommitPodName},
		{"CommitPodNamespace unset", EnvCommitPodNamespace, Config().CommitPodNamespace},
		{"CommitPodUID unset", EnvCommitPodUID, Config().CommitPodUID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Unsetenv(tt.envKey)
			got := tt.getter()
			if got != "" {
				t.Errorf("%s getter returned %q when env unset, want empty string", tt.name, got)
			}
		})
	}
}
