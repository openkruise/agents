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

package sandboxcr

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
)

// RegisterVolume registers an existing PV as a named volume under the given namespace.
// It validates that the PV exists, is Available, has sufficient capacity, and is not
// already registered. On success it patches the PV with the owner-namespace and
// volume-name labels.
func (i *Infra) RegisterVolume(ctx context.Context, opts infra.RegisterVolumeOptions) (infra.VolumeInfo, error) {
	log := klog.FromContext(ctx).WithValues("pvName", opts.PvName, "namespace", opts.Namespace, "name", opts.Name)

	// Validate required fields upfront.
	switch {
	case opts.Namespace == "":
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorBadRequest, "namespace is required")
	case opts.Name == "":
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorBadRequest, "name is required")
	case opts.PvName == "":
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorBadRequest, "pvName is required")
	case opts.SizeGB <= 0:
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorBadRequest, "sizeGB must be a positive integer")
	}

	// Read the PV directly from the API server to get a fresh copy.
	pv := &corev1.PersistentVolume{}
	if err := i.APIReader.Get(ctx, types.NamespacedName{Name: opts.PvName}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "pv %s not found", opts.PvName)
		}
		log.Error(err, "failed to get pv from api server")
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorInternal, "failed to get pv %s: %v", opts.PvName, err)
	}

	// Validate phase.
	if pv.Status.Phase != corev1.VolumeAvailable {
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorBadRequest,
			"pv %s is not in Available phase (current phase: %s)", opts.PvName, pv.Status.Phase)
	}

	// Validate capacity.
	requested := resource.MustParse(fmt.Sprintf("%dGi", opts.SizeGB))
	if pvCapacity, ok := pv.Spec.Capacity[corev1.ResourceStorage]; !ok || pvCapacity.Cmp(requested) < 0 {
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorBadRequest,
			"pv %s capacity is insufficient for requested %dGi", opts.PvName, opts.SizeGB)
	}

	// Check whether PV is already registered.
	if existingNS, exists := pv.Labels[v1alpha1.LabelVolumeOwnerNamespace]; exists {
		if existingNS == opts.Namespace {
			return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorConflict,
				"pv %s is already registered under namespace %s", opts.PvName, opts.Namespace)
		}
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotAllowed,
			"pv %s is already registered under a different namespace %s", opts.PvName, existingNS)
	}

	// Check volume name uniqueness within the namespace.
	pvList := &corev1.PersistentVolumeList{}
	if err := i.Cache.GetClient().List(ctx, pvList, client.MatchingLabels{
		v1alpha1.LabelVolumeOwnerNamespace: opts.Namespace,
		v1alpha1.LabelVolumeName:           opts.Name,
	}); err != nil {
		log.Error(err, "failed to list pvs for name uniqueness check")
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorInternal, "failed to check volume name uniqueness: %v", err)
	}
	if len(pvList.Items) > 0 {
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorConflict,
			"volume name %s is already in use within namespace %s", opts.Name, opts.Namespace)
	}

	// Patch the PV to add the owner labels.
	base := pv.DeepCopy()
	if pv.Labels == nil {
		pv.Labels = make(map[string]string)
	}
	pv.Labels[v1alpha1.LabelVolumeOwnerNamespace] = opts.Namespace
	pv.Labels[v1alpha1.LabelVolumeName] = opts.Name

	if err := i.Cache.GetClient().Patch(ctx, pv, client.MergeFrom(base)); err != nil {
		log.Error(err, "failed to patch pv labels")
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorInternal, "failed to register volume: %v", err)
	}

	log.Info("volume registered successfully")
	return pvToVolumeInfo(pv), nil
}

// ListVolumes returns all volumes registered under the given namespace by reading
// from the informer cache.
func (i *Infra) ListVolumes(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
	log := klog.FromContext(ctx).WithValues("namespace", opts.Namespace)

	pvList := &corev1.PersistentVolumeList{}
	if err := i.Cache.GetClient().List(ctx, pvList, client.MatchingLabels{
		v1alpha1.LabelVolumeOwnerNamespace: opts.Namespace,
	}); err != nil {
		log.Error(err, "failed to list pvs from cache")
		return nil, managererrors.NewError(managererrors.ErrorInternal, "failed to list volumes: %v", err)
	}

	result := make([]infra.VolumeInfo, 0, len(pvList.Items))
	for idx := range pvList.Items {
		result = append(result, pvToVolumeInfo(&pvList.Items[idx]))
	}
	return result, nil
}

// GetVolume retrieves a single volume by its ID (PV name) from the informer cache.
// Returns ErrorNotFound if the PV does not exist or is owned by a different namespace
// (no information disclosure).
func (i *Infra) GetVolume(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
	log := klog.FromContext(ctx).WithValues("volumeID", opts.VolumeID, "namespace", opts.Namespace)

	pv := &corev1.PersistentVolume{}
	if err := i.Cache.GetClient().Get(ctx, client.ObjectKey{Name: opts.VolumeID}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "volume %s not found", opts.VolumeID)
		}
		log.Error(err, "failed to get pv from cache")
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorInternal, "failed to get volume %s: %v", opts.VolumeID, err)
	}

	// Enforce namespace isolation — no information disclosure.
	if pv.Labels[v1alpha1.LabelVolumeOwnerNamespace] != opts.Namespace {
		return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "volume %s not found", opts.VolumeID)
	}

	return pvToVolumeInfo(pv), nil
}

