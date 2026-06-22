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

// Package storage defines the open-source extension points for sandbox-storage:
// a Provider interface backed by a registry, plus a generic CSI runner that
// performs NodePublishVolume against any CSI plugin socket. Vendor-specific
// drivers (NAS/OSS/etc.) live outside this package and self-register via
// blank import.
package storage

// Generic CSI socket conventions shared by all CSI plugins.
const (
	// CsiSocketDir is the standard directory under which kubelet-managed CSI
	// plugin sockets are placed. Each driver owns a sub-directory named after
	// its driver name.
	CsiSocketDir = "/var/run/csi/sockets"
	// CsiSocketFile is the standard filename of a CSI plugin socket.
	CsiSocketFile = "csi.sock"
)
