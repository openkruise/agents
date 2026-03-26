package models

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/distribution/reference"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/api/v1alpha1"
)

//goland:noinspection GoSnakeCaseUsage
const (
	ExtensionKeyClaimTimeout                  = v1alpha1.E2BPrefix + "claim-timeout-seconds"
	ExtensionKeyWaitReadyTimeout              = v1alpha1.E2BPrefix + "wait-ready-timeout-seconds"
	ExtensionKeyClaimWithImage                = v1alpha1.E2BPrefix + "image"
	ExtensionKeyClaimWithCSIMount             = v1alpha1.E2BPrefix + "csi"
	ExtensionKeyClaimWithCSIMount_VolumeName  = ExtensionKeyClaimWithCSIMount + "-volume-name"
	ExtensionKeyClaimWithCSIMount_SubPath     = ExtensionKeyClaimWithCSIMount + "-subpath"
	ExtensionKeyClaimWithCSIMount_MountPoint  = ExtensionKeyClaimWithCSIMount + "-mount-point"
	ExtensionKeyClaimWithCSIMount_MountConfig = ExtensionKeyClaimWithCSIMount + "-volume-config"
	ExtensionKeySkipInitRuntime               = v1alpha1.E2BPrefix + "skip-init-runtime"
	ExtensionKeyReserveFailedSandbox          = v1alpha1.E2BPrefix + "reserve-failed-sandbox"
	ExtensionKeyCreateOnNoStock               = v1alpha1.E2BPrefix + "create-on-no-stock"
	ExtensionKeyNeverTimeout                  = v1alpha1.E2BPrefix + "never-timeout"
)

const (
	ExtensionHeaderPrefix                     = "x-e2b-kruise-"
	ExtensionHeaderSnapshotKeepRunning        = ExtensionHeaderPrefix + "snapshot-keep-running"
	ExtensionHeaderSnapshotTTL                = ExtensionHeaderPrefix + "snapshot-ttl"
	ExtensionHeaderSnapshotPersistentContents = ExtensionHeaderPrefix + "snapshot-persistent-contents"
	ExtensionHeaderWaitSuccessSeconds         = ExtensionHeaderPrefix + "snapshot-wait-success-seconds"
)

// Extensions for NewSandboxRequest