// DeleteVolume unregisters a volume by removing its owner labels from the PV.
// Volume usage is derived live from SandboxClaims — no PV annotation needed,
// no cleanup logic, no stale state.
// If the volume is currently in use and Force is false, returns ErrorConflict.
func (i *Infra) DeleteVolume(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
	log := klog.FromContext(ctx).WithValues("volumeID", opts.VolumeID, "namespace", opts.Namespace, "force", opts.Force)

	// Read PV fresh from the API server to avoid stale cached data.
	pv := &corev1.PersistentVolume{}
	if err := i.APIReader.Get(ctx, types.NamespacedName{Name: opts.VolumeID}, pv); err != nil {
		if apierrors.IsNotFound(err) {
			return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorNotFound, "volume %s not found", opts.VolumeID)
		}
		log.Error(err, "failed to get pv from api server")
		return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorInternal, "failed to get volume %s: %v", opts.VolumeID, err)
	}

	// Namespace ownership check — no information disclosure.
	if pv.Labels[v1alpha1.LabelVolumeOwnerNamespace] != opts.Namespace {
		return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorNotFound, "volume %s not found", opts.VolumeID)
	}

	// Derive live usage from SandboxClaims — no stale annotation risk.
	sandboxIDs, err := i.getVolumeUsers(ctx, opts.Namespace, opts.VolumeID)
	if err != nil {
		log.Error(err, "failed to derive volume usage from SandboxClaims")
		return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorInternal, "failed to derive volume usage: %v", err)
	}

	// Block deletion if volume is in use and force is not set.
	if len(sandboxIDs) > 0 && !opts.Force {
		return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorConflict,
			"volume is mounted by: %v", sandboxIDs)
	}

	// Remove only the volume owner labels — no annotation to clean up.
	base := pv.DeepCopy()
	delete(pv.Labels, v1alpha1.LabelVolumeOwnerNamespace)
	delete(pv.Labels, v1alpha1.LabelVolumeName)

	if err := i.Cache.GetClient().Patch(ctx, pv, client.MergeFrom(base)); err != nil {
		log.Error(err, "failed to patch pv to remove volume labels")
		return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorInternal, "failed to delete volume: %v", err)
	}

	log.Info("volume unregistered successfully", "affectedSandboxes", len(sandboxIDs))
	return infra.DeleteVolumeResult{
		AffectedSandboxIDs: sandboxIDs,
		ForcedDelete:       opts.Force && len(sandboxIDs) > 0,
	}, nil
}

// getVolumeUsers returns the sandbox IDs currently using the given PV, derived
// from SandboxClaim objects. SandboxClaims are the authoritative, structured
// source of truth — no PV annotation is needed.
//
// For each active SandboxClaim whose spec.dynamicVolumesMount references pvName,
// the sandbox IDs are discovered by listing Sandboxes labeled with the claim name.
func (i *Infra) getVolumeUsers(ctx context.Context, namespace, pvName string) ([]string, error) {
	claimList := &v1alpha1.SandboxClaimList{}
	listOpts := []client.ListOption{}
	if namespace != "" {
		listOpts = append(listOpts, client.InNamespace(namespace))
	}
	if err := i.Cache.GetClient().List(ctx, claimList, listOpts...); err != nil {
		return nil, err
	}

	var sandboxIDs []string
	for idx := range claimList.Items {
		claim := &claimList.Items[idx]
		if !claimReferencesPV(claim, pvName) {
			continue
		}

		// Find sandboxes claimed by this claim via the LabelSandboxClaimName label.
		sbxList := &v1alpha1.SandboxList{}
		if err := i.Cache.GetClient().List(ctx, sbxList,
			client.InNamespace(claim.Namespace),
			client.MatchingLabels{v1alpha1.LabelSandboxClaimName: claim.Name},
		); err != nil {
			return nil, fmt.Errorf("failed to list sandboxes for claim %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		for sbxIdx := range sbxList.Items {
			sandboxIDs = append(sandboxIDs, utils.GetSandboxID(&sbxList.Items[sbxIdx]))
		}
	}
	return sandboxIDs, nil
}

// claimReferencesPV reports whether a SandboxClaim's DynamicVolumesMount
// includes the given pvName.
func claimReferencesPV(claim *v1alpha1.SandboxClaim, pvName string) bool {
	for _, m := range claim.Spec.DynamicVolumesMount {
		if m.PvName == pvName {
			return true
		}
	}
	return false
}

// pvToVolumeInfo converts a PersistentVolume into a VolumeInfo struct.
func pvToVolumeInfo(pv *corev1.PersistentVolume) infra.VolumeInfo {
	var sizeGB int
	if capacity, ok := pv.Spec.Capacity[corev1.ResourceStorage]; ok {
		// Convert bytes to GiB (1 GiB = 2^30 bytes). Value() returns bytes.
		sizeGB = int(capacity.Value() / (1 << 30))
	}

	return infra.VolumeInfo{
		VolumeID:  pv.Name,
		Name:      pv.Labels[v1alpha1.LabelVolumeName],
		PvName:    pv.Name,
		SizeGB:    sizeGB,
		CreatedAt: pv.CreationTimestamp.Format(time.RFC3339),
	}
}
