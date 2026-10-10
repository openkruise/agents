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

// Package opensandbox implements the OpenSandbox-compatible API layer for
// sandbox-manager. It exposes the OpenSandbox lifecycle REST contract
// (baseline: OpenSandbox server/v0.2.3) alongside the native E2B API, sharing
// the same SandboxManager instance and API-key storage. Phase 1 covers the
// create path only; later phases add describe/list/pause/resume/delete,
// endpoints, and metadata patch.
//
// Layering: this package sits in the API layer (pkg/servers/**). It depends on
// pkg/sandbox-manager (Manager) and pkg/servers/e2b/{keys,models} (shared auth
// types). It does not import pkg/servers/e2b itself, pkg/features, or any
// infra implementation subpackage.
package opensandbox

import "encoding/json"

// HeaderOpenSandboxAPIKey is the authentication header defined by the
// OpenSandbox lifecycle spec. It is translated to the same KeyStorage lookup
// used by the E2B `X-API-Key` header (issue #690: "reuse keys.KeyStorage").
const HeaderOpenSandboxAPIKey = "OPEN-SANDBOX-API-KEY" // #nosec G101 -- header name, not a credential

// SandboxState enumerates the OpenSandbox lifecycle states. Phase 1 only
// emits StateRunning from the create path; the remaining constants are
// declared here so later phases (describe/list/pause/resume) reuse the same
// vocabulary instead of introducing string literals at each call site.
type SandboxState string

const (
	SandboxStatePending    SandboxState = "Pending"
	SandboxStateRunning    SandboxState = "Running"
	SandboxStatePausing    SandboxState = "Pausing"
	SandboxStatePaused     SandboxState = "Paused"
	SandboxStateResuming   SandboxState = "Resuming"
	SandboxStateStopping   SandboxState = "Stopping"
	SandboxStateTerminated SandboxState = "Terminated"
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

// NetworkPolicy preserves the ordered OpenSandbox egress contract. The adapter
// resolves defaults and supported destinations before allocating a workload.
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
	// Entrypoint is applied before the new workload starts. Snapshot requests
	// without an entrypoint use a new keep-alive process.
	Entrypoint Entrypoint `json:"entrypoint,omitempty"`
	// Platform constrains placement of the cold workload.
	Platform *Platform `json:"platform,omitempty"`
	// Timeout is the sandbox lifetime in seconds, using E2B defaults and bounds.
	// Omitted/null/zero resolves to models.DefaultTimeoutSeconds during parsing.
	Timeout *int64 `json:"timeout,omitempty"`
	// Env is applied before workload startup and to InitRuntime.
	Env map[string]string `json:"env,omitempty"`
	// Metadata is persisted as sandbox annotations and echoed back in the
	// response. Keys must be qualified Kubernetes annotation keys.
	Metadata map[string]string `json:"metadata,omitempty"`
	// ResourceLimits is applied to the new workload before admission.
	ResourceLimits *ResourceLimits `json:"resourceLimits,omitempty"`
	// ResourceRequests replaces the complete requests map; empty uses limits.
	ResourceRequests *ResourceLimits `json:"resourceRequests,omitempty"`
	// NetworkPolicy is applied before a cold workload is allowed to start.
	NetworkPolicy   *NetworkPolicy   `json:"networkPolicy,omitempty"`
	CredentialProxy *CredentialProxy `json:"credentialProxy,omitempty"`
	// Volumes retain the supplied storage configuration.
	// Existing PVC mounts are resolved by the cold backend; other sources and
	// provisioning remain separate execution capabilities.
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
// returned synchronously with `status.state: "Running"` after provisioning
// completes. The schema marks `id`, `status`, `createdAt`, and `entrypoint`
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
	ExpiresAt  string            `json:"expiresAt,omitempty"`
	// CreatedAt is schema-required, so it is never empty: it falls back to
	// the sandbox creation timestamp (then to now) when the claim-time
	// annotation is unreadable. See convertToOpenSandboxResponse.
	CreatedAt string `json:"createdAt"`
}
