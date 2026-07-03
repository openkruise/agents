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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	cacheutils "github.com/openkruise/agents/pkg/cache/utils"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
)

// CreateVolume creates a new PersistentVolumeClaim for the user
func (i *Infra) CreateVolume(ctx context.Context, opts infra.CreateVolumeOptions) (*infra.VolumeInfo, error) {
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
				return nil, fmt.Errorf("failed to get existing PVC: %w", getErr)
			}
			if owner := existingPVC.GetAnnotations()[agentsv1alpha1.AnnotationOwner]; owner != opts.UserID {
				log.Error(err, "PVC exists but belongs to another user", "name", opts.Name, "namespace", opts.Namespace, "owner", owner)
				return nil, managererrors.NewError(managererrors.ErrorNotAllowed, "volume %s is owned by another user", opts.Name)
			}
			// Same user — if already bound, return idempotent response immediately
			if existingPVC.Spec.VolumeName != "" && existingPVC.Status.Phase == corev1.ClaimBound {
				log.Info("PVC already exists and bound, returning existing volume", "name", opts.Name, "namespace", opts.Namespace)
				return &infra.VolumeInfo{
					Name:     existingPVC.Name,
					VolumeID: existingPVC.Spec.VolumeName,
				}, nil
			}
			// PVC exists but not yet bound — fall through to wait logic
			pvc = existingPVC
			log.Info("PVC already exists but not bound, waiting for binding", "name", opts.Name, "namespace", opts.Namespace)
		} else {
			log.Error(err, "Failed to create PVC", "name", opts.Name, "namespace", opts.Namespace)
			return nil, fmt.Errorf("failed to create PVC: %w", err)
		}
	}

	log.Info("PVC created, waiting", "name", pvc.Name, "namespace", opts.Namespace)

	// Wait for PVC to be bound using cache-backed wait task
	task := i.Cache.NewPVCTask(ctx, pvc)
	if err := task.Wait(opts.WaitBoundTimeout); err != nil {
		log.Error(err, "Failed to wait for PVC", "name", opts.Name, "namespace", opts.Namespace)
		if errors.Is(err, cacheutils.ErrWaitNotSatisfied) {
			return nil, fmt.Errorf("PVC %s/%s binding timed out after %v: %w", opts.Namespace, opts.Name, opts.WaitBoundTimeout, err)
		}
		return nil, fmt.Errorf("failed to wait for PVC %s/%s: %w", opts.Namespace, opts.Name, err)
	}

	// Get the bound PVC from cache
	boundPVC := &corev1.PersistentVolumeClaim{}
	if err := i.Cache.GetClient().Get(ctx, client.ObjectKey{Namespace: opts.Namespace, Name: opts.Name}, boundPVC); err != nil {
		log.Error(err, "Failed to get bound PVC", "name", opts.Name, "namespace", opts.Namespace)
		return nil, fmt.Errorf("failed to get bound PVC: %w", err)
	}

	log.Info("Volume created and bound", "name", boundPVC.Name, "namespace", opts.Namespace, "volumeID", boundPVC.Spec.VolumeName)

	return &infra.VolumeInfo{
		Name:     boundPVC.Name,
		VolumeID: boundPVC.Spec.VolumeName,
	}, nil
}

// ListVolumes lists all volumes for a user in a namespace
func (i *Infra) ListVolumes(ctx context.Context, opts infra.ListVolumesOptions) ([]*infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	log.V(utils.DebugLogLevel).Info("list volumes", "userId", opts.UserID, "namespace", opts.Namespace)

	// List PVCs using User index for efficient filtering
	pvcList := &corev1.PersistentVolumeClaimList{}
	err := i.Cache.GetClient().List(ctx, pvcList,
		client.InNamespace(opts.Namespace),
		client.MatchingFields{cache.IndexUser: opts.UserID},
	)
	if err != nil {
		log.Error(err, "Failed to list PVCs", "namespace", opts.Namespace, "userId", opts.UserID)
		return nil, fmt.Errorf("failed to list PVCs: %w", err)
	}

	volumes := make([]*infra.VolumeInfo, 0, len(pvcList.Items))
	for _, pvc := range pvcList.Items {
		volumes = append(volumes, &infra.VolumeInfo{
			Name:     pvc.Name,
			VolumeID: pvc.Spec.VolumeName,
		})
	}

	return volumes, nil
}

// GetVolume retrieves a volume by volumeID (PV Name)
func (i *Infra) GetVolume(ctx context.Context, opts infra.GetVolumeOptions) (*infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	log.V(utils.DebugLogLevel).Info("get volume", "volumeID", opts.VolumeID, "namespace", opts.Namespace, "userId", opts.UserID)

	// Find PVC by VolumeName (PV Name) using index
	pvcList := &corev1.PersistentVolumeClaimList{}
	err := i.Cache.GetClient().List(ctx, pvcList,
		client.InNamespace(opts.Namespace),
		client.MatchingFields{cache.IndexVolumeName: opts.VolumeID},
	)
	if err != nil {
		log.Error(err, "Failed to list PVCs", "namespace", opts.Namespace)
		return nil, fmt.Errorf("failed to list PVCs: %w", err)
	}

	// No PVC found bound to this PV
	if len(pvcList.Items) == 0 {
		return nil, managererrors.NewError(managererrors.ErrorNotFound, "volume not found: %s", opts.VolumeID)
	}

	pvc := &pvcList.Items[0]
	volumeInfo := &infra.VolumeInfo{
		Name:     pvc.Name,
		VolumeID: pvc.Spec.VolumeName,
	}
	return volumeInfo, nil
}

// DeleteVolume deletes a volume by volumeID (PV Name)
func (i *Infra) DeleteVolume(ctx context.Context, opts infra.DeleteVolumeOptions) error {
	log := klog.FromContext(ctx)
	log.V(utils.DebugLogLevel).Info("delete volume", "volumeID", opts.VolumeID, "namespace", opts.Namespace, "userId", opts.UserID)

	// Find PVC by VolumeName (PV Name) using index
	pvcList := &corev1.PersistentVolumeClaimList{}
	err := i.Cache.GetClient().List(ctx, pvcList,
		client.InNamespace(opts.Namespace),
		client.MatchingFields{cache.IndexVolumeName: opts.VolumeID},
	)
	if err != nil {
		log.Error(err, "Failed to list PVCs", "namespace", opts.Namespace)
		return fmt.Errorf("failed to list PVCs: %w", err)
	}

	// Return error if no PVC found
	if len(pvcList.Items) == 0 {
		return managererrors.NewError(managererrors.ErrorNotFound, "volume not found: %s", opts.VolumeID)
	}

	pvc := &pvcList.Items[0]
	// Delete the PVC, ignore NotFound error to handle race conditions
	if err := client.IgnoreNotFound(i.Cache.GetClient().Delete(ctx, pvc)); err != nil {
		log.Error(err, "Failed to delete PVC", "name", pvc.Name, "namespace", opts.Namespace)
		return fmt.Errorf("failed to delete PVC: %w", err)
	}

	log.Info("Volume deleted", "name", pvc.Name, "namespace", opts.Namespace, "volumeID", opts.VolumeID)
	return nil
}
