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

func TestEnvConfig(t *testing.T) {
	tests := []struct {
		name     string
		envKey   string
		envValue string
		got      string
		want     string
	}{
		{name: "ContainerID", envKey: EnvContainerID, envValue: "abc123", want: "abc123"},
		{name: "CommitImage", envKey: EnvCommitImage, envValue: "registry.example.com/app:v1", want: "registry.example.com/app:v1"},
		{name: "AgentJobImage", envKey: EnvAgentJobImage, envValue: "agent-job:latest", want: "agent-job:latest"},
		{name: "ContainerdSockPath default", envKey: EnvContainerdSockPath, envValue: "", want: "/run/containerd/"},
		{name: "ContainerdSockPath override", envKey: EnvContainerdSockPath, envValue: "/var/run/custom/", want: "/var/run/custom/"},
		{name: "ContainerdSock default", envKey: EnvContainerdSock, envValue: "", want: "/run/containerd/containerd.sock"},
		{name: "ContainerdSock override", envKey: EnvContainerdSock, envValue: "/var/run/custom.sock", want: "/var/run/custom.sock"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.envKey, tt.envValue)
			switch tt.name {
			case "ContainerID":
				tt.got = Config().ContainerID()
			case "CommitImage":
				tt.got = Config().CommitImage()
			case "AgentJobImage":
				tt.got = Config().AgentJobImage()
			case "ContainerdSockPath default", "ContainerdSockPath override":
				tt.got = Config().ContainerdSockPath()
			case "ContainerdSock default", "ContainerdSock override":
				tt.got = Config().ContainerdSock()
			}
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestEnvConfig_ImagePullPolicy(t *testing.T) {
	tests := []struct {
		name     string
		envValue string
		want     corev1.PullPolicy
	}{
		{name: "default", envValue: "", want: corev1.PullIfNotPresent},
		{name: "override", envValue: string(corev1.PullAlways), want: corev1.PullAlways},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, EnvAgentJobImagePullPolicy, tt.envValue)
			if got := Config().ImagePullPolicy(); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfig_ReturnsSharedInstance(t *testing.T) {
	if Config() != Config() {
		t.Error("Config() must return the same shared instance on each call")
	}
}
