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

package opensandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/opensandbox/models"
	"github.com/openkruise/agents/pkg/servers/web"
	annotationutils "github.com/openkruise/agents/pkg/utils/annotations"
	"github.com/openkruise/agents/pkg/utils/pagination"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// claimSandboxTimeout and waitReadyTimeout bound the (synchronous, on our
// backend) claim/clone that backs the spec's async POST /sandboxes; see the
// package AGENTS.md "Protocol Contract" note on why this adapter still
// returns 202 only after the operation has actually completed.
const (
	claimSandboxTimeout = 60 * time.Second
	waitReadyTimeout    = 60 * time.Second
)

func mapManagerErrorToAPIError(err error) *web.ApiError {
	switch managererrors.GetErrCode(err) {
	case managererrors.ErrorBadRequest, managererrors.ErrorNotFound:
		return &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	case managererrors.ErrorConflict:
		return &web.ApiError{Code: http.StatusConflict, Message: err.Error()}
	case managererrors.ErrorQuotaExceeded:
		return &web.ApiError{Code: http.StatusForbidden, Message: err.Error()}
	case managererrors.ErrorNotAllowed:
		return &web.ApiError{Code: http.StatusForbidden, Message: err.Error()}
	default:
		return &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
}

func parseCreateSandboxRequest(r *http.Request) (models.CreateSandboxRequest, *web.ApiError) {
	var request models.CreateSandboxRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	hasImage := request.Image != nil && request.Image.URI != ""
	hasSnapshot := request.SnapshotID != ""
	if hasImage == hasSnapshot {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "exactly one of image.uri or snapshotId is required",
		}
	}
	if request.Platform != nil && request.Platform.OS != "" && request.Platform.OS != "linux" {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("unsupported platform.os %q: only linux sandboxes are supported by this backend", request.Platform.OS),
		}
	}
	if request.Timeout != nil && *request.Timeout < models.MinTimeoutSeconds {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("timeout must be at least %d seconds", models.MinTimeoutSeconds),
		}
	}
	for k := range request.Metadata {
		if strings.HasPrefix(k, models.ReservedMetadataPrefix) {
			return request, &web.ApiError{
				Code:    http.StatusBadRequest,
				Message: fmt.Sprintf("metadata key %q uses the reserved prefix %q", k, models.ReservedMetadataPrefix),
			}
		}
	}
	return request, nil
}

// resolveTimeoutOptions converts the spec's Timeout (nil == never expire,
// seconds otherwise) into the same timeout.Options every other API surface of
// this backend persists on the Sandbox. OpenSandbox has no auto-pause concept
// distinct from expiration, so PauseTime is always left zero: a timed-out
// OpenSandbox sandbox is deleted, not paused.
func resolveTimeoutOptions(now time.Time, seconds *int) timeout.Options {
	if seconds == nil {
		return timeout.Options{}
	}
	return timeout.Options{ShutdownTime: now.Add(time.Duration(*seconds) * time.Second)}
}

func resourceListFrom(limits models.ResourceLimits) (corev1.ResourceList, error) {
	if len(limits) == 0 {
		return nil, nil
	}
	out := make(corev1.ResourceList, len(limits))
	for k, v := range limits {
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, fmt.Errorf("invalid resource quantity %q=%q: %w", k, v, err)
		}
		out[corev1.ResourceName(k)] = q
	}
	return out, nil
}

