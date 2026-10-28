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
	"math"
	"net/http"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiMeta "k8s.io/apimachinery/pkg/api/meta"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	"github.com/openkruise/agents/pkg/servers/web"
)

const (
	defaultTemplateTag = "default"
	envdVersion        = "0.1.1"
	msgUserNotFound    = "User not found"
)

// templateBuildError maps Manager build errors onto the E2B status codes:
// 400 invalid input, 404 nothing accessible in the caller's namespace, 409
// identity conflicts, 501 an unconfigured backend, 503 unavailable storage.
func templateBuildError(err error) *web.ApiError {
	apiErr := &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
	switch managererrors.GetErrCode(err) {
	case managererrors.ErrorBadRequest:
		apiErr.Code = http.StatusBadRequest
	case managererrors.ErrorNotFound:
		apiErr.Code = http.StatusNotFound
	case managererrors.ErrorConflict:
		apiErr.Code = http.StatusConflict
	case managererrors.ErrorNotImplemented:
		apiErr.Code = http.StatusNotImplemented
	case managererrors.ErrorUnavailable:
		apiErr.Code = http.StatusServiceUnavailable
	}
	return apiErr
}

// templateBuildHeaders interprets the private build headers of the reserve,
// submit, and status routes. Every x-e2b-kruise- header other than the
// backend selector is rejected: the companion SandboxSet is not delivered
// in Phase 1, so its headers are rejected instead of silently ignored, and
// extensions the protocol does not define are rejected as well. The backend
// selector accepts only k8s, the one backend this platform serves.
func templateBuildHeaders(r *http.Request) *web.ApiError {
	for key := range r.Header {
		if strings.HasPrefix(key, "X-E2b-Kruise-") && key != http.CanonicalHeaderKey("x-e2b-kruise-infra") {
			return badRequest("header %q is not supported", key)
		}
	}
	backends := r.Header.Values("x-e2b-kruise-infra")
	switch {
	case len(backends) == 0:
		return nil
	case len(backends) > 1:
		return badRequest("header %q must not be repeated", "x-e2b-kruise-infra")
	case backends[0] == agentsv1alpha1.SandboxTemplateBackendKubernetes:
		return nil
	default:
		return badRequest("header %q accepts only k8s", "x-e2b-kruise-infra")
	}
}

// templateBuildName resolves an E2B template name to the TemplateBuild
// name: only the implicit default tag is accepted.
func templateBuildName(name string) (string, *web.ApiError) {
	base, tag, _ := strings.Cut(name, ":")
	if tag != "" && tag != defaultTemplateTag {
		return "", badRequest("name %q carries unsupported tag %q", name, tag)
	}
	return base, nil
}

func (sc *Controller) templateBuildNamespace(r *http.Request) (string, *web.ApiError) {
	user := GetUserFromContext(r.Context())
	if user == nil {
		return "", &web.ApiError{Code: http.StatusInternalServerError, Message: msgUserNotFound}
	}
	return sc.getNamespaceOfUser(user), nil
}

// checkTemplateNameOccupancy rejects a build name that an existing
// SandboxSet or Checkpoint already occupies. Instance creation resolves
// templateIDs through exactly these probes, so a TemplateBuild of the
// same name would answer template queries while sandboxes keep resolving to
// the old resource.
func (sc *Controller) checkTemplateNameOccupancy(ctx context.Context, namespace, name string) *web.ApiError {
	if sc.manager.GetInfra().HasTemplate(ctx, infra.HasTemplateOptions{Namespace: namespace, Name: name}) {
		return &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("Template name %q is occupied by an existing SandboxSet", name),
		}
	}
	if sc.manager.GetInfra().HasCheckpoint(ctx, infra.HasCheckpointOptions{Namespace: namespace, CheckpointID: name}) {
		return &web.ApiError{
			Code:    http.StatusConflict,
			Message: fmt.Sprintf("Template name %q is occupied by an existing Checkpoint", name),
		}
	}
	return nil
}

