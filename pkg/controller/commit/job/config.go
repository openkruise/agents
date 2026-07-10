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

	corev1 "k8s.io/api/core/v1"
)

const (
	// EnvAgentJobImage is the environment variable name for the agent job self-image.
	EnvAgentJobImage = "AGENT_JOB_IMAGE"

	// EnvContainerdSock is the environment variable name for the containerd socket path.
	EnvContainerdSock = "COMMIT_CONTAINERD_SOCK"
	// EnvContainerdSockPath is the environment variable name for the containerd socket dir.
	EnvContainerdSockPath = "COMMIT_CONTAINERD_SOCK_PATH"

	// ArgContainerID is the CLI argument name for the commit target container ID.
	ArgContainerID = "container-id"
	// ArgImage is the CLI argument name for the commit target image.
	ArgImage = "image"

	// EnvAgentJobImagePullPolicy is the environment variable name for the agent job image pull policy.
	EnvAgentJobImagePullPolicy = "AGENT_JOB_IMAGE_PULL_POLICY"

	// DefaultNerdctlHostsDir is the directory nerdctl loads containerd hosts.toml
	// registry configs from (passed as --hosts-dir).
	DefaultNerdctlHostsDir = "/etc/containerd/certs.d"
)

// EnvConfig reads configuration from environment variables.
type EnvConfig struct{}

func (c *EnvConfig) AgentJobImage() string { return os.Getenv(EnvAgentJobImage) }

func (c *EnvConfig) ContainerdSockPath() string {
	if sock := os.Getenv(EnvContainerdSockPath); sock != "" {
		return sock
	}
	return "/run/containerd/"
}

func (c *EnvConfig) ContainerdSock() string {
	if sock := os.Getenv(EnvContainerdSock); sock != "" {
		return sock
	}
	return "/run/containerd/containerd.sock"
}

func (c *EnvConfig) ImagePullPolicy() corev1.PullPolicy {
	if p := os.Getenv(EnvAgentJobImagePullPolicy); p != "" {
		return corev1.PullPolicy(p)
	}
	return corev1.PullIfNotPresent
}

var defaultConfig = &EnvConfig{}

// Config returns the shared EnvConfig instance.
func Config() *EnvConfig {
	return defaultConfig
}
