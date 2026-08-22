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

// Package models defines the OpenSandbox-compatible wire types, mirroring
// pkg/servers/e2b/models for the OpenSandbox protocol. Field names and shapes
// follow the OpenSandbox sandbox-lifecycle OpenAPI spec:
// https://github.com/alibaba/OpenSandbox/blob/main/specs/sandbox-lifecycle.yml
package models

import "time"

// SandboxState is the OpenSandbox lifecycle state, distinct from (and richer
// than) the E2B API's running/paused states and from the internal Sandbox CRD
// state machine. See state.go for the conversion.
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

// HeaderAPIKey is the OpenSandbox API key header, per the lifecycle spec's
// security scheme. OPEN_SANDBOX_API_KEY (the env-var form) is a client-side
// convention and has no server-side header equivalent.
const HeaderAPIKey = "OPEN-SANDBOX-API-KEY" // #nosec G101 -- header name, not a credential

const (
	MinListLimit = 1
	MaxListLimit = 100

	// ReservedMetadataPrefix keys are rejected on create and on metadata patch,
	// mirroring the spec's "opensandbox.io/ prefix is reserved" constraint.
	ReservedMetadataPrefix = "opensandbox.io/"

	// MinTimeoutSeconds is the spec's documented floor ("min 60"). A nil
	// Timeout (field omitted, or explicit JSON null) means "never expire", per
	// the spec's description of the null value.
	MinTimeoutSeconds = 60
)

// ImageSpec identifies the container image a sandbox is created from.
type ImageSpec struct {
	URI  string     `json:"uri"`
	Auth *ImageAuth `json:"auth,omitempty"`
}

type ImageAuth struct {
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// PlatformSpec constrains scheduling. Only linux/amd64 and linux/arm64 are
// meaningful against this Kubernetes-backed implementation; windows sandboxes
// are rejected at create time (see validate.go).
type PlatformSpec struct {
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// ResourceLimits is a free-form quantity map (e.g. {"cpu":"500m","memory":"512Mi"}),
// per the spec's ResourceLimits schema.
type ResourceLimits map[string]string

// NetworkPolicy is the sandbox's egress policy.
type NetworkPolicy struct {
	DefaultAction string          `json:"defaultAction,omitempty"`
	Egress        []NetworkEgress `json:"egress,omitempty"`
}

type NetworkEgress struct {
	Action string `json:"action"`
	Target string `json:"target"`
}

// CreateSandboxRequest is the POST /v1/sandboxes request body.
type CreateSandboxRequest struct {
	Image            *ImageSpec        `json:"image,omitempty"`
	SnapshotID       string            `json:"snapshotId,omitempty"`
	Platform         *PlatformSpec     `json:"platform,omitempty"`
	Timeout          *int              `json:"timeout,omitempty"`
	ResourceLimits   ResourceLimits    `json:"resourceLimits,omitempty"`
	ResourceRequests ResourceLimits    `json:"resourceRequests,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
	Entrypoint       []string          `json:"entrypoint,omitempty"`
	NetworkPolicy    *NetworkPolicy    `json:"networkPolicy,omitempty"`
	SecureAccess     bool              `json:"secureAccess,omitempty"`
	Extensions       map[string]string `json:"extensions,omitempty"`
}

// SandboxStatus is the nested status object on Sandbox.
type SandboxStatus struct {
	State            SandboxState `json:"state"`
	Reason           string       `json:"reason,omitempty"`
	Message          string       `json:"message,omitempty"`
	LastTransitionAt string       `json:"lastTransitionAt,omitempty"`
}

// Sandbox is the OpenSandbox lifecycle API's sandbox representation, returned
// by create/get/list/patch.
type Sandbox struct {
	ID         string            `json:"id"`
	Image      *ImageSpec        `json:"image,omitempty"`
	SnapshotID string            `json:"snapshotId,omitempty"`
	Platform   *PlatformSpec     `json:"platform,omitempty"`
	Status     SandboxStatus     `json:"status"`
	Metadata   map[string]string `json:"metadata,omitempty"`
	Entrypoint []string          `json:"entrypoint"`
	ExpiresAt  string            `json:"expiresAt,omitempty"`
	CreatedAt  string            `json:"createdAt"`
}

// PaginationInfo mirrors the spec's shared pagination envelope.
type PaginationInfo struct {
	Page        int  `json:"page"`
	PageSize    int  `json:"pageSize"`
	TotalItems  int  `json:"totalItems"`
	TotalPages  int  `json:"totalPages"`
	HasNextPage bool `json:"hasNextPage"`
}

// ListSandboxesResponse is the GET /v1/sandboxes response body.
type ListSandboxesResponse struct {
	Items      []*Sandbox     `json:"items"`
	Pagination PaginationInfo `json:"pagination"`
}

// RenewSandboxExpirationRequest is the POST .../renew-expiration request body.
type RenewSandboxExpirationRequest struct {
	ExpiresAt time.Time `json:"expiresAt"`
}

// RenewSandboxExpirationResponse is the POST .../renew-expiration response body.
type RenewSandboxExpirationResponse struct {
	ExpiresAt time.Time `json:"expiresAt"`
}
