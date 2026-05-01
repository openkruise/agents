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
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
	managerutils "github.com/openkruise/agents/pkg/utils/sandbox-manager"
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
	// Get all SandboxSets from cache
	cache := sc.manager.GetInfra().GetCache()
	if cache == nil {
		return web.ApiResponse[[]*models.TemplateInfo]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Cache not available",
		}
	}

	list := &agentsv1alpha1.SandboxSetList{}
	var listOpts []ctrlclient.ListOption
	if namespace != "" {
		listOpts = append(listOpts, ctrlclient.InNamespace(namespace))
	}
	if err := cache.GetClient().List(r.Context(), list, listOpts...); err != nil {
		return web.ApiResponse[[]*models.TemplateInfo]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to list templates: %v", err),
		}
	}

	// Convert to E2B format
	e2bTemplates := make([]*models.TemplateInfo, 0, len(list.Items))
	for i := range list.Items {
		e2bTemplate := sc.convertToTemplateInfo(&list.Items[i])
		e2bTemplates = append(e2bTemplates, e2bTemplate)
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

	// Get SandboxSet from cache
	cache := sc.manager.GetInfra().GetCache()
	if cache == nil {
		return web.ApiResponse[*models.Template]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Cache not available",
		}
	}

	// Get SandboxSet from cache using informer
	template, err := cache.PickSandboxSet(r.Context(), infracache.PickSandboxSetOptions{
		Namespace: namespace,
		Name:      templateID,
	})
	if err != nil {
		return web.ApiResponse[*models.Template]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Template not found: %s", templateID),
		}
	}

	// Convert to E2B format
	e2bTemplate := sc.convertToTemplate(template)

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

	if err := sc.manager.DeleteCheckpoint(ctx, user.ID.String(), infra.DeleteCheckpointOptions{
		Namespace:    sc.getNamespaceOfUser(user),
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

// convertToTemplateInfo converts SandboxSet to E2B TemplateInfo
func (sc *Controller) convertToTemplateInfo(tmpl *agentsv1alpha1.SandboxSet) *models.TemplateInfo {
	cpuCount, memoryMB, diskSizeMB := BuildResource(tmpl)
	return &models.TemplateInfo{
		TemplateID:  tmpl.Name,
		BuildID:     tmpl.Name,
		CPUCount:    cpuCount,
		MemoryMB:    memoryMB,
		DiskSizeMB:  diskSizeMB,
		Public:      true,
		Aliases:     []string{tmpl.Name},
		Names:       []string{tmpl.Name},
		CreatedAt:   tmpl.CreationTimestamp.Time,
		UpdatedAt:   tmpl.CreationTimestamp.Time,
		CreatedBy:   nil,
		SpawnCount:  0,
		BuildCount:  1,
		EnvdVersion: "0.1.1",
		BuildStatus: buildStatus(tmpl),
	}
}

// convertToTemplate converts SandboxSet to E2B Template
func (sc *Controller) convertToTemplate(tmpl *agentsv1alpha1.SandboxSet) *models.Template {
	cpuCount, memoryMB, diskSizeMB := BuildResource(tmpl)
	// Create builds array
	builds := []models.Build{
		{
			BuildID:     tmpl.Name,
			Status:      buildStatus(tmpl),
			CreatedAt:   tmpl.CreationTimestamp.Time,
			UpdatedAt:   tmpl.CreationTimestamp.Time,
			CPUCount:    cpuCount,
			MemoryMB:    memoryMB,
			FinishedAt:  tmpl.CreationTimestamp.Time,
			DiskSizeMB:  diskSizeMB,
			EnvdVersion: "0.1.1",
		},
	}
	return &models.Template{
		TemplateID:    tmpl.Name,
		Public:        true,
		Aliases:       []string{tmpl.Name},
		Names:         []string{tmpl.Name},
		CreatedAt:     tmpl.CreationTimestamp.Time,
		UpdatedAt:     tmpl.CreationTimestamp.Time,
		LastSpawnedAt: nil,
		SpawnCount:    0,
		Builds:        builds,
	}
}

func BuildResource(tmpl *agentsv1alpha1.SandboxSet) (int, int, int) {
	cpuCount, memoryMB, diskSizeMB := 0, 0, 0
	if tmpl.Spec.Template != nil {
		resource := managerutils.CalculateResourceFromContainers(tmpl.Spec.Template.Spec.Containers)
		if resource.CPUMilli > 0 {
			cpuCount = int(resource.CPUMilli / 1000)
		}
		if resource.MemoryMB > 0 {
			memoryMB = int(resource.MemoryMB)
		}
		if resource.DiskSizeMB > 0 {
			diskSizeMB = int(resource.DiskSizeMB)
		}
		// Calculate disk size from volumeClaimTemplates
		for _, pvc := range tmpl.Spec.VolumeClaimTemplates {
			if storage := pvc.Spec.Resources.Requests.Storage(); storage != nil && !storage.IsZero() {
				diskSizeMB += int(storage.Value() / (1024 * 1024))
			}
		}
	}
	return cpuCount, memoryMB, diskSizeMB
}

func buildStatus(tmpl *agentsv1alpha1.SandboxSet) string {
	if !tmpl.DeletionTimestamp.IsZero() {
		return "deleting"
	}
	return "ready"
}
