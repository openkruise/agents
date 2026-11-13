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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseOptions(t *testing.T) {
	tests := []struct {
		name                  string
		req                   *csi.NodePublishVolumeRequest
		want                  fuseOptions
		expectError           string
		expectErrorNotContain []string
	}{
		{
			name: "full attribute set is parsed",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{
					"source":      "redis://redis-cluster:6379/0",
					"bucket":      "ml-datasets",
					"url":         "https://s3.amazonaws.com",
					"path":        "/user-123",
					"otherOpts":   "cache-size=1024",
					"fuseType":    "juicefs",
					"capacity":    "100Gi",
					"storageType": "oss",
					"readOnly":    "true",
					"fuseExtra1":  "ignored",
				},
				VolumeCapability: &csi.VolumeCapability{
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
					},
				},
				Readonly: true,
			},
			want: fuseOptions{
				Source:       "redis://redis-cluster:6379/0",
				Bucket:       "ml-datasets",
				URL:          "https://s3.amazonaws.com",
				Path:         "/user-123",
				OtherOpts:    "cache-size=1024",
				FuseType:     "juicefs",
				Capacity:     "100Gi",
				StorageType:  "oss",
				ReadOnly:     true,
				AuthType:     "",
				MountOptions: nil,
			},
		},
		{
			name: "keys are matched case-insensitively and trimmed",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{
					"  Source  ": "  redis://h:6379/1  ",
					"FUSETYPE":   "JUICEfs",
				},
			},
			want: fuseOptions{
				Source:   "redis://h:6379/1",
				FuseType: "JUICEfs",
			},
		},
		{
			name: "empty values are skipped",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{
					"source":   "",
					"bucket":   "",
					"fuseType": " ",
				},
			},
			want: fuseOptions{FuseType: customFuseFsType},
		},
		{
			name: "default fuseType is customfuse",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"source": "redis://h:6379/0"},
			},
			want: fuseOptions{
				Source:   "redis://h:6379/0",
				FuseType: customFuseFsType,
			},
		},
		{
			name: "mountFlags become mount options",
			req: &csi.NodePublishVolumeRequest{
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{
							FsType:     "juicefs",
							MountFlags: []string{"cache-size=1024", "foreground"},
						},
					},
				},
			},
			want: fuseOptions{
				FuseType:     "juicefs",
				MountOptions: []string{"cache-size=1024", "foreground"},
			},
		},
		{
			name: "fsType from mount capability overrides default",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"source": "s3://b"},
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{FsType: "s3fs"},
					},
				},
			},
			want: fuseOptions{
				Source:   "s3://b",
				FuseType: "s3fs",
			},
		},
		{
			name: "conflicting fuseType and fsType are rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"fuseType": "juicefs"},
				VolumeCapability: &csi.VolumeCapability{
					AccessType: &csi.VolumeCapability_Mount{
						Mount: &csi.VolumeCapability_MountVolume{FsType: "s3fs"},
					},
				},
			},
			expectError: "conflicts with fsType",
		},
		{
			name: "read-only access mode sets ReadOnly",
			req: &csi.NodePublishVolumeRequest{
				VolumeCapability: &csi.VolumeCapability{
					AccessMode: &csi.VolumeCapability_AccessMode{
						Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
					},
				},
			},
			want: fuseOptions{FuseType: customFuseFsType, ReadOnly: true},
		},
		{
			name: "bucket and path synthesize source",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"bucket": "bkt", "path": "/sub"},
			},
			want: fuseOptions{
				Bucket:   "bkt",
				Path:     "/sub",
				Source:   "bkt:/sub",
				FuseType: customFuseFsType,
			},
		},
		{
			name: "bucket alone synthesizes source",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"bucket": "bkt"},
			},
			want: fuseOptions{
				Bucket:   "bkt",
				Source:   "bkt",
				FuseType: customFuseFsType,
			},
		},
		{
			name: "capacity with units is validated",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"capacity": "not-a-quantity"},
			},
			expectError: "invalid capacity",
		},
		{
			name: "plain integer capacity passes through",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"capacity": "100"},
			},
			want: fuseOptions{FuseType: customFuseFsType, Capacity: "100"},
		},
		{
			name: "storageType key is case-insensitive",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"STORAGETYPE": "MINIO"},
			},
			want: fuseOptions{FuseType: customFuseFsType, StorageType: "MINIO"},
		},
		{
			name: "source with shell metachar rejected",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"source": "redis://host:6379/0;rm -rf /"},
			},
			expectError: "source contains invalid characters",
		},
		{
			// bucket alone synthesizes source, so the character-set check
			// fires on the synthesized source value.
			name: "bucket with shell metachar rejected via synthesized source",
			req: &csi.NodePublishVolumeRequest{
				VolumeContext: map[string]string{"bucket": "bkt$(whoami)"},
			},
			expectError: "source contains invalid characters",
		},
		{
			name:                  "capacity quantity error masks credential-like value",
			req:                   &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{"capacity": "token=xyz"}},
			expectError:           "invalid capacity",
			expectErrorNotContain: []string{"xyz"},
		},
		{
			name:        "conflicting case variants of the same key rejected",
			req:         &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{"source": "redis://h:6379/0", "Source": "redis://h:6379/1"}},
			expectError: "conflicting values",
		},
		{
			name:        "fractional capacity rejected",
			req:         &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{"capacity": "1.5"}},
			expectError: "invalid capacity",
		},
		{
			name:        "quantity-style capacity outside entrypoint units rejected",
			req:         &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{"capacity": "100m"}},
			expectError: "invalid capacity",
		},
		{
			name: "KiB capacity accepted",
			req:  &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{"capacity": "500Ki"}},
			want: fuseOptions{FuseType: customFuseFsType, Capacity: "500Ki"},
		},
		{
			name:        "unknown fuseType rejected",
			req:         &csi.NodePublishVolumeRequest{VolumeContext: map[string]string{"fuseType": "evil"}},
			expectError: "unknown fuseType",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseOptions(tt.req)
			if tt.expectError != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.expectError)
				for _, s := range tt.expectErrorNotContain {
					assert.NotContains(t, err.Error(), s)
				}
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, *got)
		})
	}
}

