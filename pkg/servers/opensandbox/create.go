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
	"net/http"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"

	sandboxmanager "github.com/openkruise/agents/pkg/sandbox-manager"
	"github.com/openkruise/agents/pkg/sandbox-manager/config"
	managererrors "github.com/openkruise/agents/pkg/sandbox-manager/errors"
	"github.com/openkruise/agents/pkg/sandbox-manager/infra"
	"github.com/openkruise/agents/pkg/servers/web"
	annotationutils "github.com/openkruise/agents/pkg/utils/annotations"
	"github.com/openkruise/agents/pkg/utils/timeout"
)

// defaultTimeoutSeconds applies when the request omits `timeout`. It mirrors
// the E2B layer's models.DefaultTimeoutSeconds so both protocols share the
// same operator-visible default; the value is intentionally not configurable
// per-protocol to avoid a second tuning knob for the same backend behavior.
const defaultTimeoutSeconds = 300

// minTimeoutSeconds floors the accepted `timeout` value. It mirrors the E2B
// layer's models.DefaultMinTimeoutSeconds for the same reason as the default.
const minTimeoutSeconds = 30

// noServerTimeout bounds the claim/wait-ready phases when the operator has not
// configured a tighter deadline. It is a far-future duration rather than a
// true infinity so existing timeout handling (context deadlines, retry step
// counts, wait-ready polling) keeps working unchanged. The value and rationale
// mirror the E2B layer's noServerTimeout.
const noServerTimeout = 100 * 365 * 24 * time.Hour

// CreateSandbox handles `POST /v1/sandboxes`. It translates the OpenSandbox
// create request into an agents claim, delegates to SandboxManager, and
// translates the claimed sandbox back into the OpenSandbox response shape.
//
// Phase 1 scope:
//   - Startup source: `image.uri` only, resolved to a SandboxTemplate via the
//     static alias table. `snapshotId` is rejected with 400.
//   - Propagated fields: `timeout`, `env`, `metadata`.
//   - Echoed fields: `entrypoint` is not propagated to the sandbox spec but
//     is echoed in the response, which the OpenSandbox response schema
//     requires ("copied from the creation request").
//   - Ignored fields (parsed but not propagated, declared as compatibility
//     limits in the proposal): `resourceLimits`, `networkPolicy`,
//     `secureAccess`, `lifecycle`, `platform`.
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

	templateID, mapErr := ResolveTemplateID(s.imageAliases, request.Image.URI)
	if mapErr != nil {
		// An unmapped image means this deployment cannot serve the
		// requested creation source. The OpenSandbox reference server
		// reports an unavailable creation source as 500 (see
		// kubernetes_service._ensure_pool_ref_exists, which maps a
		// missing poolRef to HTTP_500), and the compat layer mirrors
		// that status for behavioral parity; the message still names
		// the unmapped URI and the flag the operator must extend.
		return web.ApiResponse[CreateSandboxResponse]{}, &web.ApiError{
			Code:    http.StatusInternalServerError,
			Message: mapErr.Error(),
		}
	}

	namespace := NamespaceOfUser(user)
	log.Info("opensandbox create request received",
		"imageURI", request.Image.URI, "templateID", templateID, "namespace", namespace, "timeout", request.Timeout)

	accessToken := config.NewDefaultAccessToken()
	infraOpts := infra.ClaimSandboxOptions{
		Namespace:    namespace,
		Template:     templateID,
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

	sbx, err := s.manager.ClaimSandbox(ctx, sandboxmanager.ClaimSandboxOptions{
		Infra: infraOpts,
		Quota: user.QuotaSpec.DeepCopy(),
	})
	if err != nil {
		log.Error(err, "opensandbox sandbox creation failed", "templateID", templateID)
		return web.ApiResponse[CreateSandboxResponse]{}, mapInfraErrorToApiError(err)
	}
	log.Info("opensandbox sandbox created", "id", sbx.GetSandboxID(), "sandbox", klog.KObj(sbx))

	return web.ApiResponse[CreateSandboxResponse]{
		Code: http.StatusAccepted,
		Body: convertToOpenSandboxResponse(sbx, request),
	}, nil
}

// parseCreateSandboxRequest decodes and validates the OpenSandbox create
// request body. Validation is protocol-level only (shape, required fields,
// timeout bounds); backend-level validation (template existence, quota) is
// left to the manager call so error classification stays in one place.
func parseCreateSandboxRequest(r *http.Request, maxTimeout int) (CreateSandboxRequest, *web.ApiError) {
	var request CreateSandboxRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: fmt.Sprintf("invalid request body: %v", err),
		}
	}

	// Phase 1 supports image-based creation only. Snapshot restore is a
	// separate code path (CloneSandbox) that belongs to a later PR; rejecting
	// it explicitly here keeps the contract honest instead of silently
	// ignoring the field.
	if request.SnapshotID != "" {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "snapshotId restore is not supported in Phase 1; use image.uri",
		}
	}
	if request.Image == nil || strings.TrimSpace(request.Image.URI) == "" {
		return request, &web.ApiError{
			Code:    http.StatusBadRequest,
			Message: "image.uri is required",
		}
	}

	// Timeout bounds mirror the E2B layer so both protocols share the same
	// operator-visible ceiling. An omitted timeout falls back to the shared
	// default rather than the backend's own default, so the two surfaces
	// behave identically for the same request.
	if request.Timeout == 0 {
		request.Timeout = defaultTimeoutSeconds
	}
	if request.Timeout < minTimeoutSeconds || (maxTimeout > 0 && request.Timeout > maxTimeout) {
		return request, &web.ApiError{
			Code: http.StatusBadRequest,
			Message: fmt.Sprintf("timeout must be between %d and %d seconds",
				minTimeoutSeconds, maxTimeout),
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

// applyCreateModifier writes the request-scoped sandbox shape (lifetime,
// user metadata) onto the claimed sandbox before it is persisted. It is the
// Phase 1 counterpart of the E2B layer's basicSandboxCreateModifier, trimmed
// to the fields OpenSandbox actually propagates.
//
// Deliberately omitted (Phase 1 compatibility limits, declared in the
// proposal): auto-pause/wake, paused-retention, security rules, CSI mounts,
// network policy, inplace-update, return-pod-IP. Adding any of these here
// without a corresponding proposal update would silently expand the contract.
func applyCreateModifier(sbx infra.Sandbox, request CreateSandboxRequest) {
	// Lifetime: OpenSandbox `timeout` is the sandbox's absolute lifetime, so
	// it maps to ShutdownTime. PauseTime stays unset — auto-pause is an E2B
	// concept with no OpenSandbox counterpart in Phase 1.
	now := time.Now()
	sbx.SetTimeout(timeout.Options{
		ShutdownTime: timeout.NormalizeTime(now.Add(time.Duration(request.Timeout) * time.Second)),
	})

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
		response.Metadata = request.Metadata
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
