package models

import (
	"fmt"
	"strconv"

	"github.com/distribution/reference"

	"github.com/openkruise/agents/api/v1alpha1"
)

// Extension keys are annotations used by sandbox-manager only.
// Since they are all delivered through the E2B interface, they uniformly use the e2b.agents.kruise.io prefix
//
//goland:noinspection GoSnakeCaseUsage
const (
	ExtensionKeyClaimTimeout                 = v1alpha1.E2BPrefix + "claim-timeout-seconds"
	ExtensionKeyInplaceUpdateTimeout         = v1alpha1.E2BPrefix + "inplace-update-timeout-seconds"
	ExtensionKeyClaimWithImage               = v1alpha1.E2BPrefix + "image"
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
	var err error
	if r.Extensions.TimeoutSeconds, err = r.parseIntExtension(ExtensionKeyClaimTimeout); err != nil {
		return err
	}
	return nil
}

func (r *NewSandboxRequest) parseExtensionImage() error {
	var err error
	// just valid image when image string is not empty
	if image, ok := r.Metadata[ExtensionKeyClaimWithImage]; ok {
		if _, err := reference.ParseNormalizedNamed(image); err != nil {
			return fmt.Errorf("invalid image [%s]: %v", image, err)
		}
		r.Extensions.InplaceUpdate.Image = image
		delete(r.Metadata, ExtensionKeyClaimWithImage)
	}
	if r.Extensions.InplaceUpdate.TimeoutSeconds, err = r.parseIntExtension(ExtensionKeyInplaceUpdateTimeout); err != nil {
		return err
	}
	if r.Extensions.InplaceUpdate.TimeoutSeconds <= 0 {
		r.Extensions.InplaceUpdate.TimeoutSeconds = DefaultInplaceUpdateTimeoutSeconds
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

func (r *NewSandboxRequest) parseIntExtension(key string) (int, error) {
	if numStr, ok := r.Metadata[key]; ok {
		defer delete(r.Metadata, key)
		num, err := strconv.Atoi(numStr)
		if err != nil {
			return 0, fmt.Errorf("invalid number [%s]: %v", numStr, err)
		}
		if num > 0 {
			return num, nil
		}
	}
	return 0, nil
}
