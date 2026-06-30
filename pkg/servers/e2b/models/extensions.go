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

package models

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/distribution/reference"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	"github.com/openkruise/agents/api/v1alpha1"
)

// Sentinel string values accepted by the
// agents.kruise.io/reserve-failed-sandbox-for extension.
const (
	ReserveFailedSandboxValueNever   = "never"
	ReserveFailedSandboxValueForever = "forever"
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
	ExtensionKeyClaimWithCSIMount_MountConfig = v1alpha1.AnnotationCSIVolumeConfig
	ExtensionKeySkipInitRuntime               = v1alpha1.E2BPrefix + "skip-init-runtime"
	ExtensionKeyReserveFailedSandbox          = v1alpha1.E2BPrefix + "reserve-failed-sandbox"
	ExtensionKeyReserveFailedSandboxFor       = v1alpha1.E2BPrefix + "reserve-failed-sandbox-for"
	ExtensionKeyCreateOnNoStock               = v1alpha1.E2BPrefix + "create-on-no-stock"
	ExtensionKeyNeverTimeout                  = v1alpha1.E2BPrefix + "never-timeout"
	ExtensionKeyReturnPodIP                   = v1alpha1.E2BPrefix + "return-sandbox-ip"
	MetadataKeyPodIP                          = v1alpha1.E2BPrefix + "sandbox-ip"
	ExtensionKeySandboxName                   = v1alpha1.E2BPrefix + "sandbox-name"
	ExtensionKeySandboxGenerateName           = v1alpha1.E2BPrefix + "sandbox-generate-name"
)

const (
	ExtensionHeaderPrefix                     = "x-e2b-kruise-"
	ExtensionHeaderSnapshotKeepRunning        = ExtensionHeaderPrefix + "snapshot-keep-running"
	ExtensionHeaderSnapshotTTL                = ExtensionHeaderPrefix + "snapshot-ttl"
	ExtensionHeaderSnapshotPersistentContents = ExtensionHeaderPrefix + "snapshot-persistent-contents"
	ExtensionHeaderWaitSuccessSeconds         = ExtensionHeaderPrefix + "snapshot-wait-success-seconds"
)

const sandboxGenerateNameValidationSuffix = "abcde"

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
	reserveFor, reserveForSet, err := r.parseAndRemoveReserveFailedSandboxFor()
	if err != nil {
		return err
	}
	if reserveForSet {
		r.Extensions.ReserveFailedSandboxFor = reserveFor
	} else if r.Metadata[ExtensionKeyReserveFailedSandbox] == v1alpha1.True {
		r.Extensions.ReserveFailedSandboxFor = ptr.To(consts.ReserveFailedSandboxForever)
	}
	r.Extensions.CreateOnNoStock = r.Metadata[ExtensionKeyCreateOnNoStock] != v1alpha1.False
	r.Extensions.NeverTimeout = r.Metadata[ExtensionKeyNeverTimeout] == v1alpha1.True
	r.Extensions.ReturnPodIP = r.Metadata[ExtensionKeyReturnPodIP] == v1alpha1.True
	delete(r.Metadata, ExtensionKeySkipInitRuntime)
	delete(r.Metadata, ExtensionKeyReserveFailedSandbox)
	delete(r.Metadata, ExtensionKeyCreateOnNoStock)
	delete(r.Metadata, ExtensionKeyNeverTimeout)
	delete(r.Metadata, ExtensionKeyReturnPodIP)
	if r.Extensions.TimeoutSeconds, err = r.parseAndRemoveIntExtension(ExtensionKeyClaimTimeout); err != nil {
		return err
	}
	if r.Extensions.WaitReadySeconds, err = r.parseAndRemoveIntExtension(ExtensionKeyWaitReadyTimeout); err != nil {
		return err
	}

	if err = r.parseExtensionLabels(); err != nil {
		return err
	}
	if err = r.parseExtensionSandboxNaming(); err != nil {
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

// parseExtensionSandboxNaming reads ExtensionKeySandboxName /
// ExtensionKeySandboxGenerateName from r.Metadata, validates them, and
// strips them so they don't propagate to sandbox annotations.
//
// We validate as DNS-1123 label (63 chars, no dots) rather than subdomain:
// the sandbox name becomes the underlying Pod name and is used as a DNS
// hostname downstream, where label semantics apply. GenerateName is also
// checked against a representative generated name so malformed trailing-dash
// prefixes fail before the request reaches Kubernetes.
func (r *NewSandboxRequest) parseExtensionSandboxNaming() error {
	name, hasName := r.Metadata[ExtensionKeySandboxName]
	gen, hasGen := r.Metadata[ExtensionKeySandboxGenerateName]
	if hasName && hasGen {
		return fmt.Errorf("sandbox-name and sandbox-generate-name are mutually exclusive")
	}
	if hasName {
		if name == "" {
			return fmt.Errorf("sandbox-name must not be empty")
		}
		if errs := apivalidation.NameIsDNSLabel(name, false); len(errs) > 0 {
			return fmt.Errorf("invalid sandbox-name [%s]: %s", name, strings.Join(errs, ", "))
		}
		r.Extensions.Name = name
		delete(r.Metadata, ExtensionKeySandboxName)
	}
	if hasGen {
		if gen == "" {
			return fmt.Errorf("sandbox-generate-name must not be empty")
		}
		if errs := validateSandboxGenerateName(gen); len(errs) > 0 {
			return fmt.Errorf("invalid sandbox-generate-name [%s]: %s", gen, strings.Join(errs, ", "))
		}
		r.Extensions.GenerateName = gen
		delete(r.Metadata, ExtensionKeySandboxGenerateName)
	}
	return nil
}

func validateSandboxGenerateName(generateName string) []string {
	if errs := apivalidation.NameIsDNSLabel(generateName, true); len(errs) > 0 {
		return errs
	}

	// Kubernetes validates generateName with a trailing-dash mask, which can
	// hide an invalid byte immediately before the dash. Validate a representative
	// generated name so malformed prefixes are rejected before creation.
	// The truncation mirrors Kubernetes SimpleNameGenerator: it cuts the prefix
	// to leave room for the generated 5-byte suffix.
	maxPrefixLength := validation.DNS1123LabelMaxLength - len(sandboxGenerateNameValidationSuffix)
	if len(generateName) > maxPrefixLength {
		generateName = generateName[:maxPrefixLength]
	}
	return apivalidation.NameIsDNSLabel(generateName+sandboxGenerateNameValidationSuffix, false)
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

func (r *NewSandboxRequest) parseAndRemoveReserveFailedSandboxFor() (*time.Duration, bool, error) {
	raw, ok := r.Metadata[ExtensionKeyReserveFailedSandboxFor]
	if !ok {
		return nil, false, nil
	}
	defer delete(r.Metadata, ExtensionKeyReserveFailedSandboxFor)

	switch raw {
	case ReserveFailedSandboxValueNever:
		return ptr.To(consts.ReserveFailedSandboxNever), true, nil
	case ReserveFailedSandboxValueForever:
		return ptr.To(consts.ReserveFailedSandboxForever), true, nil
	}

	duration, err := time.ParseDuration(raw)
	if err != nil {
		return nil, true, fmt.Errorf("invalid reserve failed sandbox duration %q: %w", raw, err)
	}
	if duration < 0 {
		return nil, true, fmt.Errorf("reserve failed sandbox duration %q cannot be negative, use %q", raw, ReserveFailedSandboxValueForever)
	}
	return &duration, true, nil
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
