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
	volumes := make([]*corev1.PersistentVolume, len(requests))
	mounts := make([]config.MountConfig, 0, len(requests))
	for i, request := range requests {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		if request.PvName == "" || h.client == nil || validateMountAttributes(request.Attributes) != nil {
			return nil, "", resourceaccess.ErrAccessDenied
		}
		pv := &corev1.PersistentVolume{}
		if err := h.client.Get(ctx, ctrlclient.ObjectKey{Name: request.PvName}, pv); err != nil {
			return nil, "", isolatedReadError(err)
		}
		if pv.Spec.CSI == nil || pv.Spec.CSI.Driver == "" || pv.DeletionTimestamp != nil {
			return nil, "", resourceaccess.ErrAccessDenied
		}
		// Shared static credentials cannot establish per-customer entitlement.
		// NodePublishSecretRef is ignored for agent-identity, as in the legacy path.
		if pv.Spec.CSI.VolumeAttributes[volumeAttributeAuthType] != isolatedAgentIdentity {
			return nil, "", resourceaccess.ErrAccessDenied
		}
		pv = pv.DeepCopy()
		attributes := pv.Spec.CSI.VolumeAttributes
		// Only driver-projected identity selectors are dynamic. Operational,
		// routing and credential attributes must remain exactly as templated,
		// including presence; an absent key cannot be introduced even as empty.
		for key, value := range request.Attributes {
			dynamic := false
			switch key {
			case isolatedProvider, isolatedProviderAlias:
				dynamic = pv.Spec.CSI.Driver == isolatedOSSDriver || pv.Spec.CSI.Driver == isolatedNASDriver
			case "bucket", "bucketSpace", "bucketSpacePrefix", "kmsKeyId", "region":
				dynamic = pv.Spec.CSI.Driver == isolatedOSSDriver
			case "server":
				dynamic = pv.Spec.CSI.Driver == isolatedNASDriver
			}
			if template, exists := attributes[key]; !dynamic && (!exists || template != value) {
				return nil, "", resourceaccess.ErrAccessDenied
			}
			if key == "kmsKeyId" && value == "" && (attributes[key] != "" || attributes["encrypted"] == "kms") {
				return nil, "", resourceaccess.ErrAccessDenied
			}
			attributes[key] = value
		}
		if request.SubPath != "" {
			base, exists := attributes[volumeAttributePath]
			if !exists {
				base = "/"
			}
			// Validate opaque identity scope before the shared path helper can
			// clean dot components into a different template prefix.
			if !validIsolatedScopePath(base) || !validIsolatedScopePath("/"+strings.TrimLeft(request.SubPath, "/")) {
				return nil, "", resourceaccess.ErrAccessDenied
			}
			merged, err := mergeAndValidatePaths(base, request.SubPath)
			if err != nil {
				return nil, "", resourceaccess.ErrAccessDenied
			}
			attributes[volumeAttributePath] = merged
		}
		// The isolated projection does not invoke mutable legacy hooks. Every
		// enrichment below comes solely from the merged template copy. Explicit
		// provider selection wins over defaults, keeping any paired alias coherent.
		if alias, selected := request.Attributes[isolatedProviderAlias]; selected {
			if provider, selected := request.Attributes[isolatedProvider]; selected && provider != alias {
				return nil, "", resourceaccess.ErrAccessDenied
			}
			attributes[isolatedProvider] = alias
		} else if provider, selected := request.Attributes[isolatedProvider]; selected {
			if _, exists := attributes[isolatedProviderAlias]; exists {
				attributes[isolatedProviderAlias] = provider
			}
		}
		if alias, exists := attributes[isolatedProviderAlias]; exists {
			if provider, present := attributes[isolatedProvider]; present && provider != alias {
				return nil, "", resourceaccess.ErrAccessDenied
			}
			attributes[isolatedProvider] = alias
		}
		if pv.Spec.CSI.Driver == isolatedOSSDriver && attributes["kmsKeyId"] != "" {
			if encrypted := attributes["encrypted"]; request.Attributes["kmsKeyId"] == "" && encrypted != "" && encrypted != "kms" {
				return nil, "", resourceaccess.ErrAccessDenied
			}
			// A selected KMS key implies KMS encryption. Clearing a template's
			// existing KMS restriction was rejected before merging above.
			attributes["encrypted"] = "kms"
		}
		readOnly := request.ReadOnly || pv.Spec.CSI.ReadOnly || storages.IsPureReadOnly(pv.Spec.AccessModes)
		preflight, err := (&storages.MountProvider{}).GenerateCSINodePublishVolumeRequest(ctx, request.MountPath, pv, readOnly, nil)
		if err != nil {
			return nil, "", resourceaccess.ErrAccessDenied
		}
		volumes[i] = pv
		mounts = append(mounts, config.MountConfig{Driver: pv.Spec.CSI.Driver, PublishRequest: preflight})
	}
	_, metadata, err := BuildIsolatedStorageAuthAnnotation(ctx, mounts)
	if err != nil {
		return nil, "", err
	}
	if !forExecution {
		return mounts, metadata, nil
	}

	// Validate all execution dependencies before invoking any custom provider.
	// Refresh intentionally does not require these resources.
	providers := make([]storages.VolumeMountProvider, len(mounts))
	for i, mount := range mounts {
		if h.storageRegistry == nil || !h.storageRegistry.IsSupported(mount.Driver) {
			return nil, "", resourceaccess.ErrAccessDenied
		}
		provider, exists := h.storageRegistry.GetProvider(mount.Driver)
		if !exists || provider == nil {
			return nil, "", resourceaccess.ErrAccessDenied
		}
		providers[i] = provider
	}
	result := make([]config.MountConfig, 0, len(mounts))
	for i, mount := range mounts {
		if err := ctx.Err(); err != nil {
			return nil, "", err
		}
		publish, err := providers[i].GenerateCSINodePublishVolumeRequest(ctx, requests[i].MountPath, volumes[i], mount.PublishRequest.Readonly, nil)
		if err != nil || publish == nil {
			// A provider error may include its credential-bearing input.
			return nil, "", resourceaccess.ErrAccessDenied
		}
		// Own the entire request before validation: a later provider call may
		// reuse or mutate any previously returned request, including nested data.
		publish = proto.Clone(publish).(*csi.NodePublishVolumeRequest)
		if len(publish.Secrets) != 0 || publish.TargetPath != mount.PublishRequest.TargetPath || publish.Readonly != mount.PublishRequest.Readonly ||
			!maps.Equal(publish.VolumeContext, mount.PublishRequest.VolumeContext) {
			return nil, "", resourceaccess.ErrAccessDenied
		}
		// The outer execution preflight validates PublishContext/binary routing.
		result = append(result, config.MountConfig{Driver: mount.Driver, PublishRequest: publish})
	}
	if err := ctx.Err(); err != nil {
		return nil, "", err
	}
	return result, metadata, nil
}

func isolatedReadError(err error) error {
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) {
		return resourceaccess.ErrAccessDenied
	}
	return fmt.Errorf("%w: %w", resourceaccess.ErrBackendRead, err)
}