// ReserveTemplateBuild binds a template name to one build: it creates the
// reserved draft SandboxTemplate whose uid is the Build identity.
func (sc *Controller) ReserveTemplateBuild(r *http.Request) (web.ApiResponse[*models.TemplateRequestResponseV3], *web.ApiError) {
	namespace, apiErr := sc.templateBuildNamespace(r)
	if apiErr != nil {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, apiErr
	}
	if apiErr := templateBuildHeaders(r); apiErr != nil {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, apiErr
	}

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request models.TemplateBuildRequestV3
	if err := decoder.Decode(&request); err != nil {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, badRequest("invalid request body: %v", err)
	}
	name, apiErr := templateBuildName(request.Name)
	if apiErr != nil {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, apiErr
	}
	if apiErr := sc.checkTemplateNameOccupancy(r.Context(), namespace, name); apiErr != nil {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, apiErr
	}
	if len(request.Tags) > 1 || (len(request.Tags) == 1 && request.Tags[0] != defaultTemplateTag) {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, badRequest("tags accept only the default tag")
	}
	if request.Alias != "" {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, badRequest("alias is not supported")
	}
	if request.TeamID != "" {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, badRequest("teamID is not supported")
	}
	if request.MinFreeDiskMB != nil {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, badRequest("minFreeDiskMb is not supported")
	}
	// Omitted resource parameters default to the E2B baseline; an explicit
	// value must be a positive count that survives the int32 conversion.
	cpuCount, memoryMB := 1, 1024
	if request.CPUCount != nil {
		if *request.CPUCount <= 0 || int64(*request.CPUCount) > math.MaxInt32 {
			return web.ApiResponse[*models.TemplateRequestResponseV3]{}, badRequest("cpuCount must be a positive integer")
		}
		cpuCount = *request.CPUCount
	}
	if request.MemoryMB != nil {
		if *request.MemoryMB <= 0 || int64(*request.MemoryMB) > math.MaxInt32 {
			return web.ApiResponse[*models.TemplateRequestResponseV3]{}, badRequest("memoryMB must be a positive integer")
		}
		memoryMB = *request.MemoryMB
	}

	details, err := sc.manager.ReserveTemplate(r.Context(), sandboxmanager.ReserveTemplateOptions{
		Namespace: namespace,
		Name:      name,
		Backend:   agentsv1alpha1.SandboxTemplateBackendKubernetes,
		CPUCount:  int32(cpuCount),
		MemoryMB:  int32(memoryMB),
	})
	if err != nil {
		return web.ApiResponse[*models.TemplateRequestResponseV3]{}, templateBuildError(err)
	}
	return web.ApiResponse[*models.TemplateRequestResponseV3]{
		Code: http.StatusAccepted,
		Body: &models.TemplateRequestResponseV3{
			TemplateID: name,
			BuildID:    details.BuildID,
			Public:     false,
			Names:      []string{name},
			Tags:       []string{defaultTemplateTag},
			Aliases:    []string{},
		},
	}, nil
}

// SubmitTemplateBuild completes one reserved draft with the converted build
// input. The response carries no build record: the status route reports the
// converged phase.
func (sc *Controller) SubmitTemplateBuild(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	namespace, apiErr := sc.templateBuildNamespace(r)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	if apiErr := templateBuildHeaders(r); apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request models.TemplateBuildStartV2
	if err := decoder.Decode(&request); err != nil {
		return web.ApiResponse[struct{}]{}, badRequest("invalid request body: %v", err)
	}
	converted, apiErr := convertBuildRecipe(request)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}

	if _, err := sc.manager.SubmitTemplate(r.Context(), sandboxmanager.SubmitTemplateOptions{
		Namespace: namespace,
		Name:      r.PathValue("templateID"),
		BuildID:   r.PathValue("buildID"),
		Spec:      submittedBuildSpec(converted),
	}); err != nil {
		return web.ApiResponse[struct{}]{}, templateBuildError(err)
	}
	return web.ApiResponse[struct{}]{Code: http.StatusAccepted}, nil
}

// submittedBuildSpec assembles the SandboxTemplateSpec the E2B entry submits.
// Both bases declare the envd runtime and the build capture scope — memory is
// added when lifecycle commands must be restored — and a fromImage build
// carries a single runtime container. The Manager merges a fromTemplate base
// and applies the reserved resource envelope.
func submittedBuildSpec(converted *convertedBuild) agentsv1alpha1.SandboxTemplateSpec {
	contents := []string{agentsv1alpha1.PersistentContentFilesystem}
	if converted.BuildOptions.StartCommand != nil || converted.BuildOptions.ReadyCommand != nil {
		contents = append(contents, agentsv1alpha1.PersistentContentMemory)
	}
	spec := agentsv1alpha1.SandboxTemplateSpec{
		Backend:            agentsv1alpha1.SandboxTemplateBackendKubernetes,
		PersistentContents: contents,
		Runtimes:           []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}},
		BuildOptions:       &converted.BuildOptions,
	}
	if converted.Image != "" {
		spec.Template = &corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name:  "runtime",
			Image: converted.Image,
		}}}}
	}
	return spec
}

