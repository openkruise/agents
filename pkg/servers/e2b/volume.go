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
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/klog/v2"

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
	req, parseErr := sc.parseCreateVolumeRequest(r)
	if parseErr != nil {
		return web.ApiResponse[*models.Volume]{}, parseErr
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
		WaitBoundTimeout: time.Duration(req.Extensions.WaitBoundSeconds) * time.Second,
	})
	if err != nil {
		log.Error(err, "Failed to create volume", "name", req.Name, "namespace", namespace)
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

func (sc *Controller) parseCreateVolumeRequest(r *http.Request) (models.NewVolumeRequest, *web.ApiError) {
	var request models.NewVolumeRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "invalid request body",
		}
	}

	if err := request.ParseExtensions(r.Header); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("Bad extension param: %s", err.Error()),
		}
	}

	if err := validateVolumeRequest(request); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: err.Error(),
		}
	}
	return request, nil
}

func validateVolumeRequest(request models.NewVolumeRequest) error {
	if request.Name == "" {
		return fmt.Errorf("name is required")
	}
	if request.Extensions.StorageClass == "" {
		return fmt.Errorf("storage class is required")
	}
	if request.Extensions.AccessMode == "" {
		return fmt.Errorf("access mode is required")
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
		if apierrors.IsNotFound(err) || managererrors.GetErrCode(err) == managererrors.ErrorNotFound {
			return web.ApiResponse[*models.Volume]{}, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: "Volume not found",
			}
		}
		if managererrors.GetErrCode(err) == managererrors.ErrorNotAllowed {
			return web.ApiResponse[*models.Volume]{}, &web.ApiError{
				Code:    http.StatusForbidden,
				Message: "Volume is not owned by the requesting user",
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
		if apierrors.IsNotFound(err) || managererrors.GetErrCode(err) == managererrors.ErrorNotFound {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: "Volume not found",
			}
		}
		if managererrors.GetErrCode(err) == managererrors.ErrorNotAllowed {
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusForbidden,
				Message: "Volume is not owned by the requesting user",
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
