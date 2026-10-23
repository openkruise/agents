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

package customfusesidecar

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/agent-runtime/customfusevalidate"
)

// proxyMounter is the mount-proxy client surface used by the node server.
type proxyMounter interface {
	Mount(ctx context.Context, req *proxyMountRequest) error
}

// newProxyClientFn is the indirection used by the node server to obtain a
// proxy client. It is a package variable so tests can substitute a fake
// without binding to a real unix socket. Production code MUST NOT reassign
// it.
var newProxyClientFn = func(socketPath string) proxyMounter {
	return newProxyClient(socketPath)
}

// nodeServer implements csi.NodeServer for the customfuse driver inside the
// sandbox sidecar. It mounts directly on the target path (no global mount +
// bind), because the target already lives inside the sandbox mount namespace.
type nodeServer struct {
	csi.UnimplementedNodeServer

	locks         *volumeLocks
	proxySockPath string
}

// NewNodeServer returns a NodeServer that forwards mount requests to the
// mount-proxy-server listening on proxySockPath.
func NewNodeServer(proxySockPath string) csi.NodeServer {
	return &nodeServer{
		locks:         newVolumeLocks(),
		proxySockPath: proxySockPath,
	}
}

func (ns *nodeServer) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	// No STAGE_UNSTAGE capability: staging is a no-op here and volumes are
	// published directly on the target path (the target already lives in
	// the sandbox mount namespace). Declaring the capability would invite
	// standard CSI consumers to drive a staging flow that this server does
	// not implement — and, matching CSI conventions, undeclared RPCs
	// return Unimplemented via the embedded UnimplementedNodeServer.
	return &csi.NodeGetCapabilitiesResponse{}, nil
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	// volume_capability is required by the CSI spec; reject a missing one
	// instead of falling through with default mount semantics.
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability is required")
	}
	if !ns.locks.TryAcquire(req.GetVolumeId()) {
		return nil, status.Errorf(codes.Aborted, "There is already an operation for %s", req.GetVolumeId())
	}
	defer ns.locks.Release(req.GetVolumeId())

	targetPath := req.GetTargetPath()
	if err := validateTargetPath(targetPath); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	// All validation runs before any filesystem mutation so a rejected
	// request leaves no empty directories behind.
	opts, err := parseOptions(req)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "failed to parse options: %v", err)
	}
	if opts.Source == "" {
		return nil, status.Error(codes.InvalidArgument, "source is required (via source, or bucket with optional path)")
	}
	if err := precheckAuthConfig(opts); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "auth config error: %v", err)
	}

	// The provider validates Secret keys and mount flags on the control-plane
	// path, but the per-driver unix socket is reachable from inside the
	// sandbox, so requests can arrive without passing through the provider.
	// mount-proxy exports every Secret entry and mount flag as an environment
	// variable of the same name into the entrypoint shell; repeat the checks
	// here because the entrypoint's own unset of dangerous keys happens too
	// late for its starting shell (bash sources BASH_ENV before the script
	// body, the loader consumes LD_PRELOAD before that).
	if err := customfusevalidate.ValidateSecrets(req.GetSecrets()); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid secret: %v", err)
	}
	// VolumeContext keys are checked with the same blocked-key logic the
	// provider applies: mount-proxy does not export them today, but a
	// future revision might.
	for key := range req.GetVolumeContext() {
		if customfusevalidate.IsBlockedEnvKey(key) {
			return nil, status.Errorf(codes.InvalidArgument, "invalid volume context key %q: it would be exported as a dangerous environment variable", key)
		}
	}
	// The merged mount options are validated as a whole: opts.MountOptions
	// carries both volumeAttributes.mountOptions entries (parsed by
	// parseOptions) and VolumeCapability.MountFlags, and every entry becomes
	// one env var in the entrypoint — a dangerous key smuggled through
	// volumeAttributes.mountOptions must be caught here too.
	if err := customfusevalidate.ValidateMountOptions(opts.MountOptions); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid mount option: %v", err)
	}
	// otherOpts travels as a single env var (one string), so run it through
	// the same option-list check: ValidateMountOptions splits on the shared
	// separator set and applies the same key-level rejections (empty key,
	// blocked env keys, reserved, subdir, credential) the provider applies
	// to its option lists. This keeps the direct-socket path symmetric with
	// the control plane.
	if opts.OtherOpts != "" {
		if err := customfusevalidate.ValidateMountOptions([]string{opts.OtherOpts}); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "invalid otherOpts: %v", err)
		}
	}
	if volCap := req.GetVolumeCapability(); volCap != nil {
		switch {
		case volCap.GetMount() != nil:
		case volCap.GetBlock() != nil:
			return nil, status.Error(codes.InvalidArgument, "block volumes are not supported")
		default:
			// CSI requires one of Mount or Block; a request carrying neither
			// is malformed and must not fall through to the mount path.
			return nil, status.Error(codes.InvalidArgument, "volume capability must specify mount or block access type")
		}
		// CSI requires a concrete access mode; UNKNOWN (nil or unset) must
		// not silently derive default read-write semantics.
		if volCap.GetAccessMode().GetMode() == csi.VolumeCapability_AccessMode_UNKNOWN {
			return nil, status.Error(codes.InvalidArgument, "volume capability must specify an access mode")
		}
	}

	// In the standard CSI flow the kubelet creates the target path before
	// calling NodePublishVolume; the storage CLI does not, so create it here.
	if err := os.MkdirAll(targetPath, 0o750); err != nil {
		klog.ErrorS(err, "failed to create target path", "volume", req.GetVolumeId(), "target", targetPath)
		return nil, status.Errorf(codes.Internal, "failed to create target path: %v", err)
	}
	// MkdirAll follows symlinks in existing ancestors; resolve the created
	// path and re-check the prefix so a symlink planted inside the mount
	// root cannot redirect the mount outside it. The check closes the
	// pre-request window; between this resolution and the mount(2) inside
	// mount-proxy a same-pod process that can write to mount-root could
	// still swap in a symlink, but the impact is limited to shadowing
	// sidecar paths with the caller's own volume content (a mount outside
	// mount-root does not propagate out of the sidecar). Skipped on
	// Windows hosts (unit tests): EvalSymlinks returns host-style paths
	// there, and the real sandbox always runs Linux.
	if runtime.GOOS != "windows" {
		resolved, err := filepath.EvalSymlinks(targetPath)
		if err != nil {
			klog.ErrorS(err, "failed to resolve target path", "volume", req.GetVolumeId(), "target", targetPath)
			return nil, status.Errorf(codes.Internal, "failed to resolve target path: %v", err)
		}
		if !strings.HasPrefix(resolved, mountRootPrefix) {
			_ = os.Remove(targetPath)
			return nil, status.Errorf(codes.InvalidArgument, "target path %q resolves outside the mount root %s", targetPath, mountRootPrefix)
		}
	}

	client := newProxyClientFn(ns.proxySockPath)
	if err := client.Mount(ctx, &proxyMountRequest{
		Source:  opts.Source,
		Target:  targetPath,
		Fstype:  customFuseFsType,
		Options: opts.makeMountOptions(),
		Secrets: req.GetSecrets(),
		// VolumeID is part of the mount-proxy protocol (metrics paths and
		// log correlation); forward the CSI request's volume id instead of
		// leaving the field empty.
		VolumeID: req.GetVolumeId(),
	}); err != nil {
		klog.ErrorS(err, "mount-proxy mount failed", "volume", req.GetVolumeId(), "target", targetPath)
		// Leave no residue behind: MkdirAll above created the target; remove
		// it again unless a mount already landed there (Remove then fails
		// with EBUSY, which is fine).
		_ = os.Remove(targetPath)
		return nil, status.Errorf(codes.Internal, "mount-proxy failed: %v", err)
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume id is required")
	}
	if !ns.locks.TryAcquire(req.GetVolumeId()) {
		return nil, status.Errorf(codes.Aborted, "There is already an operation for %s", req.GetVolumeId())
	}
	defer ns.locks.Release(req.GetVolumeId())

	// Unmounting is handled by the sandbox teardown: the mount namespace is
	// discarded together with the sandbox, so there is nothing to clean up
	// here. Accept the request to keep the CSI protocol happy.
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// mountRootPrefix is the sandbox-wide directory that hosts every per-volume
// mount point. The storage CLI relocates the user-visible target to
// <mount-root>/<driver>/<md5> before calling NodePublishVolume, so a request
// whose target resolves outside it can only come from a direct socket caller
// trying to shadow arbitrary sidecar paths. A var, not a const, so tests can
// point it at a temporary directory; production code MUST NOT modify it.
var mountRootPrefix = "/run/csi/mount-root/"

