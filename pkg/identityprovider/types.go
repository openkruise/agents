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

// Package identityprovider provides an abstraction for issuing identity-aware access tokens
// for sandboxes. It supports both a simple UUID-based fallback (community default) and
// an external identity provider service that issues tokens via HTTPS.
package identityprovider

// ActionName is the HTTP header value for the identity provider API action.
const ActionName = "GetIdentityToken"

// TokenType represents the type of token being requested.
type TokenType string

const (
	// TokenTypePrincipal requests a token for a principal (user or service).
	TokenTypePrincipal TokenType = "Principal"
	// TokenTypeAgent requests a token for an agent sandbox.
	TokenTypeAgent TokenType = "Agent"
)

// TokenRequest represents a request to issue an identity-aware access token.
type TokenRequest struct {
	// TokenType indicates the type of token being requested.
	TokenType TokenType `json:"tokenType"`

	// Agent contains the agent information. Required when TokenType is "Agent".
	Agent *AgentInfo `json:"agent,omitempty"`

	// Principal contains the identity of the requesting entity. Required when TokenType is "Principal".
	Principal *PrincipalInfo `json:"principal,omitempty"`

	// Sandbox contains the sandbox workload metadata. Optional, used when TokenType is "Agent".
	Sandbox *SandboxInfo `json:"sandbox,omitempty"`

	// Metadata contains additional key-value pairs for the token request.
	Metadata map[string]string `json:"metadata,omitempty"`
}

// AgentInfo identifies the agent that owns this sandbox.
type AgentInfo struct {
	// AgentName is the name of the agent (e.g. "openclaw").
	AgentName string `json:"agentName"`
}

// PrincipalInfo represents the identity of the entity requesting the token.
type PrincipalInfo struct {
	// PrincipalName is the name of the principal (e.g. "third-party-app").
	PrincipalName string `json:"principalName"`
}

// SandboxInfo contains metadata about the sandbox for which a token is being issued.
type SandboxInfo struct {
	// PodUID is the Kubernetes pod UID.
	PodUID string `json:"podUid,omitempty"`

	// PodName is the Kubernetes pod name backing this sandbox.
	PodName string `json:"podName,omitempty"`

	// PodNamespace is the Kubernetes namespace of the sandbox pod.
	PodNamespace string `json:"podNamespace,omitempty"`

	// SandboxID is the unique identifier of the sandbox (namespace--name format).
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

	// RefreshToken is the token used to refresh the access token.
	RefreshToken string `json:"refreshToken,omitempty"`

	// SandboxClientID is the client identifier associated with this sandbox.
	SandboxClientID string `json:"sandboxClientId,omitempty"`

	// AccessTokenExpiration is the expiration time of the access token in RFC3339 format.
	AccessTokenExpiration string `json:"accessTokenExpiration,omitempty"`

	// RefreshTokenExpiration is the expiration time of the refresh token in RFC3339 format.
	RefreshTokenExpiration string `json:"refreshTokenExpiration,omitempty"`
}
