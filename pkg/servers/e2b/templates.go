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
	"fmt"
	"net/http"

	"k8s.io/klog/v2"

	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// ListTemplates returns a list of all templates
func (sc *Controller) ListTemplates(r *http.Request) (web.ApiResponse[[]*models.TemplateInfo], *web.ApiError) {
	log := klog.FromContext(r.Context())
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[[]*models.TemplateInfo]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "User not found",
		}
	}
	namespace := sc.getNamespaceOfUser(user)
	log.Info("will list templates", "user", user.Name, "userID", user.ID, "namespace", namespace)

	templates, err := sc.manager.ListTemplates(r.Context(), namespace)
	if err != nil {
		if errors.GetErrCode(err) == errors.ErrorUnavailable {
			return web.ApiResponse[[]*models.TemplateInfo]{}, &web.ApiError{Code: http.StatusInternalServerError, Message: "Cache not available"}
		}
		return web.ApiResponse[[]*models.TemplateInfo]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to list templates: %v", err),
		}
	}

	// Convert to E2B format
	e2bTemplates := make([]*models.TemplateInfo, 0, len(templates))
	for _, template := range templates {
		e2bTemplates = append(e2bTemplates, convertToTemplateInfo(template))
	}
	return web.ApiResponse[[]*models.TemplateInfo]{
		Code: http.StatusOK,
		Body: e2bTemplates,
	}, nil
}

// GetTemplate returns a specific template by ID
func (sc *Controller) GetTemplate(r *http.Request) (web.ApiResponse[*models.Template], *web.ApiError) {
	log := klog.FromContext(r.Context())
	user := GetUserFromContext(r.Context())
	if user == nil {
		return web.ApiResponse[*models.Template]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "User not found",
		}
	}

	templateID := r.PathValue("templateID")
	namespace := sc.getNamespaceOfUser(user)
	log.Info("will get template", "user", user.Name, "userID", user.ID, "templateID", templateID, "namespace", namespace)

	template, err := sc.manager.GetTemplate(r.Context(), namespace, templateID)
	if err != nil {
		switch errors.GetErrCode(err) {
		case errors.ErrorUnavailable:
			return web.ApiResponse[*models.Template]{}, &web.ApiError{Code: http.StatusInternalServerError, Message: "Cache not available"}
		case errors.ErrorNotFound:
			return web.ApiResponse[*models.Template]{}, &web.ApiError{
				Code:    http.StatusNotFound,
				Message: fmt.Sprintf("Template not found: %s", templateID),
			}
		default:
			log.Error(err, "failed to get template", "templateID", templateID)
			return web.ApiResponse[*models.Template]{}, &web.ApiError{Code: http.StatusInternalServerError, Message: "Failed to get template"}
		}
	}

	// Convert to E2B format
	e2bTemplate := convertToTemplate(template)

	return web.ApiResponse[*models.Template]{
		Code: http.StatusOK,
		Body: e2bTemplate,
	}, nil
}

// DeleteTemplate deletes a template (checkpoint and its associated sandbox template)
func (sc *Controller) DeleteTemplate(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	templateID := r.PathValue("templateID")
	ctx := r.Context()
	log := klog.FromContext(ctx)
	log.Info("delete template request received", "templateID", templateID)

	user := GetUserFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "User not found",
		}
	}

	namespace := sc.getNamespaceOfUser(user)
	if sc.manager.GetInfra().HasTemplate(ctx, infra.HasTemplateOptions{
		Namespace: namespace,
		Name:      templateID,
	}) {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusUnauthorized,
			Message: "Deleting SandboxSet-backed templates through the E2B API is not supported",
		}
	}

	if err := sc.manager.DeleteCheckpoint(ctx, user.ID.String(), infra.DeleteCheckpointOptions{
		Namespace:    namespace,
		CheckpointID: templateID,
	}); err != nil {
		log.Error(err, "failed to delete template", "templateID", templateID)
		switch errors.GetErrCode(err) {
		case errors.ErrorNotFound:
			fallthrough
		case errors.ErrorNotAllowed:
			// Return 204 No Content as success for not found or not allowed errors
			return web.ApiResponse[struct{}]{
				Code: http.StatusNoContent,
			}, nil
		default:
			return web.ApiResponse[struct{}]{}, &web.ApiError{
				Code:    http.StatusInternalServerError,
				Message: "Failed to delete template",
			}
		}
	}

	log.Info("template deleted", "templateID", templateID)
	return web.ApiResponse[struct{}]{
		Code: http.StatusNoContent,
	}, nil
}

// convertToTemplateInfo converts a neutral template view to E2B TemplateInfo.
func convertToTemplateInfo(tmpl infra.TemplateInfo) *models.TemplateInfo {
	cpuCount, memoryMB, diskSizeMB := e2bTemplateResource(tmpl)
	return &models.TemplateInfo{
		TemplateID:  tmpl.Name,
		BuildID:     tmpl.Name,
		CPUCount:    cpuCount,
		MemoryMB:    memoryMB,
		DiskSizeMB:  diskSizeMB,
		Public:      true,
		Aliases:     []string{tmpl.Name},
		Names:       []string{tmpl.Name},
		CreatedAt:   tmpl.CreatedAt,
		UpdatedAt:   tmpl.CreatedAt,
		CreatedBy:   nil,
		SpawnCount:  0,
		BuildCount:  1,
		EnvdVersion: "0.1.1",
		BuildStatus: templateBuildStatus(tmpl),
	}
}

// convertToTemplate converts a neutral template view to an E2B Template.
func convertToTemplate(tmpl infra.TemplateInfo) *models.Template {
	cpuCount, memoryMB, diskSizeMB := e2bTemplateResource(tmpl)
	// Create builds array
	builds := []models.Build{
		{
			BuildID:     tmpl.Name,
			Status:      templateBuildStatus(tmpl),
			CreatedAt:   tmpl.CreatedAt,
			UpdatedAt:   tmpl.CreatedAt,
			CPUCount:    cpuCount,
			MemoryMB:    memoryMB,
			FinishedAt:  tmpl.CreatedAt,
			DiskSizeMB:  diskSizeMB,
			EnvdVersion: "0.1.1",
		},
	}
	return &models.Template{
		TemplateID:    tmpl.Name,
		Public:        true,
		Aliases:       []string{tmpl.Name},
		Names:         []string{tmpl.Name},
		CreatedAt:     tmpl.CreatedAt,
		UpdatedAt:     tmpl.CreatedAt,
		LastSpawnedAt: nil,
		SpawnCount:    0,
		Builds:        builds,
	}
}

// e2bTemplateResource renders the neutral template footprint in the E2B units.
func e2bTemplateResource(tmpl infra.TemplateInfo) (int, int, int) {
	return int(e2bCPUCount(tmpl.CPUMilli)), int(tmpl.MemoryMB), int(tmpl.DiskSizeMB)
}

func templateBuildStatus(tmpl infra.TemplateInfo) string {
	if tmpl.Terminating {
		return "deleting"
	}
	return "ready"
}
