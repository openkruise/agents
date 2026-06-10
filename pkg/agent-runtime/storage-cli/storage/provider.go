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

package storage

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// Provider is the extension point for one storage driver (e.g. a CSI plugin).
//
// Implementations are expected to self-register from their package init() via
// Register, so that the main binary stays free of vendor-specific identifiers
// and can pick up a driver implementation purely by blank-importing its
// package.
type Provider interface {
	// Driver returns the CSI driver name, e.g. "nasplugin.example.com".
	// It MUST match the directory name under CsiSocketDir.
	Driver() string

	// SubDir returns the sub-directory under the mount root used to host
	// the per-volume mount point for this driver, e.g. "nas" or "oss".
	SubDir() string

	// Validate checks driver-specific fields on the CSI request before
	// the mount is attempted. It MUST NOT mutate the request.
	Validate(req csi.NodePublishVolumeRequest) error

	// Mount performs the driver-specific mount. The default open-source
	// implementation forwards to the CSI plugin via NodePublishVolume; see
	// RunNodePublishVolume.
	//
	// debug controls whether sensitive fields (e.g. PublishContext) are included
	// in log output. Pass true only in non-production environments.
	Mount(ctx context.Context, req csi.NodePublishVolumeRequest, debug bool) error

	// Unmount performs the driver-specific unmount. Drivers that have not
	// implemented unmount yet may return nil.
	Unmount(ctx context.Context, req csi.NodePublishVolumeRequest) error
}
