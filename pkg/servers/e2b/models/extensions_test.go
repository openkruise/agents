package models

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
)

func TestParseExtensions(t *testing.T) {
	tests := []struct {
		name            string
		metadata        map[string]string
		wantErr         bool
		expectExtension NewSandboxRequestExtension
	}{
		{
			name:     "nil metadata",
			metadata: nil,
			wantErr:  false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock: true,
			},
		},
		{
			name:     "empty metadata",
			metadata: map[string]string{},
			wantErr:  false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock: true,
			},
		},
		{
			name: "valid image extension",
			metadata: map[string]string{
				ExtensionKeyClaimWithImage: "nginx:latest",
			},
			wantErr: false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock: true,
				InplaceUpdate: InplaceUpdateExtension{
					Image: "nginx:latest",
				},
			},
		},
		{
			name: "create on no stock == true",
			metadata: map[string]string{
				ExtensionKeyCreateOnNoStock: "true",
			},
			wantErr: false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock: true,
			},
		},
		{
			name: "create on no stock == false",
			metadata: map[string]string{
				ExtensionKeyCreateOnNoStock: "false",
			},
			wantErr: false,
		},
		{
			name: "invalid image extension",
			metadata: map[string]string{
				ExtensionKeyClaimWithImage: "invalid:image:name",
			},
			wantErr: true,
		},
		{
			name: "invalid wait ready timeout",
			metadata: map[string]string{
				ExtensionKeyWaitReadyTimeout: "invalid",
			},
			wantErr: true,
		},
		{
			name: "valid wait ready timeout",
			metadata: map[string]string{
				ExtensionKeyWaitReadyTimeout: "1234",
			},
			wantErr: false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock:  true,
				WaitReadySeconds: 1234,
			},
		},
		{
			name: "valid csi mount extension",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
			},
			wantErr: false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock: true,
				CSIMount: CSIMountExtension{
					MountConfigs: []CSIMountConfig{
						{
							PvName:    "test-volume",
							MountPath: "/valid/path",
						},
					},
				},
			},
		},
		{
			name: "invalid csi mount extension - missing volume name",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
			},
			wantErr: true,
		},
		{
			name: "invalid csi mount extension - missing mount point",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
			},
			wantErr: true,
		},
		{
			name: "invalid csi mount extension - invalid mount point",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/invalid/../path",
			},
			wantErr: true,
		},
		{
			name: "invalid claim timeout",
			metadata: map[string]string{
				ExtensionKeyClaimTimeout: "invalid",
			},
			wantErr: true,
		},
		{
			name: "valid csi mount extension",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
			},
			wantErr: false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock: true,
				CSIMount: CSIMountExtension{
					MountConfigs: []CSIMountConfig{
						{
							PvName:    "test-volume",
							MountPath: "/valid/path",
							SubPath:   "",
						},
					},
				},
			},
		},
		{
			name: "valid csi mount extension with subpath",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
				ExtensionKeyClaimWithCSIMount_SubPath:    "subdir/data",
			},
			wantErr: false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock: true,
				CSIMount: CSIMountExtension{
					MountConfigs: []CSIMountConfig{
						{
							PvName:    "test-volume",
							MountPath: "/valid/path",
							SubPath:   "subdir/data",
						},
					},
				},
			},
		},
		{
			name: "valid csi mount extension with empty subpath",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
				ExtensionKeyClaimWithCSIMount_SubPath:    "",
			},
			wantErr: false,
			expectExtension: NewSandboxRequestExtension{
				CreateOnNoStock: true,
				CSIMount: CSIMountExtension{
					MountConfigs: []CSIMountConfig{
						{
							PvName:    "test-volume",
							MountPath: "/valid/path",
							SubPath:   "",
						},
					},
				},
			},
		},
		{
			name: "invalid csi mount extension - missing volume name",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
			},
			wantErr: true,
		},
		{
			name: "invalid csi mount extension - missing mount point",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
			},
			wantErr: true,
		},
		{
			name: "invalid csi mount extension - invalid mount point",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/invalid/../path",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &NewSandboxRequest{
				Metadata: tt.metadata,
			}

			err := req.ParseExtensions()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.EqualValues(t, tt.expectExtension, req.Extensions)
				assert.Empty(t, req.Metadata)
			}
		})
	}
}

