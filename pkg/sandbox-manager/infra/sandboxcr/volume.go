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
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
)

// CreateVolume creates a new PersistentVolumeClaim for the user and waits for it to bind.
func (i *Infra) CreateVolume(ctx context.Context, opts infra.CreateVolumeOptions) (infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	log.V(utils.DebugLogLevel).Info("create volume options", "options", opts)

	// Create PVC
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      opts.Name,
			Namespace: opts.Namespace,
			Annotations: map[string]string{
				agentsv1alpha1.AnnotationOwner: opts.UserID,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.PersistentVolumeAccessMode(opts.AccessMode)},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: opts.StorageSize,
				},
			},
			StorageClassName: &opts.StorageClass,
		},
	}

	err := i.Cache.GetClient().Create(ctx, pvc)
	if err != nil {
		if apierrors.IsAlreadyExists(err) {
			// PVC already exists — check ownership for idempotent handling
			existingPVC := &corev1.PersistentVolumeClaim{}
			if getErr := i.Cache.GetClient().Get(ctx, client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}, existingPVC); getErr != nil {
				log.Error(getErr, "Failed to get existing PVC", "name", opts.Name, "namespace", opts.Namespace)
				return infra.VolumeInfo{}, fmt.Errorf("failed to get existing PVC: %w", getErr)
			}
			if owner := existingPVC.GetAnnotations()[agentsv1alpha1.AnnotationOwner]; owner != opts.UserID {
				log.Error(err, "PVC exists but belongs to another user", "name", opts.Name, "namespace", opts.Namespace, "owner", owner)
				return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotAllowed, "volume %s is owned by another user", opts.Name)
			}
			// Same user — if already bound, return idempotent response immediately
			if existingPVC.Spec.VolumeName != "" && existingPVC.Status.Phase == corev1.ClaimBound {
				log.Info("PVC already exists and bound, returning existing volume", "name", opts.Name, "namespace", opts.Namespace)
				return pvcToVolumeInfo(existingPVC), nil
			}
			// PVC exists but not yet bound — fall through to wait logic
			pvc = existingPVC
			log.Info("PVC already exists but not bound, waiting for binding", "name", opts.Name, "namespace", opts.Namespace)
		} else {
			log.Error(err, "Failed to create PVC", "name", opts.Name, "namespace", opts.Namespace)
			return infra.VolumeInfo{}, fmt.Errorf("failed to create PVC: %w", err)
		}
	}

	log.Info("PVC created, waiting", "name", pvc.Name, "namespace", opts.Namespace)

	// Wait for PVC to be bound using cache-backed wait task
	task := i.Cache.NewPVCTask(ctx, pvc)
	if err := task.Wait(opts.WaitBoundTimeout); err != nil {
		log.Error(err, "Failed to wait for PVC", "name", opts.Name, "namespace", opts.Namespace)
		if errors.Is(err, cacheutils.ErrWaitNotSatisfied) {
			return infra.VolumeInfo{}, fmt.Errorf("PVC %s/%s binding timed out after %v: %w", opts.Namespace, opts.Name, opts.WaitBoundTimeout, err)
		}
		return infra.VolumeInfo{}, fmt.Errorf("failed to wait for PVC %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	// Get the bound PVC from cache
	boundPVC := &corev1.PersistentVolumeClaim{}
	if err := i.Cache.GetClient().Get(ctx, client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}, boundPVC); err != nil {
		log.Error(err, "Failed to get bound PVC", "name", opts.Name, "namespace", opts.Namespace)
		return infra.VolumeInfo{}, fmt.Errorf("failed to get bound PVC: %w", err)
	}

	log.Info("Volume created and bound", "pvcName", boundPVC.Name, "namespace", opts.Namespace, "pvName", boundPVC.Spec.VolumeName)

	return pvcToVolumeInfo(boundPVC), nil
}

// ListVolumes lists all volumes (PVCs) for a user in a namespace.
func (i *Infra) ListVolumes(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	log.V(utils.DebugLogLevel).Info("list volumes", "userId", opts.UserID, "namespace", opts.Namespace)

	pvcList := &corev1.PersistentVolumeClaimList{}
	listOpts := []client.ListOption{client.InNamespace(opts.Namespace)}
	if opts.UserID != "" {
		listOpts = append(listOpts, client.MatchingFields{cache.IndexUser: opts.UserID})
	}

	err := i.Cache.GetClient().List(ctx, pvcList, listOpts...)
	if err != nil {
		log.Error(err, "Failed to list PVCs", "namespace", opts.Namespace, "userId", opts.UserID)
		return nil, fmt.Errorf("failed to list PVCs: %w", err)
	}

	volumes := make([]infra.VolumeInfo, 0, len(pvcList.Items))
	for idx := range pvcList.Items {
		volumes = append(volumes, pvcToVolumeInfo(&pvcList.Items[idx]))
	}

	return volumes, nil
}

// GetVolume retrieves a volume by its volumeID (PVC Name).
func (i *Infra) GetVolume(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	log.V(utils.DebugLogLevel).Info("get volume", "volumeID", opts.VolumeID, "namespace", opts.Namespace, "userId", opts.UserID)

	pvc := &corev1.PersistentVolumeClaim{}
	err := i.Cache.GetClient().Get(ctx, client.ObjectKey{Namespace: opts.Namespace, Name: opts.VolumeID}, pvc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "volume not found: %s", opts.VolumeID)
		}
		log.Error(err, "Failed to get PVC from cache", "name", opts.VolumeID, "namespace", opts.Namespace)
		return infra.VolumeInfo{}, fmt.Errorf("failed to get PVC %s: %w", opts.VolumeID, err)
	}

	// Enforce ownership if UserID is specified.
	if opts.UserID != "" {
		if owner := pvc.GetAnnotations()[agentsv1alpha1.AnnotationOwner]; owner != opts.UserID {
			return infra.VolumeInfo{}, managererrors.NewError(managererrors.ErrorNotFound, "volume not found: %s", opts.VolumeID)
		}
	}

	return pvcToVolumeInfo(pvc), nil
}

