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

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/api/v1alpha1"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// volumeManagerInterface allows mocking the volume manager in tests.
type volumeManagerInterface interface {
	RegisterVolume(ctx context.Context, opts infra.RegisterVolumeOptions) (infra.VolumeInfo, error)
	ListVolumes(ctx context.Context, opts infra.ListVolumesOptions) ([]infra.VolumeInfo, error)
	GetVolume(ctx context.Context, opts infra.GetVolumeOptions) (infra.VolumeInfo, error)
	DeleteVolume(ctx context.Context, opts infra.DeleteVolumeOptions) (infra.DeleteVolumeResult, error)
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

// RegisterVolume handles POST /volumes — registers a pre-provisioned PV as a named volume.
func (sc *Controller) RegisterVolume(r *http.Request) (web.ApiResponse[*models.VolumeResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}

	var req models.RegisterVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}

	// Validate required fields before calling infra.
	switch {
	case req.Name == "":
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "name is required",
		}
	case req.PvName == "":
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "pvName is required",
		}
	case req.SizeGB <= 0:
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "sizeGB must be a positive integer",
		}
	}

	namespace := sc.getNamespaceOfUser(user)
	log.Info("register volume request", "namespace", namespace, "pvName", req.PvName, "name", req.Name)

	info, err := sc.volumeManager.RegisterVolume(ctx, infra.RegisterVolumeOptions{
		Namespace: namespace,
		Name:      req.Name,
		PvName:    req.PvName,
		SizeGB:    req.SizeGB,
	})
	if err != nil {
		log.Error(err, "failed to register volume")
		return web.ApiResponse[*models.VolumeResponse]{}, &web.ApiError{
			Code:    mapVolumeErrorToHTTP(managererrors.GetErrCode(err)),
			Message: err.Error(),
		}
	}

	return web.ApiResponse[*models.VolumeResponse]{
		Code: http.StatusCreated,
		Body: volumeInfoToResponse(info),
	}, nil
}

// ListVolumes handles GET /volumes — lists all volumes in the caller's namespace.
func (sc *Controller) ListVolumes(r *http.Request) (web.ApiResponse[[]models.VolumeResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[[]models.VolumeResponse]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}

	namespace := sc.getNamespaceOfUser(user)
	log.Info("list volumes request", "namespace", namespace)

	volumes, err := sc.volumeManager.ListVolumes(ctx, infra.ListVolumesOptions{
		Namespace: namespace,
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
			Message: "User not found",
		}
	}

	volumeID := r.PathValue("volumeID")
	namespace := sc.getNamespaceOfUser(user)
	log.Info("get volume request", "namespace", namespace, "volumeID", volumeID)

	info, err := sc.volumeManager.GetVolume(ctx, infra.GetVolumeOptions{
		Namespace: namespace,
		VolumeID:  volumeID,
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

// DeleteVolume handles DELETE /volumes/{volumeID} — unregisters a volume.
// Accepts optional ?force=true query parameter to force deletion even when mounted.
func (sc *Controller) DeleteVolume(r *http.Request) (web.ApiResponse[*models.DeleteVolumeResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.DeleteVolumeResponse]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}

	volumeID := r.PathValue("volumeID")
	force := r.URL.Query().Get("force") == "true"
	namespace := sc.getNamespaceOfUser(user)
	log.Info("delete volume request", "namespace", namespace, "volumeID", volumeID, "force", force)

	result, err := sc.volumeManager.DeleteVolume(ctx, infra.DeleteVolumeOptions{
		Namespace: namespace,
		VolumeID:  volumeID,
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
		info, err := sc.volumeManager.GetVolume(ctx, infra.GetVolumeOptions{
			Namespace: namespace,
			VolumeID:  m.VolumeID,
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