func TestParseExtensions_WithValidData(t *testing.T) {
	// Test case with valid image and CSI mount extensions
	req := &NewSandboxRequest{
		Metadata: map[string]string{
			ExtensionKeyClaimWithImage:               "nginx:latest",
			ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
			ExtensionKeyClaimWithCSIMount_MountPoint: "/data/mount",
		},
	}

	err := req.ParseExtensions()
	if err != nil {
		t.Fatalf("ParseExtensions() unexpected error = %v", err)
	}

	// to verify that image extension is parsed correctly
	if req.Extensions.InplaceUpdate.Image != "nginx:latest" {
		t.Errorf("Expected image 'nginx:latest', got '%s'", req.Extensions.InplaceUpdate.Image)
	}

	// to verify that CSI mount extension is parsed correctly
	if req.Extensions.CSIMount.MountConfigs[0].PvName != "test-volume" {
		t.Errorf("Expected volume name 'test-volume', got '%s'", req.Extensions.CSIMount.MountConfigs[0].PvName)
	}
	if req.Extensions.CSIMount.MountConfigs[0].MountPath != "/data/mount" {
		t.Errorf("Expected mount point '/data/mount', got '%s'", req.Extensions.CSIMount.MountConfigs[0].MountPath)
	}

	// to verify that metadata has been removed
	if _, exists := req.Metadata[ExtensionKeyClaimWithImage]; exists {
		t.Error("Expected image key to be deleted from metadata")
	}
	if _, exists := req.Metadata[ExtensionKeyClaimWithCSIMount_VolumeName]; exists {
		t.Error("Expected volume name key to be deleted from metadata")
	}
	if _, exists := req.Metadata[ExtensionKeyClaimWithCSIMount_MountPoint]; exists {
		t.Error("Expected mount point key to be deleted from metadata")
	}
}

func TestParseExtensionCSIMount(t *testing.T) {
	tests := []struct {
		name         string
		metadata     map[string]string
		expectError  bool
		expectVolume string
		expectMount  string
	}{
		{
			name: "valid csi mount extension",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
			},
			expectError:  false,
			expectVolume: "test-volume",
			expectMount:  "/valid/path",
		},
		{
			name: "missing volume name",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
			},
			expectError: true,
		},
		{
			name: "missing mount point",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
			},
			expectError: true,
		},
		{
			name: "both fields missing",
			metadata: map[string]string{
				"other-key": "other-value",
			},
			expectError:  false,
			expectVolume: "",
			expectMount:  "",
		},
		{
			name: "invalid mount point with ..",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/invalid/../path",
			},
			expectError: true,
		},
		{
			name: "invalid mount point not starting with /",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "invalid/path",
			},
			expectError: true,
		},
		{
			name:         "empty metadata",
			metadata:     map[string]string{},
			expectError:  false,
			expectVolume: "",
			expectMount:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &NewSandboxRequest{
				Metadata: tt.metadata,
			}

			err := req.parseExtensionCSIMount()

			if (err != nil) != tt.expectError {
				t.Errorf("parseExtensionCSIMount() error = %v, expectError %v", err, tt.expectError)
				return
			}

			if !tt.expectError {
				if tt.expectVolume != "" && req.Extensions.CSIMount.MountConfigs[0].PvName != tt.expectVolume {
					t.Errorf("Expected volume name '%s', got '%s'", tt.expectVolume, req.Extensions.CSIMount.MountConfigs[0].PvName)
				}
				if tt.expectMount != "" && req.Extensions.CSIMount.MountConfigs[0].MountPath != tt.expectMount {
					t.Errorf("Expected mount point '%s', got '%s'", tt.expectMount, req.Extensions.CSIMount.MountConfigs[0].MountPath)
				}
			}
		})
	}
}