func (r *NewSandboxRequest) ParseExtensions() error {
	if r.Metadata == nil {
		r.Metadata = make(map[string]string)
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
	r.Extensions.CreateOnNoStock = r.Metadata[ExtensionKeyCreateOnNoStock] != v1alpha1.False
	r.Extensions.NeverTimeout = r.Metadata[ExtensionKeyNeverTimeout] == v1alpha1.True
	delete(r.Metadata, ExtensionKeySkipInitRuntime)
	delete(r.Metadata, ExtensionKeyReserveFailedSandbox)
	delete(r.Metadata, ExtensionKeyCreateOnNoStock)
	delete(r.Metadata, ExtensionKeyNeverTimeout)
	var err error
	if r.Extensions.TimeoutSeconds, err = r.parseAndRemoveIntExtension(ExtensionKeyClaimTimeout); err != nil {
		return err
	}
	if r.Extensions.WaitReadySeconds, err = r.parseAndRemoveIntExtension(ExtensionKeyWaitReadyTimeout); err != nil {
		return err
	}
	return nil
}

func (r *NewSandboxRequest) parseExtensionImage() error {
	// just valid image when image string is not empty
	if image, ok := r.Metadata[ExtensionKeyClaimWithImage]; ok {
		if _, err := reference.ParseNormalizedNamed(image); err != nil {
			return fmt.Errorf("invalid image [%s]: %v", image, err)
		}
		r.Extensions.InplaceUpdate.Image = image
		delete(r.Metadata, ExtensionKeyClaimWithImage)
	}
	return nil
}

func (r *NewSandboxRequest) parseExtensionCSIMount() error {
	// parse multi csi mount config
	if err := r.parseExtensionForMultiCSIMount(); err != nil {
		return err
	}
	// for single csi mount config
	if err := r.parseExtensionsForSingleCSIMount(); err != nil {
		return err
	}
	return nil
}

func (r *NewSandboxRequest) parseExtensionForMultiCSIMount() error {
	multiCsiMountConfigRaw, configExist := r.Metadata[ExtensionKeyClaimWithCSIMount_MountConfig]
	if !configExist {
		return nil
	}

	var multiCsiMountConfig []CSIMountConfig
	if err := json.Unmarshal([]byte(multiCsiMountConfigRaw), &multiCsiMountConfig); err != nil {
		return fmt.Errorf("invalid multiCsiMountConfig [%s]: %s", ExtensionKeyClaimWithCSIMount_MountConfig, multiCsiMountConfigRaw)
	}
	for _, mountConfig := range multiCsiMountConfig {
		// validate containerMountPoint
		if err := validateMountPoint(mountConfig.MountPath); err != nil {
			return fmt.Errorf("invalid containerMountPoint [%s]", mountConfig.MountPath)
		}
	}
	// parse multi csi mount config to r.extensions
	r.Extensions.CSIMount = CSIMountExtension{
		MountConfigs: multiCsiMountConfig,
	}
	delete(r.Metadata, ExtensionKeyClaimWithCSIMount_MountConfig)
	return nil
}

func (r *NewSandboxRequest) parseExtensionsForSingleCSIMount() error {
	// for single csi mount config
	// Both ExtensionKeyClaimWithCSIMount_VolumeName and ExtensionKeyClaimWithCSIMount_MountPoint must exist together or not at all.
	persistentVolumeName, volumeNameExists := r.Metadata[ExtensionKeyClaimWithCSIMount_VolumeName]
	containerMountPoint, mountPointExists := r.Metadata[ExtensionKeyClaimWithCSIMount_MountPoint]
	subpath, _ := r.Metadata[ExtensionKeyClaimWithCSIMount_SubPath]

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
		MountConfigs: make([]CSIMountConfig, 0, 1),
	}
	r.Extensions.CSIMount.MountConfigs = append(r.Extensions.CSIMount.MountConfigs, CSIMountConfig{
		PvName:    persistentVolumeName,
		MountPath: containerMountPoint,
		SubPath:   subpath,
	})
	delete(r.Metadata, ExtensionKeyClaimWithCSIMount_VolumeName)
	delete(r.Metadata, ExtensionKeyClaimWithCSIMount_MountPoint)
	delete(r.Metadata, ExtensionKeyClaimWithCSIMount_SubPath)
	return nil
}

func (r *NewSandboxRequest) parseAndRemoveIntExtension(key string) (int, error) {
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

func (s *NewSnapshotRequest) ParseExtensions(headers http.Header) error {
	// KeepRunning
	switch headers.Get(ExtensionHeaderSnapshotKeepRunning) {
	case v1alpha1.True:
		s.Extensions.KeepRunning = ptr.To(true)
	case v1alpha1.False:
		s.Extensions.KeepRunning = ptr.To(false)
	}
	// TTL
	if ttl := headers.Get(ExtensionHeaderSnapshotTTL); ttl != "" {
		if _, err := time.ParseDuration(ttl); err != nil {
			return fmt.Errorf("invalid TTL format %q: %w", ttl, err)
		}
		s.Extensions.TTL = ptr.To(ttl)
	}
	// PersistentContents
	if persistentContents := headers.Get(ExtensionHeaderSnapshotPersistentContents); persistentContents != "" {
		contents, err := parseAndValidatePersistentContents(persistentContents)
		if err != nil {
			return err
		}
		s.Extensions.PersistentContents = contents
	}
	// WaitSuccessSeconds
	if waitSuccessSeconds := headers.Get(ExtensionHeaderWaitSuccessSeconds); waitSuccessSeconds != "" {
		seconds, err := strconv.Atoi(waitSuccessSeconds)
		if err != nil {
			return fmt.Errorf("invalid WaitSuccessSeconds format %q: %w", waitSuccessSeconds, err)
		}
		if seconds < 0 {
			return fmt.Errorf("WaitSuccessSeconds %s cannot be negative", waitSuccessSeconds)
		}
		s.Extensions.WaitSuccessSeconds = seconds
	}
	return nil
}
