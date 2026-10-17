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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
	servervalidation "github.com/openkruise/agents/pkg/servers/validation"
	"github.com/openkruise/agents/pkg/servers/web"
	annotationutils "github.com/openkruise/agents/pkg/utils/annotations"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// noServerTimeout bounds the claim/wait-ready phases when the operator has not
// configured a tighter deadline. It is a far-future duration rather than a
// true infinity so existing timeout handling (context deadlines, retry step
// counts, wait-ready polling) keeps working unchanged. The value and rationale
// mirror the E2B layer's noServerTimeout.
const noServerTimeout = 100 * 365 * 24 * time.Hour

const (
	storedExtensionPrefix     = "opensandbox.extensions."
	extensionAnnotationPrefix = "opensandbox.io/extensions."
)

// CreateSandbox handles `POST /v1/sandboxes`. It translates the OpenSandbox
// create request into an agents claim or clone, delegates to SandboxManager, and
// translates the claimed sandbox back into the OpenSandbox response shape.
//
// Image requests use a fresh workload configuration. Warm-template preparation
// and reuse remain separate from this cold path; no static image alias is needed.
//
// The handler deliberately does not reuse the E2B CreateSandbox code path:
// the two protocols have different request/response shapes, different status
// codes (201 vs 202). Native metadata parsing and shared API validation
// are reused within the API layer; Manager receives neutral options.
func (s *Server) CreateSandbox(r *http.Request) (web.ApiResponse[CreateSandboxResponse], *web.ApiError) {
	ctx := r.Context()
	log := klog.FromContext(ctx)

	user := GetUserFromContext(ctx)
	if user == nil {
		// CheckApiKey should have rejected the request before reaching here.
		// Returning 500 (not 401) makes the missing-middleware bug visible
		// instead of masquerading as an auth failure.
		return web.ApiResponse[CreateSandboxResponse]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: "user not found in request context",
		}
	}

	request, apiErr := parseCreateSandboxRequest(r, s.maxTimeout)
	if apiErr != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, apiErr
	}
	if apiErr := validateCreateExecution(request); apiErr != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, apiErr
	}
	volumes, apiErr := resolveExistingVolumes(request.Volumes)
	if apiErr != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, apiErr
	}
	egress, apiErr := resolveEgressPolicy(request.NetworkPolicy)
	if apiErr != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, apiErr
	}
	namespace := NamespaceOfUser(user)
	log.Info("opensandbox create request received", "source", request.source, "namespace", namespace)
	if request.source == createSourceSnapshot {
		return s.createSandboxFromCheckpoint(ctx, request, user, volumes, egress)
	}
	opts, apiErr := createClaimOptions(request, user, volumes)
	if apiErr != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, apiErr
	}
	if request.source == createSourcePool {
		opts.Template = strings.TrimSpace(request.Extensions["poolRef"])
	} else {
		cold := &infra.ColdStartOptions{
			Image: request.Image.URI, Command: slices.Clone(request.Entrypoint),
			EnvVars: maps.Clone(request.Env), Volumes: volumes,
		}
		if opts.InplaceUpdate != nil {
			if opts.InplaceUpdate.Image != "" {
				cold.Image = opts.InplaceUpdate.Image
			}
			if opts.InplaceUpdate.Resources != nil {
				cold.ResourceLimits = resourceStrings(opts.InplaceUpdate.Resources.Limits)
				cold.ResourceRequests = resourceStrings(opts.InplaceUpdate.Resources.Requests)
			}
		}
		// Preserve defaults only for the extra image-only cold source.
		if len(cold.ResourceRequests) == 0 {
			cold.ResourceRequests = maps.Clone(cold.ResourceLimits)
		}
		if request.Platform != nil {
			cold.OS, cold.Architecture = request.Platform.OS, request.Platform.Arch
		}
		opts.Volumes, opts.InplaceUpdate = nil, nil
		opts.ColdStart = cold
		opts.RuntimeConfig = []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}}
		if egress != nil {
			opts.RuntimeConfig = append(opts.RuntimeConfig, agentsv1alpha1.RuntimeConfig{Name: agentsv1alpha1.RuntimeConfigForInjectTrafficProxy})
		}
	}
	sbx, err := s.manager.ClaimSandbox(ctx, sandboxmanager.ClaimSandboxOptions{Infra: opts, Quota: user.QuotaSpec.DeepCopy()})
	if err != nil {
		log.Error(err, "opensandbox sandbox creation failed", "source", request.source)
		return web.ApiResponse[CreateSandboxResponse]{}, mapInfraErrorToApiError(err)
	}
	return s.finishCreate(ctx, sbx, request, user, egress)
}