// CreateSandbox implements POST /v1/sandboxes. Exactly one of image or
// snapshotId selects the creation path:
//   - image claims a sandbox from DefaultTemplate (see Dependencies) and then
//     applies an in-place image update, reusing the same
//     SandboxManager.ClaimSandbox + InplaceUpdate capability the E2B API's
//     create-with-InplaceUpdate.Image extension already uses.
//   - snapshotId clones a sandbox from the named checkpoint, reusing
//     SandboxManager.CloneSandbox exactly as the E2B API's snapshot-restore
//     path does.
func (sc *Controller) CreateSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)
	user := userFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{Code: http.StatusUnauthorized, Message: "caller not authenticated"}
	}
	request, apiErr := parseCreateSandboxRequest(r)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}

	requestLimits, err := resourceListFrom(request.ResourceLimits)
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	requestRequests, err := resourceListFrom(request.ResourceRequests)
	if err != nil {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	}

	namespace := sc.namespaceOfUser(user)
	now := time.Now()
	modifier := func(sbx infra.Sandbox) error {
		sc.applyCreateMetadata(sbx, request, now)
		return nil
	}

	if request.SnapshotID != "" {
		log.Info("create sandbox from snapshot", "snapshotId", request.SnapshotID)
		if !sc.manager.GetInfra().HasCheckpoint(ctx, infra.HasCheckpointOptions{Namespace: namespace, CheckpointID: request.SnapshotID}) {
			return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("snapshot %s not found", request.SnapshotID)}
		}
		sbx, err := sc.manager.CloneSandbox(ctx, sandboxmanager.CloneSandboxOptions{
			Infra: infra.CloneSandboxOptions{
				Namespace:        namespace,
				User:             user.ID.String(),
				CheckPointID:     request.SnapshotID,
				CloneTimeout:     claimSandboxTimeout,
				WaitReadyTimeout: waitReadyTimeout,
				Modifier:         modifier,
			},
			Quota: user.QuotaSpec.DeepCopy(),
		})
		if err != nil {
			log.Error(err, "failed to clone sandbox from snapshot")
			return web.ApiResponse[*models.Sandbox]{}, mapManagerErrorToAPIError(err)
		}
		return web.ApiResponse[*models.Sandbox]{Code: http.StatusAccepted, Body: sc.convertToOpenSandbox(sbx)}, nil
	}

	if sc.defaultTemplate == "" {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "server misconfiguration: no default sandbox template configured for image-based creation",
		}
	}
	if !sc.manager.GetInfra().HasTemplate(ctx, infra.HasTemplateOptions{Namespace: namespace, Name: sc.defaultTemplate}) {
		return web.ApiResponse[*models.Sandbox]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: fmt.Sprintf("default sandbox template %q not found", sc.defaultTemplate),
		}
	}

	log.Info("create sandbox from image", "image", request.Image.URI, "template", sc.defaultTemplate)
	infraOpts := infra.ClaimSandboxOptions{
		Namespace:        namespace,
		Template:         sc.defaultTemplate,
		User:             user.ID.String(),
		ClaimTimeout:     claimSandboxTimeout,
		WaitReadyTimeout: waitReadyTimeout,
		CreateOnNoStock:  true,
		Modifier:         modifier,
		InplaceUpdate: &config.InplaceUpdateOptions{
			Image: request.Image.URI,
		},
	}
	if len(requestRequests) > 0 || len(requestLimits) > 0 {
		infraOpts.InplaceUpdate.Resources = &config.InplaceUpdateResourcesOptions{
			Requests: requestRequests,
			Limits:   requestLimits,
		}
	}
	// extensionSkipInitRuntime is this adapter's own vendor extension (the
	// spec explicitly reserves "extensions" for provider-specific
	// parameters): it skips the agent-runtime /init handshake, for callers
	// that only need the Kubernetes-level sandbox and drive execution through
	// their own channel.
	if request.Extensions["skipInitRuntime"] != "true" {
		infraOpts.InitRuntime = &config.InitRuntimeOptions{
			EnvVars:     request.Env,
			AccessToken: config.NewDefaultAccessToken(),
		}
	}
	sbx, err := sc.manager.ClaimSandbox(ctx, sandboxmanager.ClaimSandboxOptions{
		Infra: infraOpts,
		Quota: user.QuotaSpec.DeepCopy(),
	})
	if err != nil {
		log.Error(err, "failed to claim sandbox for image-based create")
		return web.ApiResponse[*models.Sandbox]{}, mapManagerErrorToAPIError(err)
	}
	body := sc.convertToOpenSandbox(sbx)
	body.Image = request.Image
	body.Entrypoint = request.Entrypoint
	return web.ApiResponse[*models.Sandbox]{Code: http.StatusAccepted, Body: body}, nil
}