func TestParseExtensionInplaceUpdate(t *testing.T) {
	tests := []struct {
		name        string
		metadata    map[string]string
		expectError bool
		expectImage string
	}{
		{
			name: "valid image extension",
			metadata: map[string]string{
				ExtensionKeyClaimWithImage: "nginx:latest",
			},
			expectError: false,
			expectImage: "nginx:latest",
		},
		{
			name: "valid image extension with timeout",
			metadata: map[string]string{
				ExtensionKeyClaimWithImage:   "nginx:latest",
				ExtensionKeyWaitReadyTimeout: "1234",
			},
			expectError: false,
			expectImage: "nginx:latest",
		},
		{
			name: "valid image with repository",
			metadata: map[string]string{
				ExtensionKeyClaimWithImage: "docker.io/library/ubuntu:20.04",
			},
			expectError: false,
			expectImage: "docker.io/library/ubuntu:20.04",
		},
		{
			name: "invalid image format",
			metadata: map[string]string{
				ExtensionKeyClaimWithImage: "invalid::image::format",
			},
			expectError: true,
		},
		{
			name: "malformed image name",
			metadata: map[string]string{
				ExtensionKeyClaimWithImage: "my_image@sha256:invalid_digest",
			},
			expectError: true,
		},
		{
			name: "no image extension present",
			metadata: map[string]string{
				"some-other-key": "some-value",
			},
			expectError: false,
			expectImage: "",
		},
		{
			name:        "empty metadata",
			metadata:    map[string]string{},
			expectError: false,
			expectImage: "",
		},
		{
			name:        "nil metadata",
			metadata:    nil,
			expectError: false,
			expectImage: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &NewSandboxRequest{
				Metadata: tt.metadata,
			}

			err := req.ParseExtensions()

			if (err != nil) != tt.expectError {
				t.Errorf("parseExtensionImage() error = %v, expectError %v", err, tt.expectError)
				return
			}

			if !tt.expectError {
				if tt.expectImage != "" && req.Extensions.InplaceUpdate.Image != tt.expectImage {
					t.Errorf("Expected image '%s', got '%s'", tt.expectImage, req.Extensions.InplaceUpdate.Image)
				}
				if tt.expectImage == "" && req.Extensions.InplaceUpdate.Image != "" {
					t.Errorf("Expected no image, got '%s'", req.Extensions.InplaceUpdate.Image)
				}
			}

			// Check if the image key is removed from metadata when present
			if _, exists := req.Metadata[ExtensionKeyClaimWithImage]; exists && tt.expectImage != "" {
				t.Errorf("Expected image key to be removed from metadata")
			}
			// Check if the image key is removed from metadata when present
			if _, exists := req.Metadata[ExtensionKeyWaitReadyTimeout]; exists && tt.expectImage != "" {
				t.Errorf("Expected key to be removed from metadata")
			}
		})
	}
}