func createClaimOptions(request CreateSandboxRequest, user *models.CreatedTeamAPIKey, volumes []infra.ExistingVolumeMount) (infra.ClaimSandboxOptions, *web.ApiError) {
	ext := request.native.Extensions
	if ext.Name != "" || ext.GenerateName != "" {
		return infra.ClaimSandboxOptions{}, &web.ApiError{Code: http.StatusBadRequest, Message: "sandbox-name and sandbox-generate-name are only supported for clone"}
	}
	update, apiErr := createInplaceUpdate(request)
	if apiErr != nil {
		return infra.ClaimSandboxOptions{}, apiErr
	}
	csi, apiErr := createCSIMountOptions(request, volumes)
	if apiErr != nil {
		return infra.ClaimSandboxOptions{}, apiErr
	}
	opts := infra.ClaimSandboxOptions{
		Namespace: NamespaceOfUser(user), User: user.ID.String(), Volumes: volumes, CSIMount: csi,
		ClaimTimeout: resolveServerTimeout(ext.TimeoutSeconds), WaitReadyTimeout: resolveServerTimeout(ext.WaitReadySeconds),
		CreateOnNoStock: ext.CreateOnNoStock, ReserveFailedSandboxFor: ext.ReserveFailedSandboxFor,
		InplaceUpdate: update, UserMetadataKeys: &agentsv1alpha1.UpdatedMetadataInClaim{
			Labels: slices.Sorted(maps.Keys(ext.Labels)), Annotations: slices.Sorted(maps.Keys(createAnnotations(request))),
		},
		Modifier: func(sbx infra.Sandbox) error { applyCreateModifier(sbx, request); return nil },
	}
	if !ext.SkipInitRuntime {
		opts.InitRuntime = &config.InitRuntimeOptions{EnvVars: maps.Clone(request.Env), AccessToken: config.NewDefaultAccessToken()}
	}
	if opts.InitRuntime == nil && (csi != nil || len(volumes) != 0) {
		return infra.ClaimSandboxOptions{}, &web.ApiError{Code: http.StatusBadRequest, Message: "init runtime is required when csi mount is specified"}
	}
	return opts, nil
}

func (s *Server) createSandboxFromCheckpoint(ctx context.Context, request CreateSandboxRequest, user *models.CreatedTeamAPIKey, volumes []infra.ExistingVolumeMount, egress *infra.SandboxNetworkConfig) (web.ApiResponse[CreateSandboxResponse], *web.ApiError) {
	// Clone accepts env but inherits the checkpoint runtime and workload.
	update, apiErr := createInplaceUpdate(request)
	if apiErr != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, apiErr
	}
	if update != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, &web.ApiError{Code: http.StatusBadRequest, Message: "InplaceUpdate is not supported for clone"}
	}
	csi, apiErr := createCSIMountOptions(request, volumes)
	if apiErr != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, apiErr
	}
	ext := request.native.Extensions
	sbx, err := s.manager.CloneSandbox(ctx, sandboxmanager.CloneSandboxOptions{
		Infra: infra.CloneSandboxOptions{
			Namespace: NamespaceOfUser(user), User: user.ID.String(), CheckPointID: request.SnapshotID,
			Volumes: volumes, CSIMount: csi, Name: ext.Name, GenerateName: ext.GenerateName,
			CloneTimeout: resolveServerTimeout(ext.TimeoutSeconds), WaitReadyTimeout: resolveServerTimeout(ext.WaitReadySeconds),
			ReserveFailedSandboxFor: ext.ReserveFailedSandboxFor,
			Modifier:                func(sbx infra.Sandbox) error { applyCreateModifier(sbx, request); return nil },
		}, Quota: user.QuotaSpec.DeepCopy(),
	})
	if err != nil {
		return web.ApiResponse[CreateSandboxResponse]{}, mapInfraErrorToApiError(err)
	}
	return s.finishCreate(ctx, sbx, request, user, egress)
}

