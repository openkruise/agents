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

package e2b

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// CreateVolume creates a new persistent volume for the user.
// Implementation: Creates a PVC and waits for it to bind to a PV.
// Returns the volumeID (PV name) once binding is complete.
func (sc *Controller) CreateVolume(r *http.Request) (web.ApiResponse[*models.Volume], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	// Get authenticated user & determine namespace
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.Volume]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}
	namespace := sc.getNamespaceOfUser(user)
	if namespace == "" {
		namespace = sc.systemNamespace
	}

	// Parse request body and headers
	req, parseErr := sc.parseCreateVolumeRequest(ctx, r)
	if parseErr != nil {
		return web.ApiResponse[*models.Volume]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: parseErr.Error(),
		}
	}
	log.Info("create volume request received", "request", req)

	// Call infra layer to create volume
	volumeInfo, err := sc.manager.GetInfra().CreateVolume(ctx, infra.CreateVolumeOptions{
		Namespace:        namespace,
		Name:             req.Name,
		UserID:           user.ID.String(),
		StorageSize:      req.Extensions.StorageSize,
		StorageClass:     req.Extensions.StorageClass,
		AccessMode:       req.Extensions.AccessMode,
		WaitBoundTimeout: req.Extensions.WaitBoundSeconds,
	})
	if err != nil {
		log.Error(err, "Failed to create volume", "name", req.Name, "namespace", namespace)
		if managererrors.GetErrCode(err) == managererrors.ErrorNotAllowed {
			return web.ApiResponse[*models.Volume]{}, &web.ApiError{
				Code:    http.StatusForbidden,
				Message: "Permission denied",
			}
		}
		return web.ApiResponse[*models.Volume]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to create volume: %v", err),
		}
	}
	log.Info("Volume created", "name", volumeInfo.Name, "namespace", namespace, "size", req.Extensions.StorageSize)
	return web.ApiResponse[*models.Volume]{
		Code: http.StatusCreated,
		Body: &models.Volume{
			Name:     volumeInfo.Name,
			VolumeID: volumeInfo.VolumeID,
		},
	}, nil
}

func (sc *Controller) parseCreateVolumeRequest(ctx context.Context, r *http.Request) (models.NewVolumeRequest, error) {
	var request models.NewVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, fmt.Errorf("invalid request body: %w", err)
	}

	if err := request.ParseExtensions(r.Header); err != nil {
		return request, fmt.Errorf("bad extension param: %w", err)
	}

	if err := sc.validateVolumeRequest(ctx, &request); err != nil {
		return request, err
	}
	return request, nil
}

func (sc *Controller) validateVolumeRequest(ctx context.Context, request *models.NewVolumeRequest) error {
	// Validate volume name conforms to DNS-1123 label format
	if errs := utilvalidation.IsDNS1123Label(request.Name); len(errs) > 0 {
		return fmt.Errorf("invalid volume name %q: %s", request.Name, strings.Join(errs, ", "))
	}
	// Validate AccessMode
	if err := validateAccessMode(request.Extensions.AccessMode); err != nil {
		return err
	}
	if request.Extensions.StorageSize.IsZero() || request.Extensions.StorageSize.Cmp(resource.Quantity{}) < 0 {
		return fmt.Errorf("invalid storage size: must be a positive value")
	}
	// Validate StorageClass
	if err := sc.validateStorageClass(ctx, request.Extensions.StorageClass); err != nil {
		return fmt.Errorf("invalid storage class: %w", err)
	}
	// Set default WaitBoundSeconds if not specified
	if request.Extensions.WaitBoundSeconds <= 0 {
		request.Extensions.WaitBoundSeconds = consts.DefaultWaitBoundPVCTimeout
	}
	return nil
}

// validateAccessMode checks if the access mode is valid.
func validateAccessMode(accessMode string) error {
	switch accessMode {
	case "":
		return fmt.Errorf("access mode is required")
	case string(corev1.ReadWriteOnce), string(corev1.ReadOnlyMany), string(corev1.ReadWriteMany), string(corev1.ReadWriteOncePod):
		return nil
	default:
		return fmt.Errorf("unsupported access mode %q, must be one of: ReadWriteOnce, ReadOnlyMany, ReadWriteMany, ReadWriteOncePod", accessMode)
	}
}