func TestParseExtensionForMultiCSIMount(t *testing.T) {
	tests := []struct {
		name               string
		metadata           map[string]string
		expectError        bool
		expectedErrorSub   string
		expectedMountCount int
		expectedMounts     []CSIMountConfig
	}{
		{
			name:               "no multi csi mount config",
			metadata:           map[string]string{},
			expectError:        false,
			expectedMountCount: 0,
		},
		{
			name: "valid multi csi mount config with single mount",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/data","subPath":"data"}]`,
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedMounts: []CSIMountConfig{
				{
					PvName:    "vol1",
					MountPath: "/data",
					SubPath:   "data",
				},
			},
		},
		{
			name: "valid multi csi mount config with multiple mounts",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/data","subPath":"data"},{"pvName":"vol2","mountPath":"/logs","subPath":"logs"}]`,
			},
			expectError:        false,
			expectedMountCount: 2,
			expectedMounts: []CSIMountConfig{
				{
					PvName:    "vol1",
					MountPath: "/data",
					SubPath:   "data",
				},
				{
					PvName:    "vol2",
					MountPath: "/logs",
					SubPath:   "logs",
				},
			},
		},
		{
			name: "valid multi csi mount config with mountID",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-123","pvName":"vol1","mountPath":"/data"}]`,
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedMounts: []CSIMountConfig{
				{
					MountID:   "mount-123",
					PvName:    "vol1",
					MountPath: "/data",
				},
			},
		},
		{
			name: "valid multi csi mount with readOnly true",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/data","readOnly":true}]`,
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedMounts: []CSIMountConfig{
				{
					PvName:    "vol1",
					MountPath: "/data",
					ReadOnly:  true,
				},
			},
		},
		{
			name: "valid multi csi mount with readOnly false",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/data","readOnly":false}]`,
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedMounts: []CSIMountConfig{
				{
					PvName:    "vol1",
					MountPath: "/data",
					ReadOnly:  false,
				},
			},
		},
		{
			name: "valid multi csi mount with mixed readOnly settings",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/data","readOnly":true},{"pvName":"vol2","mountPath":"/logs","readOnly":false}]`,
			},
			expectError:        false,
			expectedMountCount: 2,
			expectedMounts: []CSIMountConfig{
				{
					PvName:    "vol1",
					MountPath: "/data",
					ReadOnly:  true,
				},
				{
					PvName:    "vol2",
					MountPath: "/logs",
					ReadOnly:  false,
				},
			},
		},
		{
			name: "valid multi csi mount with readOnly and subpath",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/data","subPath":"subdir","readOnly":true}]`,
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedMounts: []CSIMountConfig{
				{
					PvName:    "vol1",
					MountPath: "/data",
					SubPath:   "subdir",
					ReadOnly:  true,
				},
			},
		},
		{
			name: "valid multi csi mount with all fields including readOnly",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"mountID":"mount-456","pvName":"vol1","mountPath":"/var/data","subPath":"data/2024","readOnly":true}]`,
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedMounts: []CSIMountConfig{
				{
					MountID:   "mount-456",
					PvName:    "vol1",
					MountPath: "/var/data",
					SubPath:   "data/2024",
					ReadOnly:  true,
				},
			},
		},
		{
			name: "invalid json format",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `invalid-json`,
			},
			expectError:      true,
			expectedErrorSub: "invalid multiCsiMountConfig",
		},
		{
			name: "invalid mount point - not absolute path",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"relative/path"}]`,
			},
			expectError:      true,
			expectedErrorSub: "invalid containerMountPoint",
		},
		{
			name: "invalid mount point - path traversal",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/data/../etc/passwd"}]`,
			},
			expectError:      true,
			expectedErrorSub: "invalid containerMountPoint",
		},
		{
			name: "invalid mount point - empty path",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":""}]`,
			},
			expectError:      true,
			expectedErrorSub: "invalid containerMountPoint",
		},
		{
			name: "invalid mount point - does not start with slash",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"data"}]`,
			},
			expectError:      true,
			expectedErrorSub: "invalid containerMountPoint",
		},
		{
			name: "mixed valid and invalid mount points - first invalid",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"invalid"},{"pvName":"vol2","mountPath":"/valid"}]`,
			},
			expectError:      true,
			expectedErrorSub: "invalid containerMountPoint",
		},
		{
			name: "valid multi csi mount with empty subpath",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/data","subPath":""}]`,
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedMounts: []CSIMountConfig{
				{
					PvName:    "vol1",
					MountPath: "/data",
					SubPath:   "",
				},
			},
		},
		{
			name: "valid multi csi mount with complex nested paths",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountConfig: `[{"pvName":"vol1","mountPath":"/var/lib/data","subPath":"user/projects/2024/data"},{"pvName":"vol2","mountPath":"/var/log/app","subPath":"logs/production"}]`,
			},
			expectError:        false,
			expectedMountCount: 2,
			expectedMounts: []CSIMountConfig{
				{
					PvName:    "vol1",
					MountPath: "/var/lib/data",
					SubPath:   "user/projects/2024/data",
				},
				{
					PvName:    "vol2",
					MountPath: "/var/log/app",
					SubPath:   "logs/production",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &NewSandboxRequest{
				Metadata: tt.metadata,
			}

			err := req.parseExtensionForMultiCSIMount()

			if tt.expectError {
				require.Error(t, err)
				if tt.expectedErrorSub != "" {
					assert.Contains(t, err.Error(), tt.expectedErrorSub)
				}
			} else {
				require.NoError(t, err)

				// Verify metadata was cleaned up
				_, exists := req.Metadata[ExtensionKeyClaimWithCSIMount_MountConfig]
				assert.False(t, exists, "metadata should be deleted after parsing")

				// Verify CSIMount extension
				if tt.expectedMountCount == 0 {
					assert.Empty(t, req.Extensions.CSIMount.MountConfigs)
				} else {
					require.Len(t, req.Extensions.CSIMount.MountConfigs, tt.expectedMountCount)

					if tt.expectedMounts != nil {
						for i, expected := range tt.expectedMounts {
							actual := req.Extensions.CSIMount.MountConfigs[i]
							assert.Equal(t, expected.PvName, actual.PvName, "PvName mismatch at index %d", i)
							assert.Equal(t, expected.MountPath, actual.MountPath, "MountPath mismatch at index %d", i)
							assert.Equal(t, expected.SubPath, actual.SubPath, "SubPath mismatch at index %d", i)
							assert.Equal(t, expected.MountID, actual.MountID, "MountID mismatch at index %d", i)
							assert.Equal(t, expected.ReadOnly, actual.ReadOnly, "ReadOnly mismatch at index %d", i)
						}
					}
				}
			}
		})
	}
}

