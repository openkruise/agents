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
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	agentsv1alpha1 "github.com/openkruise/agents/api/v1alpha1"
	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/e2b/models"
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

// CreateSandbox handles `POST /v1/sandboxes`. It translates the OpenSandbox
// create request into an agents claim or clone, delegates to SandboxManager, and
// translates the claimed sandbox back into the OpenSandbox response shape.
//
// Image requests use a fresh workload configuration. Warm-template preparation
// and reuse remain separate from this cold path; no static image alias is needed.
//
// The handler deliberately does not reuse the E2B CreateSandbox code path:
// the two protocols have different request/response shapes, different status
// codes (201 vs 202), and different field semantics. Sharing the manager call
// is enough; sharing the HTTP handler would couple the two surfaces.
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
	log.Info("opensandbox create request received",
		"source", request.source, "namespace", namespace)
	coldStart := &infra.ColdStartOptions{
		Command: slices.Clone(request.Entrypoint), EnvVars: maps.Clone(request.Env),
		Volumes: volumes, EgressPolicy: egress,
	}
	if request.Image != nil {
		coldStart.Image = request.Image.URI
	}
	if request.ResourceLimits != nil {
		coldStart.ResourceLimits = maps.Clone(*request.ResourceLimits)
	}
	// Resolve protocol defaults before crossing the neutral Manager boundary.
	coldStart.ResourceRequests = maps.Clone(coldStart.ResourceLimits)
	if request.ResourceRequests != nil && len(*request.ResourceRequests) != 0 {
		coldStart.ResourceRequests = maps.Clone(*request.ResourceRequests)
	}
	if request.Platform != nil {
		coldStart.OS, coldStart.Architecture = request.Platform.OS, request.Platform.Arch
	}

	accessToken := config.NewDefaultAccessToken()
	reserveFailedFor := time.Duration(0)
	infraOpts := infra.ClaimSandboxOptions{
		ReserveFailedSandboxFor: &reserveFailedFor,
		ColdStart:               coldStart,
		RuntimeConfig:           []agentsv1alpha1.RuntimeConfig{{Name: agentsv1alpha1.RuntimeConfigForInjectAgentRuntime}},
		UserMetadataKeys: &agentsv1alpha1.UpdatedMetadataInClaim{
			Annotations: slices.Sorted(maps.Keys(request.Metadata)),
		},
		Namespace:    namespace,
		User:         user.ID.String(),
		ClaimTimeout: noServerTimeout,
		InitRuntime: &config.InitRuntimeOptions{
			EnvVars:     request.Env,
			AccessToken: accessToken,
		},
		Modifier: func(sbx infra.Sandbox) error {
			applyCreateModifier(sbx, request)
			return nil
		},
		WaitReadyTimeout: noServerTimeout,
	}

	if request.source == createSourcePool {
		if len(request.Entrypoint) == 0 {
			request.Entrypoint = Entrypoint{"sleep", "infinity"}
		}
		infraOpts.ColdStart = nil
		infraOpts.Template = strings.TrimSpace(request.Extensions["poolRef"])
		infraOpts.StartProcess = &infra.StartProcessOptions{
			Command: slices.Clone(request.Entrypoint), EnvVars: maps.Clone(request.Env),
			OSUser: "user", Timeout: 30 * time.Second,
		}
	}

	var sbx infra.Sandbox
	var err error
	if request.source == createSourceSnapshot {
		if len(request.Entrypoint) == 0 {
			request.Entrypoint = Entrypoint{"sleep", "infinity"}
		}
		sbx, err = s.manager.CloneSandbox(ctx, sandboxmanager.CloneSandboxOptions{
			Infra: infra.CloneSandboxOptions{
				Namespace: namespace, User: user.ID.String(), CheckPointID: request.SnapshotID,
				GenerateName: "sandbox-", SkipWaitCheckpoint: true,
				CloneTimeout: noServerTimeout, WaitReadyTimeout: noServerTimeout,
				ReserveFailedSandboxFor: &reserveFailedFor, Modifier: infraOpts.Modifier,
				Startup: &infra.CloneStartupOptions{
					Command: slices.Clone(request.Entrypoint), EnvVars: maps.Clone(request.Env),
					ResourceRequests: coldStart.ResourceRequests, ResourceLimits: coldStart.ResourceLimits,
					OS: coldStart.OS, Architecture: coldStart.Architecture,
					InitRuntime: *infraOpts.InitRuntime,
				},
			},
			Quota: user.QuotaSpec.DeepCopy(),
		})
	} else {
		sbx, err = s.manager.ClaimSandbox(ctx, sandboxmanager.ClaimSandboxOptions{
			Infra: infraOpts,
			Quota: user.QuotaSpec.DeepCopy(),
		})
	}
	if err != nil {
		log.Error(err, "opensandbox sandbox creation failed", "source", request.source)
		return web.ApiResponse[CreateSandboxResponse]{}, mapInfraErrorToApiError(err)
	}
	log.Info("opensandbox sandbox created", "id", sbx.GetSandboxID(), "sandbox", klog.KObj(sbx))

	return web.ApiResponse[CreateSandboxResponse]{
		Code: http.StatusAccepted,
		Body: convertToOpenSandboxResponse(sbx, request),
	}, nil
}

