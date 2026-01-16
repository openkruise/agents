package models

import (
	"fmt"

	"github.com/distribution/reference"

	"github.com/openkruise/agents/api/v1alpha1"
)

// Extension keys are annotations used by sandbox-manager only.

const (
	ExtensionKeyClaimWithImage               = v1alpha1.E2BPrefix + "image"
	ExtensionKeyClaimWithCSIMount            = v1alpha1.E2BPrefix + "csi"
	ExtensionKeyClaimWithCSIMount_VolumeName = ExtensionKeyClaimWithCSIMount + "-volume-name"
	ExtensionKeyClaimWithCSIMount_MountPoint = ExtensionKeyClaimWithCSIMount + "-mount-point"
)

// Extensions for NewSandboxRequest

func (r *NewSandboxRequest) ParseExtensions() error {
	if r.Metadata == nil {
		return nil
	}
	// parse images
	if err := r.parseExtensionImage(); err != nil {
		return err
	}
	// parse csi mount config
	if err := r.parseExtensionCSIMount(); err != nil {
		return err
	}
	return nil
}

func (r *NewSandboxRequest) parseExtensionImage() error {
	// just valid image when image string is not empty
	image, ok := r.Metadata[ExtensionKeyClaimWithImage]
	if !ok {
		return nil
	}
	if _, err := reference.ParseNormalizedNamed(image); err != nil {
		return fmt.Errorf("invalid image [%s]: %v", image, err)
	}
	r.Extensions.Image = image
	delete(r.Metadata, ExtensionKeyClaimWithImage)
	return nil
}

func (r *NewSandboxRequest) parseExtensionCSIMount() error {
	// Both ExtensionKeyClaimWithCSIMount_VolumeName and ExtensionKeyClaimWithCSIMount_MountPoint must exist together or not at all.
	persistentVolumeName, volumeNameExists := r.Metadata[ExtensionKeyClaimWithCSIMount_VolumeName]
	containerMountPoint, mountPointExists := r.Metadata[ExtensionKeyClaimWithCSIMount_MountPoint]

	// If only one of the required fields exists, return an error
	if volumeNameExists != mountPointExists {
		return fmt.Errorf("both %s and %s must exist together or not at all",
			ExtensionKeyClaimWithCSIMount_VolumeName,
			ExtensionKeyClaimWithCSIMount_MountPoint)
	}

	// If neither field exists, nothing to process
	if !volumeNameExists && !mountPointExists {
		return nil
	}

	// validate containerMountPoint
	if err := validateMountPoint(containerMountPoint); err != nil {
		return fmt.Errorf("invalid containerMountPoint [%s]", containerMountPoint)
	}

	r.Extensions.CSIMount = CSIMountExtension{
		ContainerMountPoint:  containerMountPoint,
		PersistentVolumeName: persistentVolumeName,
	}
	delete(r.Metadata, ExtensionKeyClaimWithCSIMount_VolumeName)
	delete(r.Metadata, ExtensionKeyClaimWithCSIMount_MountPoint)
	return nil
}
