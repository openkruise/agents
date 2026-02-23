package models

import (
	"fmt"
	"strconv"

	"github.com/distribution/reference"

	"github.com/openkruise/agents/api/v1alpha1"
)

// Extension keys are annotations used by sandbox-manager only.
// Since they are all delivered through the E2B interface, they uniformly use the e2b.agents.kruise.io prefix
const (
	ExtensionKeyClaimWithImage               = v1alpha1.E2BPrefix + "image"
	ExtensionKeyInplaceUpdateTimeout         = v1alpha1.E2BPrefix + "inplace-update-timeout-seconds"
	ExtensionKeyClaimWithCSIMount            = v1alpha1.E2BPrefix + "csi"
	ExtensionKeyClaimWithCSIMount_VolumeName = ExtensionKeyClaimWithCSIMount + "-volume-name"
	ExtensionKeyClaimWithCSIMount_MountPoint = ExtensionKeyClaimWithCSIMount + "-mount-point"
	ExtensionKeySkipInitRuntime              = v1alpha1.E2BPrefix + "skip-init-runtime"
	ExtensionKeyReserveFailedSandbox         = v1alpha1.E2BPrefix + "reserve-failed-sandbox"
)

// Extensions for NewSandboxRequest

func (r *NewSandboxRequest) ParseExtensions() error {
	if r.Metadata == nil {
		return nil
	}
	// common extensions
	if err := r.parseCommonExtensions(); err != nil {
		return err
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

func (r *NewSandboxRequest) parseCommonExtensions() error {
	r.Extensions.SkipInitRuntime = r.Metadata[ExtensionKeySkipInitRuntime] == v1alpha1.True
	r.Extensions.ReserveFailedSandbox = r.Metadata[ExtensionKeyReserveFailedSandbox] == v1alpha1.True
	delete(r.Metadata, ExtensionKeySkipInitRuntime)
	delete(r.Metadata, ExtensionKeyReserveFailedSandbox)
	return nil
}

func (r *NewSandboxRequest) parseExtensionImage() error {
	// just valid image when image string is not empty
	if image, ok := r.Metadata[ExtensionKeyClaimWithImage]; ok {
		if _, err := reference.ParseNormalizedNamed(image); err != nil {
			return fmt.Errorf("invalid image [%s]: %v", image, err)
		}
		r.Extensions.InplaceUpdate.Image = image
		r.Extensions.InplaceUpdate.TimeoutSeconds = DefaultInplaceUpdateTimeoutSeconds
		delete(r.Metadata, ExtensionKeyClaimWithImage)
	}
	if timeoutStr, ok := r.Metadata[ExtensionKeyInplaceUpdateTimeout]; ok {
		timeoutSeconds, err := strconv.Atoi(timeoutStr)
		if err != nil {
			return fmt.Errorf("invalid timeout [%s]: %v", timeoutStr, err)
		}
		if timeoutSeconds > 0 {
			r.Extensions.InplaceUpdate.TimeoutSeconds = timeoutSeconds
		}
		delete(r.Metadata, ExtensionKeyInplaceUpdateTimeout)
	}
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