// parseCreateSandboxRequest decodes and validates the OpenSandbox create
// request body with the same JSON decoding and timeout rules as E2B create.
// Only source selection is specific to this protocol; backend validation
// (template existence, quota) remains with the manager.
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

	// Metadata keys become Kubernetes annotation keys, so they must satisfy
	// the qualified-name rule and must not collide with internally reserved
	// prefixes. Rejecting at parse time keeps the manager call from failing on
	// a CRD validation error that would surface as 500, and prevents tenants
	// from injecting internal annotations (e.g. claim/pool state) through
	// user metadata. The blacklist check mirrors the E2B layer's
	// ValidateMetadataKey; it is applied via the shared annotations package
	// rather than imported from pkg/servers/e2b to keep the two API surfaces
	// independent.
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
	// Parsing resolves omitted/null/zero to the E2B default. Replace both
	// deadlines so the current request does not inherit a pooled deadline.
	opts := timeout.Options{}
	if request.Timeout != nil {
		opts.ShutdownTime = timeout.NormalizeTime(time.Now().Add(time.Duration(*request.Timeout) * time.Second))
	}
	sbx.SetTimeout(opts)

	if len(request.Metadata) == 0 {
		return
	}
	annotations := sbx.GetAnnotations()
	if annotations == nil {
		annotations = make(map[string]string, len(request.Metadata))
	}
	for k, v := range request.Metadata {
		annotations[k] = v
	}
	sbx.SetAnnotations(annotations)
}

// convertToOpenSandboxResponse projects a claimed agents sandbox into the
// OpenSandbox CreateSandboxResponse shape. It is the Phase 1 counterpart of
// the E2B layer's convertToE2BSandbox, trimmed to the fields the OpenSandbox
// create response actually carries (the upstream spec explicitly omits
// startup-source details and updatedAt from the create response).
//
// metadata is echoed from the request rather than read back from the sandbox
// annotations: the annotations map carries system-owned keys alongside user
// metadata, and filtering them here would duplicate the blacklist logic the
// E2B layer already owns. Echoing the request value keeps the response
// predictable and avoids leaking internal annotation keys.
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

	// expiresAt mirrors the E2B layer's endAt: the earlier of PauseTime and
	// ShutdownTime, with PauseTime taking precedence when set. Phase 1 never
	// sets PauseTime (no auto-pause), so this is ShutdownTime in practice;
	// the branch is kept so later phases inherit the correct semantics.
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
	if len(request.Metadata) > 0 {
		response.Metadata = maps.Clone(request.Metadata)
	}
	// Resource context is written last so persisted user metadata cannot
	// spoof it. This mirrors the E2B layer's MetadataKeySandboxResource
	// convention and gives operators a stable diagnostic key.
	if response.Metadata == nil {
		response.Metadata = map[string]string{}
	}
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
