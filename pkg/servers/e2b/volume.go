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

	"github.com/openkruise/agents/api/v1alpha1"
	"github.com/openkruise/agents/pkg/sandbox-manager/consts"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// volumeManagerInterface allows mocking the volume manager in tests.
type volumeManagerInterface interface {
	CreateVolume(ctx context.Context, opts infra.CreateVolumeOptions) (infra.VolumeInfo, error)
	ListVolumes(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error)
	GetVolume(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error)
	DeleteVolume(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error)
}

// CreateVolume creates a new persistent volume (PVC) for the user.
// Implementation: Creates a PVC and waits for it to bind to a PV.
func (sc *Controller) CreateVolume(r *http.Request) (web.ApiResponse[*models.VolumeResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	// Get authenticated user & determine namespace
	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
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
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: parseErr.Error(),
		}
	}
	log.Info("create volume request received", "request", req)

	// Call volume manager (metrics wrapper → infra) to create volume
	volumeInfo, err := sc.volumeManager.CreateVolume(ctx, infra.CreateVolumeOptions{
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
			return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
				Code:    http.StatusForbidden,
				Message: "Permission denied",
			}
		}
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    mapVolumeErrorToHTTP(managererrors.GetErrCode(err)),
			Message: fmt.Sprintf("Failed to create volume: %v", err),
		}
	}
	log.Info("Volume created", "name", volumeInfo.Name, "namespace", namespace, "size", req.Extensions.StorageSize)
	return web.ApiResponse[*models.VolumeResponse]{
		Code: http.StatusCreated,
		Body: volumeInfoToResponse(volumeInfo),
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

// ListVolumes handles GET /volumes — lists all volumes in the caller's namespace.
func (sc *Controller) ListVolumes(r *http.Request) (web.ApiResponse[[]models.VolumeResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[[]models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}

	namespace := sc.getNamespaceOfUser(user)
	log.Info("list volumes request", "namespace", namespace)

	volumes, err := sc.volumeManager.ListVolumes(ctx, infra.ListVolumesOptions{
		Namespace: namespace,
		UserID:    user.ID.String(),
	})
	if err != nil {
		log.Error(err, "failed to list volumes")
		return web.ApiResponse[[]models.VolumeResponse]{}, &web.ApiError{
			Code:    mapVolumeErrorToHTTP(managererrors.GetErrCode(err)),
			Message: err.Error(),
		}
	}

	result := make([]models.VolumeResponse, 0, len(volumes))
	for _, v := range volumes {
		result = append(result, *volumeInfoToResponse(v))
	}

	return web.ApiResponse[[]models.VolumeResponse]{
		Code: http.StatusOK,
		Body: result,
	}, nil
}

// GetVolume handles GET /volumes/{volumeID} — retrieves a single volume by ID.
func (sc *Controller) GetVolume(r *http.Request) (web.ApiResponse[*models.VolumeResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}

	volumeID := r.PathValue("volumeID")
	if volumeID == "" {
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "volumeID is required",
		}
	}
	namespace := sc.getNamespaceOfUser(user)
	log.Info("get volume request", "namespace", namespace, "volumeID", volumeID)

	info, err := sc.volumeManager.GetVolume(ctx, infra.GetVolumeOptions{
		Namespace: namespace,
		VolumeID:  volumeID,
		UserID:    user.ID.String(),
	})
	if err != nil {
		log.Error(err, "failed to get volume")
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    mapVolumeErrorToHTTP(managererrors.GetErrCode(err)),
			Message: err.Error(),
		}
	}

	return web.ApiResponse[*models.VolumeResponse]{
		Code: http.StatusOK,
		Body: volumeInfoToResponse(info),
	}, nil
}

// DeleteVolume handles DELETE /volumes/{volumeID} — deletes a volume.
// Accepts optional ?force=true query parameter to force deletion even when mounted.
func (sc *Controller) DeleteVolume(r *http.Request) (web.ApiResponse[*models.DeleteVolumeResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.DeleteVolumeResponse]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User is empty",
		}
	}

	volumeID := r.PathValue("volumeID")
	if volumeID == "" {
		return web.ApiResponse[*models.DeleteVolumeResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "volumeID is required",
		}
	}
	force := r.URL.Query().Get("force") == "true"
	namespace := sc.getNamespaceOfUser(user)
	log.Info("delete volume request", "namespace", namespace, "volumeID", volumeID, "force", force)

	result, err := sc.volumeManager.DeleteVolume(ctx, infra.DeleteVolumeOptions{
		Namespace: namespace,
		VolumeID:  volumeID,
		UserID:    user.ID.String(),
		Force:     force,
	})
	if err != nil {
		log.Error(err, "failed to delete volume")
		return web.ApiResponse[*models.DeleteVolumeResponse]{}, &web.ApiError{
			Code:    mapVolumeErrorToHTTP(managererrors.GetErrCode(err)),
			Message: err.Error(),
		}
	}

	resp := &models.DeleteVolumeResponse{}
	if result.ForcedDelete {
		resp.Warning = "volume was forcibly removed while mounted"
		resp.AffectedBy = result.AffectedSandboxIDs
	}

	return web.ApiResponse[*models.DeleteVolumeResponse]{
		Code: http.StatusOK,
		Body: resp,
	}, nil
}

// resolveVolumeMounts translates a slice of VolumeMountRequest into
// v1alpha1.CSIMountConfig values by verifying that each volumeID is registered
// under the caller's namespace. Returns an error if any volumeID is not found
// or belongs to a different namespace.
func (sc *Controller) resolveVolumeMounts(ctx context.Context, namespace string, mounts []models.VolumeMountRequest) ([]v1alpha1.CSIMountConfig, error) {
	if len(mounts) == 0 {
		return nil, nil
	}
	configs := make([]v1alpha1.CSIMountConfig, 0, len(mounts))
	for _, m := range mounts {
		info, err := sc.manager.GetInfra().GetVolume(ctx, infra.GetVolumeOptions{
			Namespace: namespace,
			VolumeID:  m.VolumeID,
			// UserID is intentionally empty: namespace already scopes to one team
			// (admin → sandbox-system, others → team.Name), so namespace isolation
			// is the ownership boundary for internal mount resolution.
			UserID: "",
		})
		if err != nil {
			return nil, fmt.Errorf("volume %s: %w", m.VolumeID, err)
		}
		configs = append(configs, v1alpha1.CSIMountConfig{
			PvName:    info.PvName,
			MountPath: m.MountPath,
			ReadOnly:  m.ReadOnly,
		})
	}
	return configs, nil
}

// mapVolumeErrorToHTTP maps a managererrors.ErrorCode to an HTTP status code.
func mapVolumeErrorToHTTP(code managererrors.ErrorCode) int {
	switch code {
	case managererrors.ErrorNotFound:
		return http.StatusNotFound
	case managererrors.ErrorConflict:
		return http.StatusConflict
	case managererrors.ErrorNotAllowed:
		return http.StatusForbidden
	case managererrors.ErrorBadRequest:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// volumeInfoToResponse converts an infra.VolumeInfo to an API response model.
func volumeInfoToResponse(v infra.VolumeInfo) *models.VolumeResponse {
	return &models.VolumeResponse{
		VolumeID:  v.VolumeID,
		Name:      v.Name,
		PvName:    v.PvName,
		SizeGB:    v.SizeGB,
		CreatedAt: v.CreatedAt,
	}
}