func TestParseExtensionsForSingleCSIMount(t *testing.T) {
	tests := []struct {
		name               string
		metadata           map[string]string
		expectError        bool
		expectedErrorSub   string
		expectedMountCount int
		expectedVolume     string
		expectedMount      string
		expectedSubpath    string
	}{
		{
			name:               "no csi mount config",
			metadata:           map[string]string{},
			expectError:        false,
			expectedMountCount: 0,
		},
		{
			name: "valid single csi mount - volume name and mount point",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedVolume:     "test-volume",
			expectedMount:      "/valid/path",
		},
		{
			name: "valid single csi mount with subpath",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
				ExtensionKeyClaimWithCSIMount_SubPath:    "subdir/data",
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedVolume:     "test-volume",
			expectedMount:      "/valid/path",
			expectedSubpath:    "subdir/data",
		},
		{
			name: "valid single csi mount with empty subpath",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
				ExtensionKeyClaimWithCSIMount_SubPath:    "",
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedVolume:     "test-volume",
			expectedMount:      "/valid/path",
		},
		{
			name: "missing volume name only",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_MountPoint: "/valid/path",
			},
			expectError:      true,
			expectedErrorSub: "must exist together or not at all",
		},
		{
			name: "missing mount point only",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
			},
			expectError:      true,
			expectedErrorSub: "must exist together or not at all",
		},
		{
			name: "invalid mount point - path traversal",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/invalid/../path",
			},
			expectError:      true,
			expectedErrorSub: "invalid containerMountPoint",
		},
		{
			name: "invalid mount point - not starting with slash",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "relative/path",
			},
			expectError:      true,
			expectedErrorSub: "invalid containerMountPoint",
		},
		{
			name: "invalid mount point - empty path",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "test-volume",
				ExtensionKeyClaimWithCSIMount_MountPoint: "",
			},
			expectError:      true,
			expectedErrorSub: "invalid containerMountPoint",
		},
		{
			name: "both fields present with complex nested paths",
			metadata: map[string]string{
				ExtensionKeyClaimWithCSIMount_VolumeName: "pv-complex-subpath",
				ExtensionKeyClaimWithCSIMount_MountPoint: "/container/mount/target",
				ExtensionKeyClaimWithCSIMount_SubPath:    "user/projects/2024/data",
			},
			expectError:        false,
			expectedMountCount: 1,
			expectedVolume:     "pv-complex-subpath",
			expectedMount:      "/container/mount/target",
			expectedSubpath:    "user/projects/2024/data",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &NewSandboxRequest{
				Metadata: tt.metadata,
			}

			err := req.parseExtensionsForSingleCSIMount()

			if tt.expectError {
				require.Error(t, err)
				if tt.expectedErrorSub != "" {
					assert.Contains(t, err.Error(), tt.expectedErrorSub)
				}
			} else {
				require.NoError(t, err)

				// Verify CSIMount extension
				if tt.expectedMountCount == 0 {
					assert.Empty(t, req.Extensions.CSIMount.MountConfigs)
				} else {
					require.Len(t, req.Extensions.CSIMount.MountConfigs, tt.expectedMountCount)

					firstMount := req.Extensions.CSIMount.MountConfigs[0]
					if tt.expectedVolume != "" {
						assert.Equal(t, tt.expectedVolume, firstMount.PvName, "VolumeName mismatch")
					}
					if tt.expectedMount != "" {
						assert.Equal(t, tt.expectedMount, firstMount.MountPath, "MountTarget mismatch")
					}
					if tt.expectedSubpath != "" {
						assert.Equal(t, tt.expectedSubpath, firstMount.SubPath, "Subpath mismatch")
					}
				}

				// Verify metadata cleanup
				_, exists := req.Metadata[ExtensionKeyClaimWithCSIMount_VolumeName]
				assert.False(t, exists, "VolumeName should be deleted from metadata after parsing")

				_, exists = req.Metadata[ExtensionKeyClaimWithCSIMount_MountPoint]
				assert.False(t, exists, "MountPoint should be deleted from metadata after parsing")

				_, exists = req.Metadata[ExtensionKeyClaimWithCSIMount_SubPath]
				assert.False(t, exists, "SubPath should be deleted from metadata after parsing")
			}
		})
	}
}