func (s *Server) finishCreate(ctx context.Context, sbx infra.Sandbox, request CreateSandboxRequest, user *models.CreatedTeamAPIKey, egress *infra.SandboxNetworkConfig) (web.ApiResponse[CreateSandboxResponse], *web.ApiError) {
	if egress != nil {
		if err := s.manager.CreateSandboxNetworkPolicy(ctx, sandboxmanager.DeleteSandboxOptions{Sandbox: sbx, User: user.ID.String(), Quota: user.QuotaSpec.DeepCopy()}, *egress); err != nil {
			return web.ApiResponse[CreateSandboxResponse]{}, mapInfraErrorToApiError(err)
		}
	}
	return web.ApiResponse[CreateSandboxResponse]{Code: http.StatusAccepted, Body: convertToOpenSandboxResponse(sbx, request)}, nil
}

func resolveServerTimeout(seconds int) time.Duration {
	if seconds <= 0 {
		return noServerTimeout
	}
	return time.Duration(seconds) * time.Second
}

func resourceStrings(values corev1.ResourceList) map[string]string {
	if len(values) == 0 {
		return nil
	}
	result := make(map[string]string, len(values))
	for name, quantity := range values {
		result[string(name)] = quantity.String()
	}
	return result
}

func createInplaceUpdate(request CreateSandboxRequest) (*config.InplaceUpdateOptions, *web.ApiError) {
	var requests, limits map[string]string
	if request.ResourceRequests != nil {
		requests = maps.Clone(*request.ResourceRequests)
	}
	if request.ResourceLimits != nil {
		limits = maps.Clone(*request.ResourceLimits)
	}
	image := ""
	if request.Image != nil {
		image = request.Image.URI
	}
	ext := request.native.Extensions.InplaceUpdate
	if ext.Image != "" {
		image = ext.Image
	}
	if ext.Resources != nil {
		if len(ext.Resources.Requests) != 0 {
			if requests == nil {
				requests = map[string]string{}
			}
			maps.Copy(requests, resourceStrings(ext.Resources.Requests))
		}
		if len(ext.Resources.Limits) != 0 {
			if limits == nil {
				limits = map[string]string{}
			}
			maps.Copy(limits, resourceStrings(ext.Resources.Limits))
		}
	}
	return claimInplaceUpdate(image, requests, limits)
}

func createCSIMountOptions(request CreateSandboxRequest, volumes []infra.ExistingVolumeMount) (*config.CSIMountOptions, *web.ApiError) {
	mounts := request.native.Extensions.CSIMount.MountConfigs
	if len(mounts) == 0 {
		return nil, nil
	}
	if len(volumes) != 0 {
		return nil, &web.ApiError{Code: http.StatusBadRequest, Message: "volumes and native CSI metadata cannot be combined"}
	}
	raw, err := json.Marshal(mounts)
	if err != nil {
		return nil, &web.ApiError{Code: http.StatusInternalServerError, Message: fmt.Sprintf("encode CSI mount configuration: %v", err)}
	}
	return &config.CSIMountOptions{MountOptionListRaw: string(raw), Concurrency: config.DefaultCSIMountConcurrency}, nil
}

