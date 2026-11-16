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

package csiutils

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/protobuf/proto"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/agent-runtime/storages"
	"github.com/openkruise/agents/pkg/resourceaccess"
	"github.com/openkruise/agents/pkg/utils/runtime/config"
)

// GenerateIsolatedMountOptions delegates to GenerateIsolatedMountOptionsWithScope
// without returning the storage authorization metadata.
func (h *CSIMountHandler) GenerateIsolatedMountOptions(ctx context.Context, namespace, owner string, requests []v1alpha1.CSIMountConfig, forExecution bool) ([]config.MountConfig, error) {
	mounts, _, err := h.GenerateIsolatedMountOptionsWithScope(ctx, namespace, owner, requests, forExecution)
	return mounts, err
}

// GenerateIsolatedMountOptionsWithScope validates shared platform PV mount templates,
// not customer ownership of PVs. namespace and owner must identify the admitted,
// API-key-authenticated delivery, never request attributes. The existing service
// and downstream IdP/storage MUST independently authorize that customer for the
// selected credential provider and backend target. Token issuance alone is not
// proof of local entitlement; this helper neither checks nor grants entitlement.
//
// The entire batch's scope is validated before execution providers run. With
// forExecution=false, only metadata preflight is performed, without a driver
// registry. Neither phase reads PVCs or Secrets; only agent-identity is supported.
// The returned scope is the preflight JSON for resourceaccess.StorageAuthAnnotation;
// an empty batch returns an empty scope. Errors return no mounts or scope.
func (h *CSIMountHandler) GenerateIsolatedMountOptionsWithScope(ctx context.Context, namespace, owner string, requests []v1alpha1.CSIMountConfig, forExecution bool) ([]config.MountConfig, string, error) {
	if strings.TrimSpace(namespace) == "" || strings.TrimSpace(owner) == "" {
		return nil, "", resourceaccess.ErrAccessDenied
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	entries := make([]isolatedMountPreflight, 0, len(requests))
	mounts := make([]config.MountConfig, 0, len(requests))
	for _, request := range requests {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		pv, err := h.loadIsolatedPV(ctx, request)
		if err != nil {
			return nil, "", err
		}
		attributes := pv.Spec.CSI.VolumeAttributes
		if err := mergeIsolatedRequestAttributes(request, pv.Spec.CSI.Driver, attributes); err != nil {
			return nil, "", err
		}
		if err := applyIsolatedSubPath(request, attributes); err != nil {
			return nil, "", err
		}
		if err := reconcileIsolatedProviderAliases(request, attributes); err != nil {
			return nil, "", err
		}
		if err := applyIsolatedKMSImplication(request, pv.Spec.CSI.Driver, attributes); err != nil {
			return nil, "", err
		}
		readOnly := request.ReadOnly || pv.Spec.CSI.ReadOnly || storages.IsPureReadOnly(pv.Spec.AccessModes)
		preflight, err := (&storages.MountProvider{}).GenerateCSINodePublishVolumeRequest(ctx, request.MountPath, pv, readOnly, nil)
		if err != nil {
			return nil, "", resourceaccess.ErrAccessDenied
		}
		provider, err := h.resolveIsolatedProvider(pv.Spec.CSI.Driver, forExecution)
		if err != nil {
			return nil, "", err
		}
		entries = append(entries, isolatedMountPreflight{mountPath: request.MountPath, pv: pv, preflight: preflight, provider: provider})
		mounts = append(mounts, config.MountConfig{Driver: pv.Spec.CSI.Driver, PublishRequest: preflight})
	}
	_, metadata, err := BuildIsolatedStorageAuthAnnotation(ctx, mounts)
	if err != nil {
		return nil, "", err
	}
	if !forExecution {
		return mounts, metadata, nil
	}
	// Every entry was fully preflighted above before any custom provider runs.
	// Refresh intentionally does not require these resources.
	result, err := executeIsolatedMounts(ctx, entries)
	if err != nil {
		return nil, "", err
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return result, metadata, nil
}

// isolatedMountPreflight carries one request's validated state into execution,
// so the execution loop never indexes parallel slices.
type isolatedMountPreflight struct {
	mountPath string
	pv        *corev1.PersistentVolume
	preflight *csi.NodePublishVolumeRequest
	provider  storages.VolumeMountProvider
}

func (h *CSIMountHandler) loadIsolatedPV(ctx context.Context, request v1alpha1.CSIMountConfig) (*corev1.PersistentVolume, error) {
	if request.PvName == "" || h.client == nil || validateMountAttributes(request.Attributes) != nil {
		return nil, resourceaccess.ErrAccessDenied
	}
	pv := &corev1.PersistentVolume{}
	if err := h.client.Get(ctx, ctrlclient.ObjectKey{Name: request.PvName}, pv); err != nil {
		return nil, isolatedReadError(err)
	}
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver == "" || pv.DeletionTimestamp != nil {
		return nil, resourceaccess.ErrAccessDenied
	}
	// Shared static credentials cannot establish per-customer entitlement.
	// NodePublishSecretRef is ignored for agent-identity, as in the legacy path.
	if pv.Spec.CSI.VolumeAttributes[volumeAttributeAuthType] != isolatedAgentIdentity {
		return nil, resourceaccess.ErrAccessDenied
	}
	return pv.DeepCopy(), nil
}

// mergeIsolatedRequestAttributes merges request attributes over the templated
// copy. Only driver-projected identity selectors are dynamic. Operational,
// routing and credential attributes must remain exactly as templated,
// including presence; an absent key cannot be introduced even as empty.
func mergeIsolatedRequestAttributes(request v1alpha1.CSIMountConfig, driver string, attributes map[string]string) error {
	for key, value := range request.Attributes {
		dynamic := false
		switch key {
		case isolatedProvider, isolatedProviderAlias:
			dynamic = driver == isolatedOSSDriver || driver == isolatedNASDriver
		case "bucket", "bucketSpace", "bucketSpacePrefix", volumeAttributeKMSKeyID, volumeAttributeRegion:
			dynamic = driver == isolatedOSSDriver
		case "server":
			dynamic = driver == isolatedNASDriver
		}
		if template, exists := attributes[key]; !dynamic && (!exists || template != value) {
			return resourceaccess.ErrAccessDenied
		}
		if key == volumeAttributeKMSKeyID && value == "" && (attributes[key] != "" || attributes[volumeAttributeEncrypted] == volumeAttributeKMSEnc) {
			return resourceaccess.ErrAccessDenied
		}
		attributes[key] = value
	}
	return nil
}

func applyIsolatedSubPath(request v1alpha1.CSIMountConfig, attributes map[string]string) error {
	if request.SubPath == "" {
		return nil
	}
	base, exists := attributes[volumeAttributePath]
	if !exists {
		base = "/"
	}
	// Validate opaque identity scope before the shared path helper can
	// clean dot components into a different template prefix.
	if !validIsolatedScopePath(base) || !validIsolatedScopePath("/"+strings.TrimLeft(request.SubPath, "/")) {
		return resourceaccess.ErrAccessDenied
	}
	merged, err := mergeAndValidatePaths(base, request.SubPath)
	if err != nil {
		return resourceaccess.ErrAccessDenied
	}
	attributes[volumeAttributePath] = merged
	return nil
}

// reconcileIsolatedProviderAliases keeps the canonical provider key and its
// alias coherent. The isolated projection does not invoke mutable legacy
// hooks; explicit provider selection wins over defaults.
func reconcileIsolatedProviderAliases(request v1alpha1.CSIMountConfig, attributes map[string]string) error {
	if alias, selected := request.Attributes[isolatedProviderAlias]; selected {
		if provider, selected := request.Attributes[isolatedProvider]; selected && provider != alias {
			return resourceaccess.ErrAccessDenied
		}
		attributes[isolatedProvider] = alias
	} else if provider, selected := request.Attributes[isolatedProvider]; selected {
		if _, exists := attributes[isolatedProviderAlias]; exists {
			attributes[isolatedProviderAlias] = provider
		}
	}
	if alias, exists := attributes[isolatedProviderAlias]; exists {
		if provider, present := attributes[isolatedProvider]; present && provider != alias {
			return resourceaccess.ErrAccessDenied
		}
		attributes[isolatedProvider] = alias
	}
	return nil
}

func applyIsolatedKMSImplication(request v1alpha1.CSIMountConfig, driver string, attributes map[string]string) error {
	if driver != isolatedOSSDriver || attributes[volumeAttributeKMSKeyID] == "" {
		return nil
	}
	if encrypted := attributes[volumeAttributeEncrypted]; request.Attributes[volumeAttributeKMSKeyID] == "" && encrypted != "" && encrypted != volumeAttributeKMSEnc {
		return resourceaccess.ErrAccessDenied
	}
	// A selected KMS key implies KMS encryption. Clearing a template's
	// existing KMS restriction was rejected before merging above.
	attributes[volumeAttributeEncrypted] = volumeAttributeKMSEnc
	return nil
}

// resolveIsolatedProvider validates execution dependencies during preflight,
// before any custom provider is invoked. Registry lookups are side-effect free.
func (h *CSIMountHandler) resolveIsolatedProvider(driver string, forExecution bool) (storages.VolumeMountProvider, error) {
	if !forExecution {
		return nil, nil
	}
	if h.storageRegistry == nil || !h.storageRegistry.IsSupported(driver) {
		return nil, resourceaccess.ErrAccessDenied
	}
	provider, exists := h.storageRegistry.GetProvider(driver)
	if !exists || provider == nil {
		return nil, resourceaccess.ErrAccessDenied
	}
	return provider, nil
}

func executeIsolatedMounts(ctx context.Context, entries []isolatedMountPreflight) ([]config.MountConfig, error) {
	result := make([]config.MountConfig, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		publish, err := entry.provider.GenerateCSINodePublishVolumeRequest(ctx, entry.mountPath, entry.pv, entry.preflight.Readonly, nil)
		if err != nil || publish == nil {
			// A provider error may include its credential-bearing input.
			return nil, resourceaccess.ErrAccessDenied
		}
		// Own the entire request before validation: a later provider call may
		// reuse or mutate any previously returned request, including nested data.
		publish = proto.Clone(publish).(*csi.NodePublishVolumeRequest)
		if len(publish.Secrets) != 0 || publish.TargetPath != entry.preflight.TargetPath || publish.Readonly != entry.preflight.Readonly ||
			!maps.Equal(publish.VolumeContext, entry.preflight.VolumeContext) {
			return nil, resourceaccess.ErrAccessDenied
		}
		// The outer execution preflight validates PublishContext/binary routing.
		result = append(result, config.MountConfig{Driver: entry.pv.Spec.CSI.Driver, PublishRequest: publish})
	}
	return result, nil
}

func isolatedReadError(err error) error {
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return resourceaccess.ErrAccessDenied
	}
	return fmt.Errorf("%w: %w", resourceaccess.ErrBackendRead, err)
}
