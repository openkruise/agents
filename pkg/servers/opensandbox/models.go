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

// Package opensandbox implements the OpenSandbox API adapter alongside the
// native E2B API, sharing SandboxManager and API-key storage. Create and the
// existing list/describe/delete/pause/resume/renew routes are consolidated here.
// Full create compatibility and endpoint integration remain in progress; the
// proposal records the fixed contract baseline and remaining execution gaps.
//
// Layering: this package sits in the API layer (pkg/servers/**). It depends on
// pkg/sandbox-manager (Manager) and pkg/servers/e2b/{keys,models} (shared auth
// types) and pure E2B validation wrappers. It does not use an E2B Controller,
// pkg/features, or any infra implementation subpackage.
package opensandbox

import (
	"encoding/json"

	"github.com/openkruise/agents/pkg/servers/e2b/models"
)

// HeaderOpenSandboxAPIKey is the authentication header defined by the
// OpenSandbox lifecycle spec. It is translated to the same KeyStorage lookup
// used by the E2B `X-API-Key` header (issue #690: "reuse keys.KeyStorage").
const HeaderOpenSandboxAPIKey = "OPEN-SANDBOX-API-KEY" // #nosec G101 -- header name, not a credential

// SandboxState is the shared lifecycle vocabulary for create and read responses.
type SandboxState string

const (
	SandboxStatePending    SandboxState = "Pending"
	SandboxStateRunning    SandboxState = "Running"
	SandboxStatePausing    SandboxState = "Pausing"
	SandboxStatePaused     SandboxState = "Paused"
	SandboxStateResuming   SandboxState = "Resuming"
	SandboxStateStopping   SandboxState = "Stopping"
	SandboxStateTerminated SandboxState = "Terminated"
	SandboxStateFailed     SandboxState = "Failed"
)

// Image identifies the container image used by a fresh sandbox workload.
type Image struct {
	URI string `json:"uri"`
	// Request-scoped registry credentials require pull-secret support.
	Auth *json.RawMessage `json:"auth,omitempty"`
}

// Platform mirrors the OpenSandbox top-level `platform` (PlatformSpec) field,
// which the spec defines as independent from `image`. Cold creation applies
// these constraints before the workload is scheduled.
type Platform struct {
	OS   string `json:"os,omitempty"`
	Arch string `json:"arch,omitempty"`
}

// ResourceLimits preserves the wire string map, including extended resources.
// The cold backend parses quantities before admission and creation.
type ResourceLimits map[string]string

// Extensions carries string-valued protocol extensions, including poolRef.
type Extensions map[string]string

// CredentialProxy enables outbound credential interception only when true.
type CredentialProxy struct {
	Enabled bool `json:"enabled"`
}

// Entrypoint preserves argv boundaries using the standard JSON decoder.
type Entrypoint []string

// NetworkPolicy preserves the wire shape. Execution follows native E2B
// allow-before-deny rules and the internet-access label for the default action.
type NetworkPolicy struct {
	DefaultAction string            `json:"defaultAction,omitempty"`
	Egress        []json.RawMessage `json:"egress,omitempty"`
}

// LifecycleHooks mirrors the OpenSandbox `lifecycle` field. Phase 1 parses it
// as raw JSON for forward compatibility but does not propagate it.
type LifecycleHooks struct {
	PreStart *json.RawMessage  `json:"preStart,omitempty"`
	Periodic []json.RawMessage `json:"periodic,omitempty"`
}

// CreateSandboxRequest models the create protocol independently of execution
// availability. Source validation precedes the handler's capability checks;
// successful parsing does not mean the requested source is implemented.
type CreateSandboxRequest struct {
	// Image and SnapshotID select sources outside template and ordinary Pool mode.
	Image      *Image `json:"image,omitempty"`
	SnapshotID string `json:"snapshotId,omitempty"`
	// Empty, absent and null templateId values do not select a template.
	TemplateID *string    `json:"templateId,omitempty"`
	Extensions Extensions `json:"extensions,omitempty"`
	// source is resolved after decoding and checking for competing sources.
	source createSource
	// native contains parsed API-only extensions and normalized security rules.
	native models.NewSandboxRequest
	// Entrypoint starts the image-only workload. Other sources inherit their
	// workload entrypoint; echoing this field does not imply execution.
	Entrypoint Entrypoint `json:"entrypoint,omitempty"`
	// Platform constrains placement of the cold workload.
	Platform *Platform `json:"platform,omitempty"`
	// Timeout is the sandbox lifetime in seconds, using E2B defaults and bounds.
	// Omitted/null/zero resolves to models.DefaultTimeoutSeconds during parsing.
	Timeout *int64 `json:"timeout,omitempty"`
	// Env configures the image-only workload or claim runtime commands.
	// Clone restores checkpoint runtime configuration without applying this map.
	Env map[string]string `json:"env,omitempty"`
	// Metadata supports native extensions and label: keys. Remaining entries
	// are qualified, non-reserved Sandbox annotations.
	Metadata map[string]string `json:"metadata,omitempty"`
	// ResourceLimits configures cold creation or native claim updates.
	ResourceLimits *ResourceLimits `json:"resourceLimits,omitempty"`
	// ResourceRequests independently updates CPU/memory on claims. Only image
	// cold creation defaults omitted/null/empty requests to its limits.
	ResourceRequests *ResourceLimits `json:"resourceRequests,omitempty"`
	// NetworkPolicy is applied before a cold workload is allowed to start.
	NetworkPolicy   *NetworkPolicy   `json:"networkPolicy,omitempty"`
	CredentialProxy *CredentialProxy `json:"credentialProxy,omitempty"`
	// Volumes retain the supplied storage configuration.
	// Existing PVC mounts use Pod volumes for cold starts and native CSI mounts
	// for other sources; provisioning remains a separate execution capability.
	Volumes []json.RawMessage `json:"volumes,omitempty"`
	// SecureAccess is parsed but not propagated in Phase 1.
	SecureAccess *bool `json:"secureAccess,omitempty"`
	// Lifecycle hooks are parsed but not propagated in Phase 1.
	Lifecycle *LifecycleHooks `json:"lifecycle,omitempty"`
}