// claimInplaceUpdate translates allocation overrides into native claim updates.
// Empty overrides retain the existing configuration. Pool claims preserve
// native best-effort updates.
func claimInplaceUpdate(image string, requests, limits map[string]string) (*config.InplaceUpdateOptions, *web.ApiError) {
	update := &config.InplaceUpdateOptions{}
	if strings.TrimSpace(image) != "" {
		update.Image = image
	}
	if len(requests) != 0 || len(limits) != 0 {
		parse := func(values map[string]string, field string) (corev1.ResourceList, *web.ApiError) {
			if len(values) == 0 {
				return nil, nil
			}
			result := make(corev1.ResourceList, len(values))
			for _, name := range slices.Sorted(maps.Keys(values)) {
				if name != "cpu" && name != "memory" {
					return nil, &web.ApiError{Code: http.StatusBadRequest, Message: "extended " + field + " execution is not implemented"}
				}
				quantity, err := resource.ParseQuantity(values[name])
				if err != nil {
					return nil, &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("invalid %s quantity for %s: %v", field, name, err)}
				}
				result[corev1.ResourceName(name)] = quantity
			}
			return result, nil
		}
		parsedRequests, apiErr := parse(requests, "resourceRequests")
		if apiErr != nil {
			return nil, apiErr
		}
		parsedLimits, apiErr := parse(limits, "resourceLimits")
		if apiErr != nil {
			return nil, apiErr
		}
		update.Resources = &config.InplaceUpdateResourcesOptions{
			Requests: parsedRequests, Limits: parsedLimits,
		}
	}
	if update.Image == "" && update.Resources == nil {
		return nil, nil
	}
	return update, nil
}

// parseCreateSandboxRequest decodes and validates the OpenSandbox create
// request body with the same JSON decoding and timeout rules as E2B create.
// Only source selection is specific to this protocol; backend validation
// (source existence, quota) remains with the manager.
func parseCreateSandboxRequest(r *http.Request, maxTimeout int) (CreateSandboxRequest, *web.ApiError) {
	var request CreateSandboxRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&request); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("invalid request body: %v", err),
		}
	}

	if err := resolveCreateSource(&request); err != nil {
		return request, &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	if request.Timeout == nil || *request.Timeout == 0 {
		seconds := int64(models.DefaultTimeoutSeconds)
		request.Timeout = &seconds
	}
	if *request.Timeout < models.DefaultMinTimeoutSeconds || *request.Timeout > int64(maxTimeout) {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("timeout should between %d and %d", models.DefaultMinTimeoutSeconds, maxTimeout),
		}
	}

	request.native = models.NewSandboxRequest{Metadata: maps.Clone(request.Metadata)}
	if err := request.native.ParseExtensions(); err != nil {
		return request, &web.ApiError{Code: http.StatusBadRequest, Message: fmt.Sprintf("Bad extension param: %v", err)}
	}
	var err error
	request.native.SecurityRulesJSON, err = servervalidation.NormalizeSecurityRules(request.native.Extensions.SecurityRules)
	if err != nil {
		return request, &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	}
	request.Metadata = request.native.Metadata
	// Only annotations left after native extension parsing reach this check.
	for k := range request.Metadata {
		if errLists := validation.IsQualifiedName(k); len(errLists) > 0 {
			return request, &web.ApiError{
				Code: http.StatusBadRequest,
				Message: fmt.Sprintf("unqualified metadata key [%s]: %s",
					k, strings.Join(errLists, ", ")),
			}
		}
		if annotationutils.IsBlackListed(k) {
			return request, &web.ApiError{
				Code: http.StatusBadRequest,
				Message: fmt.Sprintf("Forbidden metadata key [%s]: cannot contain prefixes: %v",
					k, annotationutils.BlackListPrefix),
			}
		}
	}

	return request, nil
}

