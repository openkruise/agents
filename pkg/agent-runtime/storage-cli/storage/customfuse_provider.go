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
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

// CustomFuseDriver is the CSI driver name of the generic FUSE plugin. The
// mount-proxy component originates from the alibaba-cloud-csi-driver
// customfuse feature, but the driver is community-owned under the
// openkruise.io prefix. It MUST match both the directory under CsiSocketDir
// created by the csi-sidecar and the driver name used by the control-plane
// storage registration.
const CustomFuseDriver = "customfuseplugin.csi.openkruise.io"

// customFuseSubDir is the sub-directory under the mount root that hosts the
// per-volume mount point for the customfuse driver.
const customFuseSubDir = "customfuse"

// customFuseProvider forwards NodePublishVolume to the csi-sidecar's CSI
// node server over the standard per-driver unix socket. The sidecar performs
// the actual FUSE mount via mount-proxy and the driver entrypoint.
type customFuseProvider struct{}

func init() {
	Register(&customFuseProvider{})
}

func (p *customFuseProvider) Driver() string {
	return CustomFuseDriver
}

func (p *customFuseProvider) SubDir() string {
	return customFuseSubDir
}

// Validate checks the routing fields required to forward a publish request
// to the sidecar — and nothing more. Deep option validation (source,
// fuseType whitelist, credential separation) is done by the control-plane
// CustomFuseMountProvider at claim time; the sidecar CSI node server
// repeats the environment-key checks (Secret keys and mount flags) as
// defense in depth, and the FUSE entrypoint re-validates shell safety of
// the values it interpolates.
func (p *customFuseProvider) Validate(req csi.NodePublishVolumeRequest) error {
	if req.VolumeId == "" {
		return fmt.Errorf("volumeId is required")
	}
	if req.TargetPath == "" {
		return fmt.Errorf("targetPath is required")
	}
	return nil
}

// Mount forwards the request to the sidecar CSI node server via
// RunNodePublishVolume (see csi_runner.go). debug enables printing the
// credential-bearing publish context and must never be enabled in
// production.
func (p *customFuseProvider) Mount(ctx context.Context, req csi.NodePublishVolumeRequest, debug bool) error {
	return RunNodePublishVolume(ctx, p.Driver(), req, debug)
}

// Unmount is not implemented yet; the sandbox teardown removes the mount
// point together with the sandbox.
func (p *customFuseProvider) Unmount(ctx context.Context, req csi.NodePublishVolumeRequest) error {
	return nil
}
