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
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// fakeProxyMounter records the forwarded request and returns a configurable
// error.
type fakeProxyMounter struct {
	gotReq *proxyMountRequest
	err    error
}

func (f *fakeProxyMounter) Mount(_ context.Context, req *proxyMountRequest) error {
	f.gotReq = req
	return f.err
}

// withProxyClientFactory swaps newProxyClientFn for the duration of fn.
func withProxyClientFactory(t *testing.T, factory func(socketPath string) proxyMounter) {
	t.Helper()
	orig := newProxyClientFn
	newProxyClientFn = factory
	t.Cleanup(func() { newProxyClientFn = orig })
}

// withTempMountRoot points mountRootPrefix at a temporary directory for the
// duration of the test, so filesystem-mutating tests never touch the real
// /run path (the CI runner is unprivileged there). On Windows the temp dir
// carries a drive prefix, which validateTargetPath rejects under its POSIX
// path.IsAbs semantics; stripping the volume name keeps the path
// POSIX-absolute while still resolving to the same physical directory.
func withTempMountRoot(t *testing.T) {
	t.Helper()
	dir := filepath.ToSlash(t.TempDir())
	if vol := filepath.VolumeName(t.TempDir()); vol != "" {
		dir = strings.TrimPrefix(dir, vol)
	}
	// path.Clean collapses a doubled leading slash, which would break the
	// HasPrefix comparison in validateTargetPath; keep exactly one.
	if !strings.HasPrefix(dir, "/") {
		dir = "/" + dir
	}
	mountRootPrefix = dir + "/mount-root" + "/"
	t.Cleanup(func() { mountRootPrefix = "/run/csi/mount-root/" })
}

func TestValidateTargetPath(t *testing.T) {
	tests := []struct {
		name        string
		targetPath  string
		expectError string
	}{
		{name: "absolute path passes", targetPath: "/run/csi/mount-root/customfuse/abc123"},
		{name: "nested absolute path passes", targetPath: "/run/csi/mount-root/customfuse/abc123/sub"},
		{name: "empty path is rejected", targetPath: "", expectError: "empty"},
		{name: "whitespace path is rejected", targetPath: "   ", expectError: "empty"},
		{name: "relative path is rejected", targetPath: "workspace/data", expectError: "not an absolute path"},
		{name: "null byte is rejected", targetPath: "/run/csi/mount-root/x\x00y", expectError: "null byte"},
		{name: "path outside mount root is rejected", targetPath: "/workspace/data", expectError: "outside the mount root"},
		{name: "path outside mount root with dotdot is rejected", targetPath: "/run/../etc/passwd", expectError: "outside the mount root"},
		{name: "dotdot escape from mount root is rejected", targetPath: "/run/csi/mount-root/../customfuse/x", expectError: "outside the mount root"},
		{name: "mount root itself is rejected", targetPath: "/run/csi/mount-root", expectError: "outside the mount root"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTargetPath(tt.targetPath)
			if tt.expectError == "" {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
			}
		})
	}
}