// GetTemplateBuildStatus reports the status of one bound Build. Phase 1
// executes no build commands and persists no logs, so logEntries stay empty
// while the query validation keeps the wire contract.
func (sc *Controller) GetTemplateBuildStatus(r *http.Request) (web.ApiResponse[*models.TemplateBuildInfo], *web.ApiError) {
	namespace, apiErr := sc.templateBuildNamespace(r)
	if apiErr != nil {
		return web.ApiResponse[*models.TemplateBuildInfo]{}, apiErr
	}
	if apiErr := templateBuildHeaders(r); apiErr != nil {
		return web.ApiResponse[*models.TemplateBuildInfo]{}, apiErr
	}
	if apiErr := templateLogQuery(r); apiErr != nil {
		return web.ApiResponse[*models.TemplateBuildInfo]{}, apiErr
	}

	templateID := r.PathValue("templateID")
	details, err := sc.manager.GetTemplate(r.Context(), namespace, templateID)
	if err != nil {
		return web.ApiResponse[*models.TemplateBuildInfo]{}, templateBuildError(err)
	}
	buildID := r.PathValue("buildID")
	if details.BuildID != buildID {
		return web.ApiResponse[*models.TemplateBuildInfo]{}, &web.ApiError{
			Code:    http.StatusNotFound,
			Message: fmt.Sprintf("Build %q of template %q not found", buildID, templateID),
		}
	}
	status, reason := templateBuildStatus(details)
	return web.ApiResponse[*models.TemplateBuildInfo]{
		Code: http.StatusOK,
		Body: &models.TemplateBuildInfo{
			Logs:       []string{},
			LogEntries: []models.TemplateBuildLogEntry{},
			TemplateID: templateID,
			BuildID:    buildID,
			Status:     status,
			Reason:     reason,
		},
	}, nil
}

// templateBuildStatus projects the converged Template phase onto the E2B
// build-status vocabulary; an unset or Pending draft is still waiting for its
// submission.
func templateBuildStatus(details *sandboxmanager.TemplateDetails) (string, *models.TemplateBuildFailReason) {
	switch details.Status.Phase {
	case agentsv1alpha1.SandboxTemplatePhaseReady:
		return "ready", nil
	case agentsv1alpha1.SandboxTemplatePhaseFailed:
		reason := &models.TemplateBuildFailReason{Message: "build failed"}
		if condition := apiMeta.FindStatusCondition(details.Status.Conditions, agentsv1alpha1.SandboxTemplateBuildConditionReady); condition != nil && condition.Message != "" {
			reason.Message = condition.Message
		}
		return "error", reason
	case agentsv1alpha1.SandboxTemplatePhaseBuilding:
		return "building", nil
	default:
		return "waiting", nil
	}
}

// templateLogQuery validates the log-draining query of the status route.
func templateLogQuery(r *http.Request) *web.ApiError {
	query := r.URL.Query()
	if query.Has("level") {
		return badRequest("level filtering is not supported")
	}
	if raw := query.Get("logsOffset"); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil || offset < 0 {
			return badRequest("logsOffset must be a non-negative integer")
		}
	}
	if raw := query.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > 100 {
			return badRequest("limit must be between 1 and 100")
		}
	}
	return nil
}

// GetTemplateAlias answers the existence probe of Template.exists: the name
// resolves within the caller's namespace and a bound name reports 200
// regardless of its phase, because exists being true does not mean Ready.
func (sc *Controller) GetTemplateAlias(r *http.Request) (web.ApiResponse[*models.TemplateAliasResponse], *web.ApiError) {
	namespace, apiErr := sc.templateBuildNamespace(r)
	if apiErr != nil {
		return web.ApiResponse[*models.TemplateAliasResponse]{}, apiErr
	}
	name, apiErr := templateBuildName(r.PathValue("alias"))
	if apiErr != nil {
		return web.ApiResponse[*models.TemplateAliasResponse]{}, apiErr
	}
	if _, err := sc.manager.GetTemplate(r.Context(), namespace, name); err != nil {
		return web.ApiResponse[*models.TemplateAliasResponse]{}, templateBuildError(err)
	}
	return web.ApiResponse[*models.TemplateAliasResponse]{
		Code: http.StatusOK,
		Body: &models.TemplateAliasResponse{TemplateID: name, Public: false},
	}, nil
}
