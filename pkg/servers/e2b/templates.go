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
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	infracache "github.com/openkruise/agents/pkg/cache"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

// reservedTemplateName is kept out of the template routes: the tag-management
// routes stay unregistered, and this literal name must never enter legacy
// delete semantics, so existing routes report it as absent.
const reservedTemplateName = "tags"

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

	sandboxSets, err := cache.ListSandboxSets(r.Context(), infracache.ListSandboxSetsOptions{
		Namespace: namespace,
	})
	if err != nil {
		return web.ApiResponse[[]*models.TemplateInfo]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to list templates: %v", err),
		}
	}

	// Convert to E2B format
	e2bTemplates := make([]*models.TemplateInfo, 0, len(sandboxSets))
	for _, sandboxSet := range sandboxSets {
		e2bTemplate := sc.convertToTemplateInfo(sandboxSet)
		e2bTemplates = append(e2bTemplates, e2bTemplate)
	}
	return web.ApiResponse[[]*models.TemplateInfo]{
		Code: http.StatusOK,
		Body: e2bTemplates,
	}, nil
}

// GetTemplate returns a specific template by ID. A TemplateBuild is
// projected first; when no TemplateBuild resource exists the existing SandboxSet
// details are kept.
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

	if templateID == reservedTemplateName {
		return web.ApiResponse[*models.Template]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Template not found: %s", templateID),
		}
	}
	if namespace != "" {
		details, err := sc.manager.GetTemplate(r.Context(), namespace, templateID)
		if err == nil {
			template, apiErr := convertTemplateBuild(details)
			if apiErr != nil {
				return web.ApiResponse[*models.Template]{}, apiErr
			}
			return web.ApiResponse[*models.Template]{Code: http.StatusOK, Body: template}, nil
		}
		if errors.GetErrCode(err) != errors.ErrorNotFound {
			return web.ApiResponse[*models.Template]{}, templateBuildError(err)
		}
	}

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

// DeleteTemplate deletes a template: a TemplateBuild is removed
// synchronously after its references are checked, and when no TemplateBuild
// resource exists the existing checkpoint deletion semantics are kept.
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

	if templateID == reservedTemplateName {
		return web.ApiResponse[struct{}]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Template not found: %s", templateID),
		}
	}

	namespace := sc.getNamespaceOfUser(user)
	if namespace != "" {
		if apiErr := sc.checkTemplateReferences(ctx, namespace, templateID); apiErr != nil {
			return web.ApiResponse[struct{}]{}, apiErr
		}
		err := sc.manager.DeleteTemplate(ctx, namespace, templateID)
		if err == nil {
			log.Info("TemplateBuild deleted", "templateID", templateID)
			return web.ApiResponse[struct{}]{Code: http.StatusNoContent}, nil
		}
		if errors.GetErrCode(err) != errors.ErrorNotFound {
			return web.ApiResponse[struct{}]{}, templateBuildError(err)
		}
	}

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

// checkTemplateReferences rejects deleting a TemplateBuild name that
// SandboxSets still reference through spec.templateRef. The name is checked
// before the TemplateBuild read because a stale name would otherwise fall through
// to the legacy semantics and bypass the reference protection.
func (sc *Controller) checkTemplateReferences(ctx context.Context, namespace, name string) *web.ApiError {
	cache := sc.manager.GetInfra().GetCache()
	if cache == nil {
		return &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "Cache not available",
		}
	}
	sandboxSets, err := cache.ListSandboxSets(ctx, infracache.ListSandboxSetsOptions{Namespace: namespace, TemplateRef: name})
	if err != nil {
		return &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("Failed to list templates: %v", err),
		}
	}
	for _, sandboxSet := range sandboxSets {
		return &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("Template %q is referenced by SandboxSet %q", name, sandboxSet.Name),
		}
	}
	return nil
}

// convertTemplateBuild projects one TemplateBuild onto the community
// TemplateWithBuilds shape. Only a workload the E2B protocol can represent is
// projected: one business container with complete, equal, whole-number cpu
// and memory requests and limits; anything else reports 400 instead of a
// distorted projection.
func convertTemplateBuild(details *sandboxmanager.TemplateDetails) (*models.Template, *web.ApiError) {
	cpuCount, memoryMB, diskSizeMB, apiErr := templateBuildResources(details.Spec)
	if apiErr != nil {
		return nil, apiErr
	}
	status, _ := templateBuildStatus(details)
	createdAt := details.CreationTimestamp.Time
	updatedAt, finishedAt := createdAt, time.Time{}
	if condition := apiMeta.FindStatusCondition(details.Status.Conditions, agentsv1alpha1.SandboxTemplateBuildConditionReady); condition != nil {
		updatedAt = condition.LastTransitionTime.Time
		if details.Status.Phase == agentsv1alpha1.SandboxTemplatePhaseReady ||
			details.Status.Phase == agentsv1alpha1.SandboxTemplatePhaseFailed {
			finishedAt = condition.LastTransitionTime.Time
		}
	}
	return &models.Template{
		TemplateID:    details.Name,
		Public:        false,
		Aliases:       []string{},
		Names:         []string{details.Name},
		CreatedAt:     createdAt,
		UpdatedAt:     updatedAt,
		LastSpawnedAt: nil,
		SpawnCount:    0,
		Builds: []models.Build{{
			BuildID:     details.BuildID,
			Status:      status,
			CreatedAt:   createdAt,
			UpdatedAt:   updatedAt,
			CPUCount:    cpuCount,
			MemoryMB:    memoryMB,
			FinishedAt:  finishedAt,
			DiskSizeMB:  diskSizeMB,
			EnvdVersion: envdVersion,
		}},
	}, nil
}

func templateBuildResources(spec agentsv1alpha1.SandboxTemplateSpec) (int, int, int, *web.ApiError) {
	if spec.Template == nil || len(spec.Template.Spec.Containers) != 1 {
		return 0, 0, 0, badRequest("the workload needs exactly one business container to be represented in the E2B protocol")
	}
	resources := spec.Template.Spec.Containers[0].Resources
	unrepresentable := badRequest("cpu and memory need complete, equal, whole-number requests and limits to be represented in the E2B protocol")
	cpu := resources.Requests[corev1.ResourceCPU]
	if cpu.IsZero() || cpu.MilliValue() <= 0 || cpu.MilliValue()%1000 != 0 || !cpu.Equal(resources.Limits[corev1.ResourceCPU]) {
		return 0, 0, 0, unrepresentable
	}
	memory := resources.Requests[corev1.ResourceMemory]
	if memory.IsZero() || memory.Value() <= 0 || memory.Value()%(1024*1024) != 0 || !memory.Equal(resources.Limits[corev1.ResourceMemory]) {
		return 0, 0, 0, unrepresentable
	}
	diskSizeMB := 0
	for _, pvc := range spec.VolumeClaimTemplates {
		if storage := pvc.Spec.Resources.Requests.Storage(); storage != nil && !storage.IsZero() {
			diskSizeMB += int(storage.Value() / (1024 * 1024))
		}
	}
	return int(cpu.MilliValue() / 1000), int(memory.Value() / (1024 * 1024)), diskSizeMB, nil
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
		EnvdVersion: envdVersion,
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
			EnvdVersion: envdVersion,
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
		resource := infra.CalculateResourceFromContainers(tmpl.Spec.Template.Spec.Containers)
		cpuCount64, memoryMB64, diskSizeMB64 := e2bResource(resource)
		cpuCount = int(cpuCount64)
		memoryMB = int(memoryMB64)
		diskSizeMB = int(diskSizeMB64)
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