// validateTargetPath rejects empty, relative, null-byte-bearing, or
// mount-root-escaping target paths before they reach the entrypoint.
func validateTargetPath(targetPath string) error {
	if strings.TrimSpace(targetPath) == "" {
		return fmt.Errorf("target path is empty")
	}
	// path.IsAbs uses POSIX semantics, which is what the sandbox container
	// sees; filepath.IsAbs would treat "/..." as relative on Windows hosts.
	if !path.IsAbs(targetPath) {
		return fmt.Errorf("target path %q is not an absolute path", targetPath)
	}
	if strings.ContainsRune(targetPath, '\x00') {
		return fmt.Errorf("target path contains a null byte")
	}
	// path.Clean resolves ".." segments; the cleaned path must stay under the
	// mount root so a malicious request cannot mount over arbitrary sidecar
	// directories (e.g. "/run/../etc/x" -> "/etc/x"). The trailing slash in
	// mountRootPrefix is load-bearing: Clean strips it from the candidate,
	// so the bare mount root itself ("/run/csi/mount-root/") fails the
	// HasPrefix check and is rejected as a mount target.
	cleaned := path.Clean(targetPath)
	if !strings.HasPrefix(cleaned, mountRootPrefix) {
		return fmt.Errorf("target path %q is outside the mount root %s", targetPath, mountRootPrefix)
	}
	return nil
}

// volumeLocks serializes operations per volume id. TryAcquire fails instead
// of blocking so a concurrent request on the same volume is rejected fast.
type volumeLocks struct {
	mu    sync.Mutex
	holds map[string]struct{}
}

func newVolumeLocks() *volumeLocks {
	return &volumeLocks{holds: map[string]struct{}{}}
}

func (l *volumeLocks) TryAcquire(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.holds[id]; ok {
		return false
	}
	l.holds[id] = struct{}{}
	return true
}

func (l *volumeLocks) Release(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.holds, id)
}