func TestNewSnapshotRequest_ParseExtensions(t *testing.T) {
	tests := []struct {
		name                     string
		headers                  map[string]string
		wantErr                  bool
		errContains              string
		expectKeepRunning        *bool
		expectTTL                *string
		expectPersistentContents []string
		expectWaitSuccessSeconds int
	}{
		// KeepRunning cases
		{
			name: "KeepRunning header set to true",
			headers: map[string]string{
				ExtensionHeaderSnapshotKeepRunning: "true",
			},
			expectKeepRunning: ptr.To(true),
		},
		{
			name: "KeepRunning header set to false",
			headers: map[string]string{
				ExtensionHeaderSnapshotKeepRunning: "false",
			},
			expectKeepRunning: ptr.To(false),
		},
		{
			name:              "KeepRunning header not set",
			headers:           map[string]string{},
			expectKeepRunning: nil,
		},
		{
			name: "KeepRunning header set to invalid value",
			headers: map[string]string{
				ExtensionHeaderSnapshotKeepRunning: "invalid",
			},
			expectKeepRunning: nil,
		},

		// TTL cases
		{
			name:      "TTL header not set",
			headers:   map[string]string{},
			expectTTL: nil,
		},
		{
			name: "TTL header with valid duration 30m",
			headers: map[string]string{
				ExtensionHeaderSnapshotTTL: "30m",
			},
			expectTTL: ptr.To("30m"),
		},
		{
			name: "TTL header with valid duration 1h",
			headers: map[string]string{
				ExtensionHeaderSnapshotTTL: "1h",
			},
			expectTTL: ptr.To("1h"),
		},
		{
			name: "TTL header with invalid format",
			headers: map[string]string{
				ExtensionHeaderSnapshotTTL: "invalid",
			},
			wantErr:     true,
			errContains: "invalid TTL format",
		},

		// PersistentContents cases
		{
			name:                     "PersistentContents header not set",
			headers:                  map[string]string{},
			expectPersistentContents: nil,
		},
		{
			name: "PersistentContents header with valid value memory",
			headers: map[string]string{
				ExtensionHeaderSnapshotPersistentContents: "memory",
			},
			expectPersistentContents: []string{"memory"},
		},
		{
			name: "PersistentContents header with valid value filesystem",
			headers: map[string]string{
				ExtensionHeaderSnapshotPersistentContents: "filesystem",
			},
			expectPersistentContents: []string{"filesystem"},
		},
		{
			name: "PersistentContents header with invalid value",
			headers: map[string]string{
				ExtensionHeaderSnapshotPersistentContents: "invalid",
			},
			wantErr:     true,
			errContains: "invalid persistent content",
		},

		// WaitSuccessSeconds cases
		{
			name:                     "WaitSuccessSeconds header not set",
			headers:                  map[string]string{},
			expectWaitSuccessSeconds: 0,
		},
		{
			name: "WaitSuccessSeconds header with valid positive integer",
			headers: map[string]string{
				ExtensionHeaderWaitSuccessSeconds: "30",
			},
			expectWaitSuccessSeconds: 30,
		},
		{
			name: "WaitSuccessSeconds header with zero",
			headers: map[string]string{
				ExtensionHeaderWaitSuccessSeconds: "0",
			},
			expectWaitSuccessSeconds: 0,
		},
		{
			name: "WaitSuccessSeconds header with invalid format",
			headers: map[string]string{
				ExtensionHeaderWaitSuccessSeconds: "abc",
			},
			wantErr:     true,
			errContains: "invalid WaitSuccessSeconds format",
		},
		{
			name: "WaitSuccessSeconds header with negative value",
			headers: map[string]string{
				ExtensionHeaderWaitSuccessSeconds: "-1",
			},
			wantErr:     true,
			errContains: "cannot be negative",
		},

		// Combined scenarios
		{
			name: "all headers set with valid values",
			headers: map[string]string{
				ExtensionHeaderSnapshotKeepRunning:        "true",
				ExtensionHeaderSnapshotTTL:                "2h",
				ExtensionHeaderSnapshotPersistentContents: "memory",
				ExtensionHeaderWaitSuccessSeconds:         "60",
			},
			expectKeepRunning:        ptr.To(true),
			expectTTL:                ptr.To("2h"),
			expectPersistentContents: []string{"memory"},
			expectWaitSuccessSeconds: 60,
		},
		{
			name:                     "no headers set - all defaults",
			headers:                  map[string]string{},
			expectKeepRunning:        nil,
			expectTTL:                nil,
			expectPersistentContents: nil,
			expectWaitSuccessSeconds: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := &NewSnapshotRequest{}
			headers := http.Header{}
			for key, value := range tt.headers {
				headers.Set(key, value)
			}

			err := req.ParseExtensions(headers)

			if tt.wantErr {
				require.Error(t, err)
				if tt.errContains != "" {
					assert.Contains(t, err.Error(), tt.errContains)
				}
				return
			}

			require.NoError(t, err)

			// Verify KeepRunning
			if tt.expectKeepRunning == nil {
				assert.Nil(t, req.Extensions.KeepRunning)
			} else {
				require.NotNil(t, req.Extensions.KeepRunning)
				assert.Equal(t, *tt.expectKeepRunning, *req.Extensions.KeepRunning)
			}

			// Verify TTL
			if tt.expectTTL == nil {
				assert.Nil(t, req.Extensions.TTL)
			} else {
				require.NotNil(t, req.Extensions.TTL)
				assert.Equal(t, *tt.expectTTL, *req.Extensions.TTL)
			}

			// Verify PersistentContents
			if tt.expectPersistentContents == nil {
				assert.Nil(t, req.Extensions.PersistentContents)
			} else {
				assert.ElementsMatch(t, tt.expectPersistentContents, req.Extensions.PersistentContents)
			}

			// Verify WaitSuccessSeconds
			assert.Equal(t, tt.expectWaitSuccessSeconds, req.Extensions.WaitSuccessSeconds)
		})
	}
}
