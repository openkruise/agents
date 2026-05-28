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
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	// EnvTargetRegistry is the environment variable for the target registry host
	// used by the prepare-hosts-dir init container.
	EnvTargetRegistry = "TARGET_REGISTRY"

	// defaultRegistryHost is used when no registry host can be extracted from the image reference.
	defaultRegistryHost = "docker.io"

	// containerdCertsDir is the default directory where containerd looks for
	// per-registry TLS certificates and hosts.toml configuration files.
	containerdCertsDir = "/etc/containerd/certs.d"
)

// extractRegistryHost extracts the registry host from an image reference.
// Examples:
//
//	"registry.example.com/repo:tag"       → "registry.example.com"
//	"registry.example.com:5000/repo:tag"  → "registry.example.com:5000"
//	"ubuntu:latest"                       → "docker.io"
//	"library/nginx"                       → "docker.io"
func extractRegistryHost(image string) string {
	// Remove digest
	ref := image
	if atIdx := strings.IndexByte(ref, '@'); atIdx >= 0 {
		ref = ref[:atIdx]
	}
	// For tag, strip from the last component only (after the last slash).
	if slashIdx := strings.IndexByte(ref, '/'); slashIdx >= 0 {
		if colonIdx := strings.LastIndexByte(ref, ':'); colonIdx > slashIdx {
			ref = ref[:colonIdx]
		}
	} else {
		// No slash — "image:tag" or "image". No registry host.
		return defaultRegistryHost
	}

	// The first component before '/' is the registry host if it contains '.' or ':' or is "localhost".
	firstComponent := ref[:strings.IndexByte(ref, '/')]
	if strings.ContainsAny(firstComponent, ".:") || firstComponent == "localhost" {
		return firstComponent
	}
	return defaultRegistryHost
}

// PrepareHostsDirScript returns the shell script executed by the prepare-hosts-dir
// init container. It copies the node's containerd certs.d directory into the
// writable emptyDir and ensures the target registry's hosts.toml contains
// the "push" capability.
func PrepareHostsDirScript() string {
	return `#!/bin/sh
set -e
SRC="/etc/containerd/certs.d.orig"
DST="/etc/containerd/certs.d"

# 1. Copy all registry configs and CA certs from the node
cp -a "$SRC"/. "$DST"/ 2>/dev/null || true

# 2. Ensure the target registry's hosts.toml includes push capability
DIR="$DST/$TARGET_REGISTRY"
mkdir -p "$DIR"
TOML="$DIR/hosts.toml"

if [ ! -f "$TOML" ]; then
  cat > "$TOML" <<EOF
server = "https://$TARGET_REGISTRY"

[host."https://$TARGET_REGISTRY"]
  capabilities = ["pull", "resolve", "push"]
EOF
elif ! grep -q '"push"' "$TOML"; then
  sed -i 's/"resolve"\]/"resolve", "push"]/' "$TOML"
fi
`
}

// initContainerVolumeMounts returns the volume mounts for the prepare-hosts-dir init container.
func initContainerVolumeMounts() []corev1.VolumeMount {
	return []corev1.VolumeMount{
		{
			Name:      "host-containerd-certs",
			MountPath: "/etc/containerd/certs.d.orig",
			ReadOnly:  true,
		},
		{
			Name:      "hosts-dir",
			MountPath: containerdCertsDir,
		},
	}
}