// validateStorageClass checks if the StorageClass exists and uses Immediate binding mode.
func (sc *Controller) validateStorageClass(ctx context.Context, storageClassName string) error {
	if storageClassName == "" {
		return fmt.Errorf("storage class is required")
	}
	// StorageClass is a cluster-scoped resource, no namespace needed
	storageClass := &storagev1.StorageClass{}
	err := sc.manager.GetInfra().GetCache().GetClient().Get(ctx, client.ObjectKey{Name: storageClassName}, storageClass)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("storage class %q not found", storageClassName)
		}
		return fmt.Errorf("failed to get storage class %q: %w", storageClassName, err)
	}
	// Check volume binding mode
	if storageClass.VolumeBindingMode != nil && *storageClass.VolumeBindingMode == storagev1.VolumeBindingWaitForFirstConsumer {
		return fmt.Errorf("storage class %q uses WaitForFirstConsumer binding mode, which is not supported. Please use a StorageClass with Immediate binding mode", storageClassName)
	}
	return nil
}

// ListVolumes lists all volumes for the authenticated user
func (sc *Controller) ListVolumes(r *http.Request) (web.ApiResponse[[]*models.Volume], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[[]*models.Volume]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}
	namespace := sc.getNamespaceOfUser(user)
	if namespace == "" {
		namespace = sc.systemNamespace
	}

	volumes, err := sc.manager.GetInfra().ListVolumes(ctx, infra.ListVolumesOptions{
		Namespace: namespace,
		UserID:    user.ID.String(),
	})
	if err != nil {
		log.Error(err, "Failed to list volumes", "namespace", namespace, "user", user.ID)
		return web.ApiResponse[[]*models.Volume]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Failed to list volumes",
		}
	}

	result := make([]*models.Volume, 0, len(volumes))
	for _, v := range volumes {
		result = append(result, &models.Volume{
			Name:     v.Name,
			VolumeID: v.VolumeID,
		})
	}

	return web.ApiResponse[[]*models.Volume]{
		Code: http.StatusOK,
		Body: result,
	}, nil
}

// GetVolume retrieves a volume by volumeID
func (sc *Controller) GetVolume(r *http.Request) (web.ApiResponse[*models.Volume], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.Volume]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}
	namespace := sc.getNamespaceOfUser(user)
	if namespace == "" {
		namespace = sc.systemNamespace
	}

	volumeID := r.PathValue("volumeID")
	if volumeID == "" {
		return web.ApiResponse[*models.Volume]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "volumeID is required",
		}
	}

	volumeInfo, err := sc.manager.GetInfra().GetVolume(ctx, infra.GetVolumeOptions{
		Namespace: namespace,
		VolumeID:  volumeID,
		UserID:    user.ID.String(),
	})
	if err != nil {
		log.Error(err, "Failed to get volume", "volumeID", volumeID, "namespace", namespace)
		if managererrors.GetErrCode(err) == managererrors.ErrorNotFound {
			return web.ApiResponse[*models.Volume]{}, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: "Volume not found",
			}
		}
		return web.ApiResponse[*models.Volume]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Failed to get volume",
		}
	}

	return web.ApiResponse[*models.Volume]{
		Code: http.StatusOK,
		Body: &models.Volume{
			Name:     volumeInfo.Name,
			VolumeID: volumeInfo.VolumeID,
		},
	}, nil
}

// DeleteVolume deletes a volume by volumeID
func (sc *Controller) DeleteVolume(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}
	namespace := sc.getNamespaceOfUser(user)
	if namespace == "" {
		namespace = sc.systemNamespace
	}

	volumeID := r.PathValue("volumeID")
	if volumeID == "" {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "volumeID is required",
		}
	}

	if err := sc.manager.GetInfra().DeleteVolume(ctx, infra.DeleteVolumeOptions{
		Namespace: namespace,
		VolumeID:  volumeID,
		UserID:    user.ID.String(),
	}); err != nil {
		log.Error(err, "Failed to delete volume", "volumeID", volumeID, "namespace", namespace)
		if managererrors.GetErrCode(err) == managererrors.ErrorNotFound {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: "Volume not found",
			}
		}
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Failed to delete volume",
		}
	}

	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}