// applyCreateMetadata writes the request's timeout and user metadata onto sbx
// before it is persisted, mirroring e2b.Controller.basicSandboxCreateModifier
// for the fields OpenSandbox actually has.
func (sc *Controller) applyCreateMetadata(sbx infra.Sandbox, request models.CreateSandboxRequest, now time.Time) {
	sbx.SetTimeout(resolveTimeoutOptions(now, request.Timeout))
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, len(request.Metadata))
	}
	maps.Copy(annotations, request.Metadata)
	sbx.SetAnnotations(annotations)
}

func (sc *Controller) getSandboxOfUser(ctx context.Context, sandboxID string) (infra.Sandbox, *web.ApiError) {
	user := userFromContext(ctx)
	if user == nil {
		return nil, &web.ApiError{Code: http.StatusUnauthorized, Message: "caller not authenticated"}
	}
	getCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	sbx, err := sc.manager.GetSandbox(getCtx, user.ID.String(), nil, infra.GetSandboxOptions{
		Namespace: sc.namespaceOfUser(user),
		SandboxID: sandboxID,
	})
	if err != nil {
		return nil, &web.ApiError{
			Code:    getSandboxErrorCode(err),
			Message: fmt.Sprintf("sandbox %s not found: %v", sandboxID, err),
		}
	}
	return sbx, nil
}

func getSandboxErrorCode(err error) int {
	if managererrors.GetErrCode(err) == managererrors.ErrorInternal {
		return http.StatusInternalServerError
	}
	return http.StatusNotFound
}

// GetSandbox implements GET /v1/sandboxes/{sandboxId}.
func (sc *Controller) GetSandbox(r *http.Request) (web.ApiResponse[*models.Sandbox], *web.ApiError) {
	sandboxID := r.PathValue("sandboxId")
	sbx, apiErr := sc.getSandboxOfUser(r.Context(), sandboxID)
	if apiErr != nil {
		return web.ApiResponse[*models.Sandbox]{}, apiErr
	}
	return web.ApiResponse[*models.Sandbox]{Body: sc.convertToOpenSandbox(sbx)}, nil
}

// DeleteSandbox implements DELETE /v1/sandboxes/{sandboxId}.
func (sc *Controller) DeleteSandbox(r *http.Request) (web.ApiResponse[struct{}], *web.ApiError) {
	ctx := r.Context()
	sandboxID := r.PathValue("sandboxId")
	user := userFromContext(ctx)
	if user == nil {
		return web.ApiResponse[struct{}]{}, &web.ApiError{Code: http.StatusUnauthorized, Message: "caller not authenticated"}
	}
	sbx, apiErr := sc.getSandboxOfUser(ctx, sandboxID)
	if apiErr != nil {
		return web.ApiResponse[struct{}]{}, apiErr
	}
	if err := sc.manager.DeleteSandbox(ctx, sandboxmanager.DeleteSandboxOptions{
		Sandbox: sbx,
		User:    user.ID.String(),
		Quota:   user.QuotaSpec.DeepCopy(),
	}); err != nil {
		return web.ApiResponse[struct{}]{}, mapManagerErrorToAPIError(err)
	}
	return web.ApiResponse[struct{}]{Code: http.StatusNoContent}, nil
}

