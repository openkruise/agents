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
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
)

const (
	// EnvAgentJobImage is the environment variable name for the agent job self-image.
	EnvAgentJobImage = "AGENT_JOB_IMAGE"

	// EnvContainerID is the environment variable name for the commit target container ID.
	EnvContainerID = "COMMIT_CONTAINER_ID"
	// EnvContainerName is the environment variable name for the commit target container name.
	EnvContainerName = "COMMIT_CONTAINER_NAME"
	// EnvCommitImage is the environment variable name for the commit target image.
	EnvCommitImage = "COMMIT_IMAGE"
	// EnvContainerdSock is the environment variable name for the containerd socket path.
	EnvContainerdSock = "COMMIT_CONTAINERD_SOCK"
	// EnvContainerdSockPath is the environment variable name for the containerd socket dir.
	EnvContainerdSockPath = "COMMIT_CONTAINERD_SOCK_PATH"

	// EnvCommitNamespace is the environment variable name for the commit cr namespace.
	EnvCommitNamespace = "COMMIT_NAMESPACE"
	// EnvCommitName is the environment variable name for the commit cr name.
	EnvCommitName = "COMMIT_NAME"

	// EnvCommitPodName is the environment variable name for the commit pod name.
	EnvCommitPodName = "COMMIT_POD_NAME"
	// EnvCommitPodNamespace is the environment variable name for the commit pod namespace.
	EnvCommitPodNamespace = "COMMIT_POD_NAMESPACE"
	// EnvCommitPodUID is the environment variable name for the commit pod uid.
	EnvCommitPodUID = "COMMIT_POD_UID"

	// EnvAgentJobActionKey is the environment variable name for the job action.
	EnvAgentJobActionKey = "ACTION"
	// EnvAgentJobActionCommit is the environment variable value for the commit job action.
	EnvAgentJobActionCommit = "COMMIT"

	// EnvAgentJobImagePullPolicy is the environment variable name for the agent job image pull policy.
	EnvAgentJobImagePullPolicy = "AGENT_JOB_IMAGE_PULL_POLICY"
)

var agentJobImage string

func init() {
	flag.StringVar(&agentJobImage, "agent-job-image", "", "Image for commit job pods. Defaults to AGENT_JOB_IMAGE env.")
}

// EnvConfig reads configuration from environment variables.
type EnvConfig struct{}

func (c *EnvConfig) ContainerID() string { return os.Getenv(EnvContainerID) }
func (c *EnvConfig) CommitImage() string { return os.Getenv(EnvCommitImage) }
func (c *EnvConfig) AgentJobImage() string {
	if agentJobImage != "" {
		return agentJobImage
	}
	return os.Getenv(EnvAgentJobImage)
}

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
