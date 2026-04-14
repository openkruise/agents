package models

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/api/v1alpha1"
)

//goland:noinspection GoSnakeCaseUsage
const (
	ExtensionKeyClaimTimeout                  = v1alpha1.E2BPrefix + "claim-timeout-seconds"
	ExtensionKeyWaitReadyTimeout              = v1alpha1.E2BPrefix + "wait-ready-timeout-seconds"
	ExtensionKeyClaimWithCPURequest           = v1alpha1.E2BPrefix + "cpu-request"
	ExtensionKeyClaimWithCPULimit             = v1alpha1.E2BPrefix + "cpu-limit"
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

	if err = r.parseExtensionLabels(); err != nil {
		return err
	}
	return nil
}

func (r *NewSandboxRequest) parseExtensionLabels() error {
	for k, v := range r.Metadata {
		key := strings.TrimPrefix(k, v1alpha1.E2BLabelPrefix)
		if key == k {
			// not a label
			continue
		}
		if r.Extensions.Labels == nil {
			r.Extensions.Labels = make(map[string]string)
		}
		if len(validation.IsQualifiedName(key)) != 0 {
			return fmt.Errorf("invalid label name [%s]", key)
		}

		if len(validation.IsValidLabelValue(v)) != 0 {
			return fmt.Errorf("invalid label value [%s]", v)
		}

		r.Extensions.Labels[key] = v
		delete(r.Metadata, k)
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
	if err := r.parseExtensionResources(); err != nil {
		return err
	}
	return nil
}

func (r *NewSandboxRequest) parseExtensionResources() error {
	cpuReq, hasCPUReq, err := r.parseAndRemoveQuantity(ExtensionKeyClaimWithCPURequest)
	if err != nil {
		return err
	}
	cpuLim, hasCPULim, err := r.parseAndRemoveQuantity(ExtensionKeyClaimWithCPULimit)
	if err != nil {
		return err
	}
	if !hasCPUReq && !hasCPULim {
		return nil
	}
	if r.Extensions.InplaceUpdate.Resources == nil {
		r.Extensions.InplaceUpdate.Resources = &InplaceUpdateResourcesExtension{}
	}
	if hasCPUReq {
		if r.Extensions.InplaceUpdate.Resources.Requests == nil {
			r.Extensions.InplaceUpdate.Resources.Requests = corev1.ResourceList{}
		}
		r.Extensions.InplaceUpdate.Resources.Requests[corev1.ResourceCPU] = cpuReq
	}
	if hasCPULim {
		if r.Extensions.InplaceUpdate.Resources.Limits == nil {
			r.Extensions.InplaceUpdate.Resources.Limits = corev1.ResourceList{}
		}
		r.Extensions.InplaceUpdate.Resources.Limits[corev1.ResourceCPU] = cpuLim
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

	var multiCsiMountConfig []v1alpha1.CSIMountConfig
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
		MountConfigs: make([]v1alpha1.CSIMountConfig, 0, 1),
	}
	r.Extensions.CSIMount.MountConfigs = append(r.Extensions.CSIMount.MountConfigs, v1alpha1.CSIMountConfig{
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

func (r *NewSandboxRequest) parseAndRemoveQuantity(key string) (resource.Quantity, bool, error) {
	raw, ok := r.Metadata[key]
	if !ok {
		return resource.Quantity{}, false, nil
	}
	defer delete(r.Metadata, key)
	qty, err := resource.ParseQuantity(raw)
	if err != nil {
		return resource.Quantity{}, false, fmt.Errorf("invalid quantity for %s [%s]: %v", key, raw, err)
	}
	if qty.IsZero() || qty.Cmp(resource.Quantity{}) < 0 {
		return resource.Quantity{}, false, fmt.Errorf("%s must be a positive value, got [%s]", key, raw)
	}
	return qty, true, nil
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