// ListSandboxes implements GET /v1/sandboxes.
func (sc *Controller) ListSandboxes(r *http.Request) (web.ApiResponse[*models.ListSandboxesResponse], *web.ApiError) {
	ctx := r.Context()
	user := userFromContext(ctx)
	if user == nil {
		return web.ApiResponse[*models.ListSandboxesResponse]{}, &web.ApiError{Code: http.StatusUnauthorized, Message: "caller not authenticated"}
	}

	page, pageSize, apiErr := parsePagination(r)
	if apiErr != nil {
		return web.ApiResponse[*models.ListSandboxesResponse]{}, apiErr
	}
	states, metadataFilter := parseListFilters(r)

	sandboxes, _, err := sc.manager.ListSandboxes(ctx, infra.SelectSandboxesOptions{
		Namespace: sc.namespaceOfUser(user),
		User:      user.ID.String(),
	}, &pagination.Paginator[infra.Sandbox]{
		// Limit 0 returns every match; this handler applies the spec's
		// page/pageSize windowing itself below rather than the paginator's
		// cursor-token scheme, since OpenSandbox's list contract is
		// page-number based with a totalItems/totalPages count.
		Limit: 0,
		Filter: func(sbx infra.Sandbox) bool {
			if len(states) > 0 && !states[string(convertPhaseToOpenSandboxState(sbx.Phase()))] {
				return false
			}
			for k, v := range metadataFilter {
				if sbx.GetAnnotations()[k] != v {
					return false
				}
			}
			return true
		},
		GetKey:       func(sbx infra.Sandbox) string { return sbx.GetAnnotations()[agentsv1alpha1.AnnotationClaimTime] },
		GetUniqueKey: func(sbx infra.Sandbox) string { return sbx.GetSandboxID() },
	})
	if err != nil {
		return web.ApiResponse[*models.ListSandboxesResponse]{}, &web.ApiError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("failed to list sandboxes: %v", err)}
	}

	totalItems := len(sandboxes)
	start := min((page-1)*pageSize, totalItems)
	end := min(start+pageSize, totalItems)
	items := make([]*models.Sandbox, 0, end-start)
	for _, sbx := range sandboxes[start:end] {
		items = append(items, sc.convertToOpenSandbox(sbx))
	}
	totalPages := (totalItems + pageSize - 1) / pageSize

	return web.ApiResponse[*models.ListSandboxesResponse]{
		Body: &models.ListSandboxesResponse{
			Items: items,
			Pagination: models.PaginationInfo{
				Page:        page,
				PageSize:    pageSize,
				TotalItems:  totalItems,
				TotalPages:  totalPages,
				HasNextPage: end < totalItems,
			},
		},
	}, nil
}

func parsePagination(r *http.Request) (page, pageSize int, apiErr *web.ApiError) {
	page, pageSize = 1, 20
	if v := r.URL.Query().Get("page"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return 0, 0, &web.ApiError{Code: http.StatusBadRequest, Message: "page must be a positive integer"}
		}
		page = n
	}
	if v := r.URL.Query().Get("pageSize"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < models.MinListLimit || n > models.MaxListLimit {
			return 0, 0, &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("pageSize must be between %d and %d", models.MinListLimit, models.MaxListLimit)}
		}
		pageSize = n
	}
	return page, pageSize, nil
}

func parseListFilters(r *http.Request) (states map[string]bool, metadata map[string]string) {
	if values := r.URL.Query()["state"]; len(values) > 0 {
		states = make(map[string]bool, len(values))
		for _, v := range values {
			states[v] = true
		}
	}
	if raw := r.URL.Query().Get("metadata"); raw != "" {
		metadata = make(map[string]string)
		for pair := range strings.SplitSeq(raw, "&") {
			if k, v, ok := strings.Cut(pair, "="); ok {
				metadata[k] = v
			}
		}
	}
	return states, metadata
}

// convertToOpenSandbox projects an infra.Sandbox onto the OpenSandbox wire
// representation. Only user-supplied (non-reserved-prefix) annotations are
// surfaced as metadata, mirroring the E2B adapter's ValidateMetadataKey guard
// so internal bookkeeping annotations never leak to callers.
func (sc *Controller) convertToOpenSandbox(sbx infra.Sandbox) *models.Sandbox {
	annotations := sbx.GetAnnotations()
	metadata := make(map[string]string, len(annotations))
	for k, v := range annotations {
		if annotationutils.IsBlackListed(k) || strings.HasPrefix(k, models.ReservedMetadataPrefix) {
			continue
		}
		metadata[k] = v
	}

	status := models.SandboxStatus{State: convertPhaseToOpenSandboxState(sbx.Phase())}
	if _, reason := sbx.GetState(); reason != "" {
		status.Reason = reason
	}

	result := &models.Sandbox{
		ID:       sbx.GetSandboxID(),
		Status:   status,
		Metadata: metadata,
	}
	if claimTime, err := sbx.GetClaimTime(); err == nil {
		result.CreatedAt = claimTime.Format(time.RFC3339)
	}
	if shutdown := sbx.GetTimeout().ShutdownTime; !shutdown.IsZero() {
		result.ExpiresAt = shutdown.Format(time.RFC3339)
	}
	if image := sbx.GetImage(); image != "" {
		result.Image = &models.ImageSpec{URI: image}
	}
	return result
}
