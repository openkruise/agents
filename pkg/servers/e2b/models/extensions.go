package models

import (
	"encoding/json"
	"fmt"

	"github.com/distribution/reference"
	"github.com/openkruise/agents/pkg/sandbox-manager/storage"
)

// Extension keys are annotations used by sandbox-manager only.

const (
	ExtensionKeyClaimWithImage        = InternalPrefix + "image"
	ExtensionKeyClaimWithStorageMount = InternalPrefix + "storage-mount"
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
		case ExtensionKeyClaimWithStorageMount:
			if err := r.parseExtensionStorageMount(v); err != nil {
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

func (r *NewSandboxRequest) parseExtensionStorageMount(raw string) error {
	params := make(map[string]string)
	if err := json.Unmarshal([]byte(raw), &params); err != nil {
		return fmt.Errorf("cannot unmarshal storage-mount extension into go map: %s", err.Error())
	}
	opts, err := storage.NewMountOptions(params)
	if err != nil {
		return err
	}
	r.Extensions.StorageMount = opts
	return nil
}