// SandboxStatus mirrors the OpenSandbox `status` object on a sandbox
// resource. Phase 1 emits State and Reason; Message is reserved for later
// phases (describe/list) where the backend surfaces a human-readable
// explanation.
type SandboxStatus struct {
	State   SandboxState `json:"state"`
	Reason  string       `json:"reason,omitempty"`
	Message string       `json:"message,omitempty"`
}

// CreateSandboxResponse is the Phase 1 response body for `POST /v1/sandboxes`.
// It mirrors the OpenSandbox `CreateSandboxResponse` schema: the sandbox is
// returned after allocation with its native state projected into the protocol.
// The schema marks `id`, `status`, `createdAt`, and `entrypoint`
// required, so those keys are always serialized (never omitempty): the
// generated SDKs pop them unconditionally and fail on a missing key. Other
// startup-source details (image, resourceLimits) and `updatedAt` are
// intentionally omitted per the upstream spec note ("Use GET
// /sandboxes/{sandboxId} to retrieve the complete sandbox information").
type CreateSandboxResponse struct {
	ID     string        `json:"id"`
	Status SandboxStatus `json:"status"`
	// Entrypoint is echoed from the creation request, per the upstream spec
	// ("copied from the creation request"). Cold creation applies the same argv
	// to the workload before it starts.
	Entrypoint []string          `json:"entrypoint"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Extensions Extensions        `json:"extensions,omitempty"`
	ExpiresAt  string            `json:"expiresAt,omitempty"`
	// CreatedAt is schema-required, so it is never empty: it falls back to
	// the sandbox creation timestamp (then to now) when the claim-time
	// annotation is unreadable. See convertToOpenSandboxResponse.
	CreatedAt string `json:"createdAt"`
}

// SandboxResponse is the response body for the sandbox-scoped read routes
// (`GET /v1/sandboxes/{sandboxId}` and each item of `GET /v1/sandboxes`). It
// mirrors the OpenSandbox `Sandbox` schema, whose required keys are `id`,
// `status`, `createdAt`, and `entrypoint`; those are always serialized (never
// omitempty) so the generated SDKs can pop them unconditionally.
//
// Compatibility limits (Phase 2):
//   - `entrypoint` is always an empty list: the create path echoes the request
//     entrypoint but does not persist it, so it cannot be recovered on read.
//   - `image`, `snapshotId`, `platform`, `extensions`, and `allocation` are
//     omitted: agents sandboxes are template-backed, and the OpenSandbox
//     startup-source model has no agents counterpart yet (tracked by the
//     virtual-template direction in issue #690).
type SandboxResponse struct {
	ID         string            `json:"id"`
	Status     SandboxStatus     `json:"status"`
	Entrypoint []string          `json:"entrypoint"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	ExpiresAt  string            `json:"expiresAt,omitempty"`
	CreatedAt  string            `json:"createdAt"`
}

// PaginationInfo mirrors the OpenSandbox `PaginationInfo` schema. Every field
// is required, so the struct is always fully populated (no omitempty).
type PaginationInfo struct {
	Page        int  `json:"page"`
	PageSize    int  `json:"pageSize"`
	TotalItems  int  `json:"totalItems"`
	TotalPages  int  `json:"totalPages"`
	HasNextPage bool `json:"hasNextPage"`
}

// ListSandboxesResponse is the response body for `GET /v1/sandboxes`. It
// mirrors the OpenSandbox `ListSandboxesResponse` schema: both `items` and
// `pagination` are required. `Items` is always non-nil so it serializes as
// `[]` rather than `null` for an empty result set.
type ListSandboxesResponse struct {
	Items      []SandboxResponse `json:"items"`
	Pagination PaginationInfo    `json:"pagination"`
}

// RenewSandboxExpirationRequest is the request body for
// `POST /v1/sandboxes/{sandboxId}/renew-expiration`. It mirrors the OpenSandbox
// `RenewSandboxExpirationRequest` schema: `expiresAt` is a required RFC3339
// timestamp that must be in the future and after the current expiration.
type RenewSandboxExpirationRequest struct {
	ExpiresAt string `json:"expiresAt"`
}

// RenewSandboxExpirationResponse is the response body for the renew route. It
// mirrors the OpenSandbox `RenewSandboxExpirationResponse` schema, whose only
// required field is the updated `expiresAt`.
type RenewSandboxExpirationResponse struct {
	ExpiresAt string `json:"expiresAt"`
}