func TestPrecheckAuthConfig(t *testing.T) {
	assert.NoError(t, precheckAuthConfig(&fuseOptions{AuthType: ""}))
	err := precheckAuthConfig(&fuseOptions{AuthType: "rrsa"})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unsupported authType")
}

func TestMakeMountOptions(t *testing.T) {
	opts := &fuseOptions{
		Bucket:       "bkt",
		URL:          "https://s3",
		Path:         "/sub",
		ReadOnly:     true,
		OtherOpts:    "cache-size=1024",
		Capacity:     "100Gi",
		StorageType:  "oss",
		MountOptions: []string{"foreground"},
	}
	assert.Equal(t, []string{
		"bucket=bkt",
		"url=https://s3",
		"path=/sub",
		"otherOpts=cache-size=1024",
		"capacity=100Gi",
		"storageType=oss",
		"foreground",
		"readOnly=true",
	}, opts.makeMountOptions())

	empty := &fuseOptions{}
	assert.Empty(t, empty.makeMountOptions())
}

func TestMakeMountOptionsReadOnlyWins(t *testing.T) {
	// A conflicting readOnly=false in pv.Spec.MountOptions must not weaken
	// the read-only semantics: mount-proxy builds the entrypoint env in
	// slice order and the last duplicate key wins, so readOnly=true is
	// emitted after the MountOptions entries.
	opts := &fuseOptions{
		ReadOnly:     true,
		MountOptions: []string{"readOnly=false", "no-update"},
	}
	assert.Equal(t, []string{
		"readOnly=false",
		"no-update",
		"readOnly=true",
	}, opts.makeMountOptions())

	writable := &fuseOptions{
		MountOptions: []string{"readOnly=true"},
	}
	assert.Equal(t, []string{"readOnly=true"}, writable.makeMountOptions())
}
