package models

import (
	"encoding/json"
	"fmt"

	"github.com/distribution/reference"
	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/utils"
)

// Extension keys are annotations used by sandbox-manager only.

const (
	ExtensionKeyClaimWithImage    = v1alpha1.E2BPrefix + "image"
	ExtensionKeyClaimWithCSIMount = v1alpha1.E2BPrefix + "csi-mount"
)

// Extensions for NewSandboxRequest

func (r *NewSandboxRequest) ParseExtensions() error {
	for k, v := range r.Metadata {
		isExtension := true
		switch k {
		case ExtensionKeyClaimWithImage:
			if err := r.parseExtensionImage(v); err != nil {
				return err
			}
		case ExtensionKeyClaimWithCSIMount:
			if err := r.parseExtensionCSIMount(v); err != nil {
				return err
			}
		default:
			isExtension = false
		}
		if isExtension {
			delete(r.Metadata, k)
		}
	}
	return nil
}

func (r *NewSandboxRequest) parseExtensionImage(image string) error {
	if _, err := reference.ParseNormalizedNamed(image); err != nil {
		return fmt.Errorf("invalid image [%s]: %v", image, err)
	}
	r.Extensions.Image = image
	return nil
}

func (r *NewSandboxRequest) parseExtensionCSIMount(raw string) error {
	if err := json.Unmarshal([]byte(raw), &r.Extensions.CSIMount); err != nil {
		return fmt.Errorf("cannot unmarshal storage-mount extension into go map: %s", err.Error())
	}
	if err := utils.DecodeBase64Proto(r.Extensions.CSIMount.Request, &r.Extensions.CSIMount.RealRequest); err != nil {
		return fmt.Errorf("cannot decode csi.NodePublishVolumeRequest from base64: %s", err.Error())
	}
	return nil
}
