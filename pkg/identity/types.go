/*
Copyright 2025.

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

// Package identity provides an abstraction for issuing identity-aware access tokens
// for sandboxes. It supports both a default random token strategy (community default) and
// an identity provider service that issues tokens via HTTPS (enterprise deployment).
package identity

// TokenType represents the type of token being requested.
type TokenType string

const (
	// TokenTypePrincipal requests a token for a principal (user or service).
	TokenTypePrincipal TokenType = "Principal"
	// TokenTypeAgent requests a token for an agent sandbox.
	TokenTypeAgent TokenType = "Agent"
)

// TokenKind selects which token an IdentityProvider mints on a given IssueToken
// call. A single provider serves both kinds so enterprise deployments register
// one implementation instead of wiring two separate interfaces.
type TokenKind string

const (
	// TokenKindIDToken requests the ID token that is propagated into the sandbox
	// as a credential. This is the original identity-provider issuance path
	// (IssueSandboxToken and the security-token refresh flow).
	TokenKindIDToken TokenKind = "IDToken"
	// TokenKindAccessToken requests the access token (a JWT) used to reach the
	// sandbox through the sandbox gateway. Callers read the minted token from
	// TokenResponse.AccessToken.
	TokenKindAccessToken TokenKind = "AccessToken"
)

const (
	// SecurityMetadataPrefix is the prefix for all security-related annotations.
	SecurityMetadataPrefix = "security.agents.kruise.io/"
	// AgentKeyTokenRefreshStatus is the Sandbox Annotation Key,
	// used to store the JSON serialized result of TokenRefreshStatus.
	AgentKeyTokenRefreshStatus = SecurityMetadataPrefix + "token-status"
	// AnnotationAgentName is the sandbox Annotation Key whose presence opts the
	// sandbox into the identity provider issuance path. Its value carries the
	// logical agent name that the identity provider uses to mint the security
	// token. An annotation is used instead of a label so the value is free of
	// the 63-char / DNS-label constraints and can express richer content.
	AnnotationAgentName = SecurityMetadataPrefix + "agent-name"
	// AnnotationEnableJwtAuth is the sandbox Annotation Key whose value "true"
	// opts the sandbox into the JWT traffic-token issuance path. Unlike
	// AnnotationAgentName (which carries a meaningful agent name), this is a pure
	// boolean toggle: the traffic token is minted during claim only when this
	// annotation equals exactly "true". Any other value (including "1" or "True")
	// leaves the sandbox out of the issuance path.
	AnnotationEnableJwtAuth = SecurityMetadataPrefix + "enable-jwt-auth"
)

// TokenRequest represents a request to issue an identity-aware access token.
type TokenRequest struct {
	// TokenType indicates the type of token being requested.
	TokenType TokenType `json:"tokenType"`

	// Principal contains the identity of the requesting entity. Required when TokenType is "Principal".
	Principal *PrincipalInfo `json:"principal,omitempty"`

	// Sandbox contains the sandbox workload metadata. Optional, used when TokenType is "Agent".
	Sandbox *SandboxInfo `json:"sandbox,omitempty"`

	// Metadata contains additional key-value pairs for the token request.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// PrincipalInfo represents the identity of the entity requesting the token.
type PrincipalInfo struct {
	// PrincipalName is the name of the principal (e.g. "third-party-app").
	PrincipalName string `json:"principalName"`
}

// SandboxInfo contains metadata about the sandbox for which a token is being issued.
type SandboxInfo struct {
	// PodName is the Kubernetes pod name backing this sandbox.
	PodName string `json:"podName,omitempty"`

	// PodNamespace is the Kubernetes namespace of the sandbox pod.
	PodNamespace string `json:"podNamespace,omitempty"`

	// SandboxID is the unique identifier of the sandbox (namespace/name/uid format).
	SandboxID string `json:"sandboxId,omitempty"`

	// SandboxName is the name of the sandbox resource.
	SandboxName string `json:"sandboxName,omitempty"`

	// SandboxUID is the UID of the sandbox resource.
	SandboxUID string `json:"sandboxUid,omitempty"`
}

// TokenResponse represents the response from an identity provider.
type TokenResponse struct {
	// RequestID is the unique identifier of this token issuance request.
	RequestID string `json:"requestId"`

	// AccessToken is the issued identity-aware access token.
	AccessToken string `json:"accessToken"`

	// SandboxClientID is the client identifier associated with this sandbox.
	SandboxClientID string `json:"sandboxClientId,omitempty"`

	// AccessTokenExpiration is the expiration time of the access token in RFC3339 format.
	AccessTokenExpiration string `json:"accessTokenExpiration,omitempty"`
}

type TokenRefreshStatus struct {
	// AccessTokenExpiration is the expiration time of the refreshed access token in RFC3339 format
	AccessTokenExpiration string `json:"accessTokenExpiration,omitempty"`
}