// applyCreateModifier writes the lifetime and metadata onto the claimed sandbox
// before persistence. Startup configuration is applied by the infra backend.
func applyCreateModifier(sbx infra.Sandbox, request CreateSandboxRequest) {
	opts := timeout.Options{}
	if !request.native.Extensions.NeverTimeout && request.Timeout != nil {
		opts.ShutdownTime = timeout.NormalizeTime(time.Now().Add(time.Duration(*request.Timeout) * time.Second))
	}
	sbx.SetTimeout(opts)
	annotations := maps.Clone(sbx.GetAnnotations())
	if annotations == nil {
		annotations = map[string]string{}
	}
	retention := request.native.Extensions.ReservePausedSandboxDuration
	if retention == "" {
		retention = timeout.ReservePausedSandboxDurationForeverValue
	}
	annotations[agentsv1alpha1.AnnotationReservePausedSandboxDuration] = retention
	maps.Copy(annotations, createAnnotations(request))
	if request.native.SecurityRulesJSON != "" {
		annotations[agentsv1alpha1.AnnotationSecurityRules] = request.native.SecurityRulesJSON
	} else {
		delete(annotations, agentsv1alpha1.AnnotationSecurityRules)
	}
	if request.native.Extensions.ReturnPodIP {
		annotations[models.ExtensionKeyReturnPodIP] = agentsv1alpha1.True
	}
	sbx.SetAnnotations(annotations)
	// Match the native default for a fresh delivery, including missed recycle.
	sbx.ClearWakeOnIngressTraffic()
	labels := maps.Clone(sbx.GetLabels())
	if labels == nil {
		labels = map[string]string{}
	}
	maps.Copy(labels, request.native.Extensions.Labels)
	internet := allowInternetAccess(request.NetworkPolicy)
	labels[agentsv1alpha1.LabelAllowInternetAccess] = internet
	sbx.SetLabels(labels)
	podLabels := maps.Clone(request.native.Extensions.Labels)
	if podLabels == nil {
		podLabels = map[string]string{}
	}
	podLabels[agentsv1alpha1.LabelAllowInternetAccess] = internet
	infra.MergePodLabels(sbx, podLabels)
}

// createAnnotations follows the reference extension codec while keeping
// protocol extension encoding out of Manager and Infra. Extension values win
// over conflicting metadata, as in the reference backend.
func createAnnotations(request CreateSandboxRequest) map[string]string {
	annotations := maps.Clone(request.Metadata)
	for key, value := range request.Extensions {
		if strings.HasPrefix(key, storedExtensionPrefix) {
			if annotations == nil {
				annotations = map[string]string{}
			}
			annotations[extensionAnnotationPrefix+strings.TrimPrefix(key, storedExtensionPrefix)] = value
		}
	}
	return annotations
}

