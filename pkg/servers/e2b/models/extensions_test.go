package models

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		},
		{
			name:     "empty metadata",
			metadata: map[string]string{},
			wantErr:  false,
		},
		{
			name: "valid image extension",
			metadata: map[string]string{
				ExtensionKeyClaimWithImage: "nginx:latest",
			},
			wantErr: false,
			expectExtension: NewSandboxRequestExtension{
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
				CSIMount: CSIMountExtension{
					PersistentVolumeName: "test-volume",
					ContainerMountPoint:  "/valid/path",
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
	if req.Extensions.CSIMount.PersistentVolumeName != "test-volume" {
		t.Errorf("Expected volume name 'test-volume', got '%s'", req.Extensions.CSIMount.PersistentVolumeName)
	}
	if req.Extensions.CSIMount.ContainerMountPoint != "/data/mount" {
		t.Errorf("Expected mount point '/data/mount', got '%s'", req.Extensions.CSIMount.ContainerMountPoint)
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
				if tt.expectVolume != "" && req.Extensions.CSIMount.PersistentVolumeName != tt.expectVolume {
					t.Errorf("Expected volume name '%s', got '%s'", tt.expectVolume, req.Extensions.CSIMount.PersistentVolumeName)
				}
				if tt.expectMount != "" && req.Extensions.CSIMount.ContainerMountPoint != tt.expectMount {
					t.Errorf("Expected mount point '%s', got '%s'", tt.expectMount, req.Extensions.CSIMount.ContainerMountPoint)
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