func TestNodePublishVolume(t *testing.T) {
	// Set up the temporary mount root BEFORE building validReq: the target
	// path expressions capture mountRootPrefix at construction time, so
	// overriding it later would leave the request pointing at /run/csi.
	withTempMountRoot(t)

	validReq := &csi.NodePublishVolumeRequest{
		VolumeId:   "vol-1",
		TargetPath: mountRootPrefix + "customfuse/vol-1",
		VolumeContext: map[string]string{
			"source":   "redis://redis-cluster:6379/0",
			"fuseType": "juicefs",
			"bucket":   "ml-datasets",
		},
		Secrets: map[string]string{"token": "secret-token"},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{FsType: "juicefs"},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	}

	tests := []struct {
		name       string
		req        *csi.NodePublishVolumeRequest
		proxyErr   error
		expectCode codes.Code
		assertReq  func(t *testing.T, got *proxyMountRequest)
	}{
		{
			name: "success forwards mount request",
			req:  validReq,
			assertReq: func(t *testing.T, got *proxyMountRequest) {
				require.NotNil(t, got)
				assert.Equal(t, "vol-1", got.VolumeID)
				assert.Equal(t, "redis://redis-cluster:6379/0", got.Source)
				assert.Equal(t, mountRootPrefix+"customfuse/vol-1", got.Target)
				assert.Equal(t, customFuseFsType, got.Fstype)
				assert.Equal(t, []string{"bucket=ml-datasets"}, got.Options)
				assert.Equal(t, map[string]string{"token": "secret-token"}, got.Secrets)
			},
		},
		{
			name: "missing volume capability is invalid argument",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				VolumeContext: map[string]string{
					"source": "redis://h:6379/0",
				},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			// Symmetry with the provider: a reserved key smuggled through
			// otherOpts must be rejected on the direct-socket path too.
			name: "otherOpts reserved key rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				VolumeContext: map[string]string{
					"source":    "redis://h:6379/0",
					"otherOpts": "cache-size=1024,url=http://attacker:9000",
				},
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
					},
				},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name:       "proxy error maps to internal",
			req:        validReq,
			proxyErr:   errors.New("entrypoint exited: boom"),
			expectCode: codes.Internal,
		},
		{
			name:       "empty target path is invalid argument",
			req:        &csi.NodePublishVolumeRequest{VolumeId: "vol-1"},
			expectCode: codes.InvalidArgument,
		},
		{
			name:       "empty volume id is invalid argument",
			req:        &csi.NodePublishVolumeRequest{TargetPath: "/run/csi/mount-root/customfuse/vol-1"},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "block volume capability rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				VolumeContext: map[string]string{
					"source": "redis://h:6379/0",
				},
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Block{
						Block: &csi.VolumeCapability_BlockVolume{},
					},
				},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "volume context BASH_ENV key rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				VolumeContext: map[string]string{
					"source":   "redis://h:6379/0",
					"BASH_ENV": "/tmp/x.sh",
				},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			// A dangerous key smuggled through volumeAttributes.mountOptions
			// entries must be caught by the merged MountOptions validation.
			name: "volume attributes mountOptions dangerous key rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				VolumeContext: map[string]string{
					"source":       "redis://h:6379/0",
					"mountOptions": "cache-size=1024 BASH_ENV=/tmp/x.sh",
				},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "invalid capacity is invalid argument",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:      "vol-1",
				TargetPath:    "/run/csi/mount-root/customfuse/vol-1",
				VolumeContext: map[string]string{"capacity": "bad"},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "unsupported authType is invalid argument",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:      "vol-1",
				TargetPath:    "/run/csi/mount-root/customfuse/vol-1",
				VolumeContext: map[string]string{"source": "redis://h:6379/0", "authType": "rrsa"},
			},
			expectCode: codes.InvalidArgument,
		},

		// Attack surface: the unix socket is reachable from inside the
		// sandbox, so Secrets and MountFlags can arrive without passing
		// through the control-plane provider. mount-proxy exports each entry
		// as an env var of the same name into the entrypoint shell; the
		// entrypoint's own unset runs too late for its starting shell
		// (BASH_ENV is sourced at bash startup, LD_PRELOAD at exec time), so
		// the node server must reject them here.
		{
			name: "secret BASH_ENV rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				Secrets:    map[string]string{"BASH_ENV": "/tmp/x.sh"},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "secret LD_PRELOAD rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				Secrets:    map[string]string{"LD_PRELOAD": "/tmp/x.so"},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "secret reserved source key rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				Secrets:    map[string]string{"source": "redis://evil:6379/0"},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "secret invalid key name rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				Secrets:    map[string]string{"access-key": "AKID"},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "mount flag BASH_ENV rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							MountFlags: []string{"BASH_ENV=/tmp/x.sh"},
						},
					},
				},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "mount flag reserved url key rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							MountFlags: []string{"url=http://attacker:9000"},
						},
					},
				},
			},
			expectCode: codes.InvalidArgument,
		},
		{
			name: "safe mount flags forwarded",
			req: &csi.NodePublishVolumeRequest{
				VolumeId:   "vol-1",
				TargetPath: mountRootPrefix + "customfuse/vol-1",
				VolumeContext: map[string]string{
					"source": "redis://h:6379/0",
				},
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							MountFlags: []string{"cache-size=1024"},
						},
					},
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
					},
				},
			},
			assertReq: func(t *testing.T, got *proxyMountRequest) {
				require.NotNil(t, got)
				assert.Contains(t, got.Options, "cache-size=1024")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &fakeProxyMounter{err: tt.proxyErr}
			withProxyClientFactory(t, func(socketPath string) proxyMounter {
				assert.Equal(t, "/var/run/csi/mounter.sock", socketPath)
				return fake
			})

			ns := NewNodeServer("/var/run/csi/mounter.sock")
			_, err := ns.NodePublishVolume(context.Background(), tt.req)

			if tt.expectCode != 0 {
				require.Error(t, err)
				assert.Equal(t, tt.expectCode, status.Code(err))
				return
			}
			require.NoError(t, err)
			if tt.assertReq != nil {
				tt.assertReq(t, fake.gotReq)
			}
		})
	}
}

// TestNodePublishVolumeSerializesConcurrentRequests verifies the per-volume
// lock rejects a second in-flight request for the same volume.
func TestNodePublishVolumeSerializesConcurrentRequests(t *testing.T) {
	blocked := make(chan struct{})
	unblock := make(chan struct{})
	fake := &fakeProxyMounter{
		err: nil,
	}
	orig := newProxyClientFn
	newProxyClientFn = func(_ string) proxyMounter {
		return &blockingMounter{inner: fake, blocked: blocked, unblock: unblock}
	}
	defer func() { newProxyClientFn = orig }()

	withTempMountRoot(t)

	ns := NewNodeServer("/var/run/csi/mounter.sock")
	req := &csi.NodePublishVolumeRequest{
		VolumeId:   "vol-lock",
		TargetPath: mountRootPrefix + "customfuse/vol-1",
		VolumeContext: map[string]string{
			"source": "redis://h:6379/0",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
			},
		},
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_, err := ns.NodePublishVolume(context.Background(), req)
		if err != nil {
			t.Logf("first call failed early: %v", err)
		}
	}()
	<-blocked // first request holds the lock

	_, err := ns.NodePublishVolume(context.Background(), req)
	require.Error(t, err)
	assert.Equal(t, codes.Aborted, status.Code(err))

	close(unblock)
	<-done
}

type blockingMounter struct {
	inner   *fakeProxyMounter
	blocked chan struct{}
	unblock chan struct{}
}

func (b *blockingMounter) Mount(ctx context.Context, req *proxyMountRequest) error {
	close(b.blocked)
	<-b.unblock
	return b.inner.Mount(ctx, req)
}

func TestNodeUnpublishVolumeAccepted(t *testing.T) {
	ns := NewNodeServer("/var/run/csi/mounter.sock")
	resp, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "vol-1",
		TargetPath: mountRootPrefix + "customfuse/vol-1",
	})
	require.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestNodeGetCapabilities(t *testing.T) {
	ns := NewNodeServer("/var/run/csi/mounter.sock")
	resp, err := ns.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	require.NoError(t, err)
	// Staging is a no-op here and volumes are published directly on the
	// target path; declaring STAGE_UNSTAGE would invite standard CSI
	// consumers to drive a flow this server does not implement.
	assert.Empty(t, resp.Capabilities)
}