// convertToOpenSandboxResponse projects a claimed agents sandbox into the
// OpenSandbox CreateSandboxResponse shape. It is the Phase 1 counterpart of
// the E2B layer's convertToE2BSandbox, trimmed to the fields the OpenSandbox
// create response actually carries (the upstream spec explicitly omits
// startup-source details and updatedAt from the create response).
//
// Metadata projects parsed user annotations and labels. Consumed native
// extensions and internal annotations are not echoed as ordinary metadata;
// requested Pod IP and the protected resource diagnostic are added separately.
func convertToOpenSandboxResponse(sbx infra.Sandbox, request CreateSandboxRequest) CreateSandboxResponse {
	agentsState, reason := sbx.GetState()
	state := MapState(agentsState, reason)

	// createdAt is schema-required and must never be empty: prefer the
	// claim-time annotation, fall back to the sandbox creation timestamp (a
	// persisted fact, essentially the claim moment for a fresh claim), and
	// finally to now as a defensive last resort.
	createdAt := time.Now().UTC()
	if claimTime, err := sbx.GetClaimTime(); err == nil {
		createdAt = claimTime
	} else if creationTimestamp := sbx.GetCreationTimestamp(); !creationTimestamp.IsZero() {
		createdAt = creationTimestamp.Time
	}

	// Match the native timeout projection: PauseTime takes precedence when
	// present; ordinary OpenSandbox creates set ShutdownTime.
	timeoutOpts := sbx.GetTimeout()
	expiresAt := timeoutOpts.ShutdownTime
	if !timeoutOpts.PauseTime.IsZero() {
		expiresAt = timeoutOpts.PauseTime
	}

	response := CreateSandboxResponse{
		ID: sbx.GetSandboxID(),
		Status: SandboxStatus{
			State:  state,
			Reason: reason,
		},
		Entrypoint: request.Entrypoint,
		CreatedAt:  createdAt.Format(time.RFC3339),
	}
	// The entrypoint key must stay present even for an empty value: the
	// generated SDKs pop it unconditionally, and [] reads better than null.
	if response.Entrypoint == nil {
		response.Entrypoint = []string{}
	}
	if !expiresAt.IsZero() {
		response.ExpiresAt = expiresAt.Format(time.RFC3339)
	}
	response.Metadata = maps.Clone(request.native.Extensions.Labels)
	if response.Metadata == nil {
		response.Metadata = map[string]string{}
	}
	maps.Copy(response.Metadata, request.Metadata)
	if sbx.GetAnnotations()[models.ExtensionKeyReturnPodIP] == agentsv1alpha1.True {
		if ip := sbx.GetIP(); ip != "" {
			response.Metadata[models.MetadataKeyPodIP] = ip
		}
	}
	for key, value := range sbx.GetAnnotations() {
		if strings.HasPrefix(key, extensionAnnotationPrefix) {
			if response.Extensions == nil {
				response.Extensions = Extensions{}
			}
			response.Extensions[storedExtensionPrefix+strings.TrimPrefix(key, extensionAnnotationPrefix)] = value
		}
	}
	// Resource context is written last so persisted user metadata cannot
	// spoof it. This mirrors the E2B layer's MetadataKeySandboxResource
	// convention and gives operators a stable diagnostic key.
	response.Metadata[MetadataKeySandboxResource] = fmt.Sprintf("%s/%s", sbx.GetNamespace(), sbx.GetName())
	return response
}

// MetadataKeySandboxResource is the protected metadata key carrying the
// agents-side namespace/name of the backing sandbox. It is written last so
// user-supplied metadata cannot spoof it, matching the E2B layer's
// models.MetadataKeySandboxResource convention.
const MetadataKeySandboxResource = "opensandbox.kruise.io/sandbox-resource"

// mapInfraErrorToApiError converts a manager/infra error into an ApiError
// with the appropriate HTTP status code based on managererrors.ErrorCode.
//
// The mapping mirrors the E2B create path so both protocols classify the same
// backend failure identically:
//   - ErrorBadRequest, ErrorNotFound → 400 (validation/lookup failures)
//   - ErrorConflict → 409
//   - ErrorQuotaExceeded → 403
//   - ErrorInternal, ErrorUnknown, untyped → 500
//
// It is duplicated here rather than imported from the E2B package to keep the
// two API surfaces independent: a future change to E2B's error contract must
// not silently reshape the OpenSandbox contract.
func mapInfraErrorToApiError(err error) *web.ApiError {
	switch managererrors.GetErrCode(err) {
	case managererrors.ErrorBadRequest, managererrors.ErrorNotFound:
		return &web.ApiError{Code: http.StatusBadRequest, Message: err.Error()}
	case managererrors.ErrorConflict:
		return &web.ApiError{Code: http.StatusConflict, Message: err.Error()}
	case managererrors.ErrorQuotaExceeded:
		return &web.ApiError{Code: http.StatusForbidden, Message: err.Error()}
	default:
		return &web.ApiError{Code: http.StatusInternalServerError, Message: err.Error()}
	}
}
