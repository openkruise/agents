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

func setEnv(t *testing.T, key, value string) {
	t.Helper()
	original, hadOriginal := os.LookupEnv(key)
	if value == "" {
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	} else {
		if err := os.Setenv(key, value); err != nil {
			t.Fatalf("set %s: %v", key, err)
		}
	}
	t.Cleanup(func() {
		if hadOriginal {
			_ = os.Setenv(key, original)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}

func TestEnvConfig_ContainerID(t *testing.T) {
	setEnv(t, EnvContainerID, "abc123")
	if got := Config().ContainerID(); got != "abc123" {
		t.Errorf("ContainerID()=%q, want %q", got, "abc123")
	}
}

func TestEnvConfig_CommitImage(t *testing.T) {
	setEnv(t, EnvCommitImage, "registry.example.com/app:v1")
	if got := Config().CommitImage(); got != "registry.example.com/app:v1" {
		t.Errorf("CommitImage()=%q, want %q", got, "registry.example.com/app:v1")
	}
}

func TestEnvConfig_AgentJobImage(t *testing.T) {
	setEnv(t, EnvAgentJobImage, "agent-job:latest")
	if got := Config().AgentJobImage(); got != "agent-job:latest" {
		t.Errorf("AgentJobImage()=%q, want %q", got, "agent-job:latest")
	}
}

func TestEnvConfig_ContainerdSockPath_Default(t *testing.T) {
	setEnv(t, EnvContainerdSockPath, "")
	if got := Config().ContainerdSockPath(); got != "/run/containerd/" {
		t.Errorf("ContainerdSockPath() default=%q, want %q", got, "/run/containerd/")
	}
}

func TestEnvConfig_ContainerdSockPath_Override(t *testing.T) {
	setEnv(t, EnvContainerdSockPath, "/var/run/custom/")
	if got := Config().ContainerdSockPath(); got != "/var/run/custom/" {
		t.Errorf("ContainerdSockPath() override=%q, want %q", got, "/var/run/custom/")
	}
}

func TestEnvConfig_ContainerdSock_Default(t *testing.T) {
	setEnv(t, EnvContainerdSock, "")
	if got := Config().ContainerdSock(); got != "/run/containerd/containerd.sock" {
		t.Errorf("ContainerdSock() default=%q, want %q", got, "/run/containerd/containerd.sock")
	}
}

func TestEnvConfig_ContainerdSock_Override(t *testing.T) {
	setEnv(t, EnvContainerdSock, "/var/run/custom.sock")
	if got := Config().ContainerdSock(); got != "/var/run/custom.sock" {
		t.Errorf("ContainerdSock() override=%q, want %q", got, "/var/run/custom.sock")
	}
}

func TestEnvConfig_ImagePullPolicy_Default(t *testing.T) {
	setEnv(t, EnvAgentJobImagePullPolicy, "")
	if got := Config().ImagePullPolicy(); got != corev1.PullIfNotPresent {
		t.Errorf("ImagePullPolicy() default=%q, want %q", got, corev1.PullIfNotPresent)
	}
}

func TestEnvConfig_ImagePullPolicy_Override(t *testing.T) {
	setEnv(t, EnvAgentJobImagePullPolicy, string(corev1.PullAlways))
	if got := Config().ImagePullPolicy(); got != corev1.PullAlways {
		t.Errorf("ImagePullPolicy() override=%q, want %q", got, corev1.PullAlways)
	}
}

func TestConfig_ReturnsSharedInstance(t *testing.T) {
	if Config() != Config() {
		t.Error("Config() must return the same shared instance on each call")
	}
}