// DeleteVolume deletes a volume (PVC) by its volumeID (PVC Name).
// Prior to deleting, it checks active users via SandboxClaims and blocks deletion if force=false.
func (i *Infra) DeleteVolume(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error) {
	log := klog.FromContext(ctx)
	log.V(utils.DebugLogLevel).Info("delete volume", "volumeID", opts.VolumeID, "namespace", opts.Namespace, "userId", opts.UserID, "force", opts.Force)

	// Get PVC fresh from the API server to avoid stale cached data.
	pvc := &corev1.PersistentVolumeClaim{}
	err := i.APIReader.Get(ctx, types.NamespacedName{Namespace: opts.Namespace, Name: opts.VolumeID}, pvc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorNotFound, "volume not found: %s", opts.VolumeID)
		}
		log.Error(err, "Failed to get PVC from API server", "name", opts.VolumeID, "namespace", opts.Namespace)
		return infra.DeleteVolumeResult{}, fmt.Errorf("failed to get PVC %s: %w", opts.VolumeID, err)
	}

	// Enforce ownership if UserID is specified.
	if opts.UserID != "" {
		if owner := pvc.GetAnnotations()[agentsv1alpha1.AnnotationOwner]; owner != opts.UserID {
			return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorNotFound, "volume not found: %s", opts.VolumeID)
		}
	}

	// Determine active users via SandboxClaims mapping to this PVC name or its bound PV.
	sandboxIDs, err := i.getVolumeUsers(ctx, opts.Namespace, pvc.Name, pvc.Spec.VolumeName)
	if err != nil {
		log.Error(err, "failed to derive volume usage from SandboxClaims")
		return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorInternal, "failed to derive volume usage: %v", err)
	}

	// Block deletion if volume is in use and force is false.
	if len(sandboxIDs) > 0 && !opts.Force {
		return infra.DeleteVolumeResult{}, managererrors.NewError(managererrors.ErrorConflict,
			"volume is mounted by: %v", sandboxIDs)
	}

	// Delete PVC.
	if err := client.IgnoreNotFound(i.Cache.GetClient().Delete(ctx, pvc)); err != nil {
		log.Error(err, "Failed to delete PVC", "name", pvc.Name, "namespace", opts.Namespace)
		return infra.DeleteVolumeResult{}, fmt.Errorf("failed to delete PVC: %w", err)
	}

	log.Info("Volume deleted successfully", "name", pvc.Name, "namespace", opts.Namespace, "affectedSandboxes", len(sandboxIDs))
	return infra.DeleteVolumeResult{
		AffectedSandboxIDs: sandboxIDs,
		ForcedDelete:       opts.Force && len(sandboxIDs) > 0,
	}, nil
}

// getVolumeUsers returns the sandbox IDs currently using the given PVC/PV, derived from SandboxClaims.
func (i *Infra) getVolumeUsers(ctx context.Context, namespace, pvcName, pvName string) ([]string, error) {
	claimList := &agentsv1alpha1.SandboxClaimList{}
	listOpts := []client.ListOption{client.InNamespace(namespace)}
	if err := i.Cache.GetClient().List(ctx, claimList, listOpts...); err != nil {
		return nil, err
	}

	var sandboxIDs []string
	for idx := range claimList.Items {
		claim := &claimList.Items[idx]
		if !claimReferencesPV(claim, pvcName, pvName) {
			continue
		}

		// Find sandboxes claimed by this claim via the LabelSandboxClaimName label.
		sbxList := &agentsv1alpha1.SandboxList{}
		if err := i.Cache.GetClient().List(ctx, sbxList,
			client.InNamespace(claim.Namespace),
			client.MatchingLabels{agentsv1alpha1.LabelSandboxClaimName: claim.Name},
		); err != nil {
			return nil, fmt.Errorf("failed to list sandboxes for claim %s/%s: %w", claim.Namespace, claim.Name, err)
		}
		for sbxIdx := range sbxList.Items {
			sandboxIDs = append(sandboxIDs, utils.GetSandboxID(&sbxList.Items[sbxIdx]))
		}
	}
	return sandboxIDs, nil
}

// claimReferencesPV checks if a SandboxClaim mounts either the PVC name or resolved PV name.
func claimReferencesPV(claim *agentsv1alpha1.SandboxClaim, pvcName, pvName string) bool {
	for _, m := range claim.Spec.DynamicVolumesMount {
		if m.PvName == pvcName || (pvName != "" && m.PvName == pvName) {
			return true
		}
	}
	return false
}

// pvcToVolumeInfo converts a PersistentVolumeClaim into a VolumeInfo struct.
func pvcToVolumeInfo(pvc *corev1.PersistentVolumeClaim) infra.VolumeInfo {
	var sizeGB int
	if capacity, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		// Convert bytes to GiB (1 GiB = 2^30 bytes).
		sizeGB = int(capacity.Value() / (1 << 30))
	} else if capacity, ok := pvc.Status.Capacity[corev1.ResourceStorage]; ok {
		sizeGB = int(capacity.Value() / (1 << 30))
	}

	return infra.VolumeInfo{
		VolumeID:  pvc.Name,
		Name:      pvc.Name,
		PvName:    pvc.Spec.VolumeName,
		SizeGB:    sizeGB,
		CreatedAt: pvc.CreationTimestamp.Format(time.RFC3339),
	}
}
