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

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/utils"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CreateVolume creates a new PersistentVolumeClaim for the user
func (i *Infra) CreateVolume(ctx context.Context, opts infra.CreateVolumeOptions) (*infra.VolumeInfo, error) {
	log := klog.FromContext(ctx)
	log.V(utils.DebugLogLevel).Info("create volume options", "options", opts)

	// Pre-flight checks
	if err := i.validateCreateVolumeOptions(ctx, &opts); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

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
		log.Error(err, "Failed to create PVC", "name", opts.Name, "namespace", opts.Namespace)
		return nil, fmt.Errorf("failed to create PVC: %w", err)
	}

	log.Info("PVC created, waiting", "name", pvc.Name, "namespace", opts.Namespace)

	// Wait for PVC to be bound using cache-backed wait task
	task := i.Cache.NewPVCTask(ctx, pvc)
	if err := task.Wait(opts.WaitBoundTimeout); err != nil {
		log.Error(err, "Failed to wait for PVC", "name", opts.Name, "namespace", opts.Namespace)
		// Clean up the PVC that failed to bind to prevent resource leak
		if deleteErr := i.Cache.GetClient().Delete(ctx, pvc); deleteErr != nil && !apierrors.IsNotFound(deleteErr) {
			log.Error(deleteErr, "Failed to clean up PVC after bind failure", "name", opts.Name, "namespace", opts.Namespace)
		}
		return nil, fmt.Errorf("failed to wait for PVC: %w", err)
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

// GetVolume retrieves a volume by volumeID (PV Name) and verifies ownership.
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

	// Verify ownership if UserID is specified
	if opts.UserID != "" && pvc.GetAnnotations()[agentsv1alpha1.AnnotationOwner] != opts.UserID {
		return nil, managererrors.NewError(managererrors.ErrorNotAllowed,
			"volume %s is not owned by user %s", opts.VolumeID, opts.UserID)
	}

	volumeInfo := &infra.VolumeInfo{
		Name:     pvc.Name,
		VolumeID: pvc.Spec.VolumeName,
	}
	return volumeInfo, nil
}

// DeleteVolume deletes a volume by volumeID (PV Name) and verifies ownership.
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

	// Verify ownership if UserID is specified
	if opts.UserID != "" && pvc.GetAnnotations()[agentsv1alpha1.AnnotationOwner] != opts.UserID {
		return managererrors.NewError(managererrors.ErrorNotAllowed,
			"volume %s is not owned by user %s", opts.VolumeID, opts.UserID)
	}

	// Delete the PVC
	if err := i.Cache.GetClient().Delete(ctx, pvc); err != nil {
		log.Error(err, "Failed to delete PVC", "name", pvc.Name, "namespace", opts.Namespace)
		return fmt.Errorf("failed to delete PVC: %w", err)
	}

	log.Info("Volume deleted", "name", pvc.Name, "namespace", opts.Namespace, "volumeID", opts.VolumeID)
	return nil
}

// validateCreateVolumeOptions validates volume creation parameters before creating PVC.
func (i *Infra) validateCreateVolumeOptions(ctx context.Context, opts *infra.CreateVolumeOptions) error {
	if opts.WaitBoundTimeout <= 0 {
		opts.WaitBoundTimeout = consts.DefaultWaitBoundPVCTimeout
	}
	// Validate storage size is set and positive
	if opts.StorageSize.IsZero() || opts.StorageSize.Cmp(resource.Quantity{}) < 0 {
		return fmt.Errorf("invalid storage size: must be a positive value")
	}
	// Validate AccessMode
	if err := validateAccessMode(opts.AccessMode); err != nil {
		return fmt.Errorf("invalid access mode: %w", err)
	}
	// Validate StorageClass if specified
	if err := i.validateStorageClass(ctx, opts.StorageClass); err != nil {
		return fmt.Errorf("invalid storage class: %w", err)
	}
	return nil
}

// validateAccessMode checks if the access mode is valid.
func validateAccessMode(accessMode string) error {
	switch accessMode {
	case string(corev1.ReadWriteOnce), string(corev1.ReadOnlyMany), string(corev1.ReadWriteMany), string(corev1.ReadWriteOncePod):
		return nil
	default:
		return fmt.Errorf("unsupported access mode %q, must be one of: ReadWriteOnce, ReadOnlyMany, ReadWriteMany, ReadWriteOncePod", accessMode)
	}
}

// validateStorageClass checks if the StorageClass exists and uses Immediate binding mode.
func (i *Infra) validateStorageClass(ctx context.Context, storageClassName string) error {
	// StorageClass is a cluster-scoped resource, no namespace needed
	sc := &storagev1.StorageClass{}
	err := i.Cache.GetClient().Get(ctx, client.ObjectKey{Name: storageClassName}, sc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("storage class %q not found", storageClassName)
		}
		return fmt.Errorf("failed to get storage class %q: %w", storageClassName, err)
	}

	// Check volume binding mode
	if sc.VolumeBindingMode != nil && *sc.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer {
		return fmt.Errorf("storage class %q uses WaitForFirstConsumer binding mode, which is not supported. Please use a StorageClass with Immediate binding mode", storageClassName)
	}

	return nil
}
