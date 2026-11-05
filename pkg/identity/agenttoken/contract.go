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

// Package agenttoken defines the wire contract for requesting an agent token
// from the identity provider service. The serving process and the control-plane
// client share these types so the two cannot drift.
package agenttoken

// Path is where the identity provider serves agent token issuance. It is
// registered on the same TLS listener as OIDC discovery and the JWKS.
const Path = "/v1/agent-tokens"

// Request names the sandbox a token is requested for.
//
// None of these fields is a trust source: the service re-reads the Sandbox and
// verifies that its UID matches before minting. They exist so the service knows
// which object to look up, not to let the caller assert an identity.
type Request struct {
	Namespace   string `json:"namespace"`
	SandboxName string `json:"sandboxName"`
	SandboxUID  string `json:"sandboxUid"`
}

// Response carries the minted agent token. ExpiresAt is RFC3339.
type Response struct {
	AgentToken string `json:"agentToken"`
	ExpiresAt  string `json:"expiresAt"`
	RequestID  string `json:"requestId"`
}

// Error is the body returned for a rejected request. Code is a stable,
// machine-readable reason; Message is human-readable and carries no token
// material.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId"`
}

// Error codes returned by the issuance endpoint.
const (
	// CodeUnauthenticated means the caller presented no verified client
	// certificate.
	CodeUnauthenticated = "UNAUTHENTICATED"
	// CodeMethodNotAllowed means the request used a method other than POST.
	CodeMethodNotAllowed = "METHOD_NOT_ALLOWED"
	// CodeInvalidRequest means the body was unreadable or a required field was
	// empty.
	CodeInvalidRequest = "INVALID_REQUEST"
	// CodeSandboxNotFound means the namespace and name resolved to no Sandbox.
	CodeSandboxNotFound = "SANDBOX_NOT_FOUND"
	// CodeSandboxUIDMismatch means the Sandbox exists but its UID differs from
	// the one requested, so the caller is looking at a different object than the
	// one it believes it holds.
	CodeSandboxUIDMismatch = "SANDBOX_UID_MISMATCH"
	// CodeAgentIdentityNotSelected means the Sandbox carries no agent-name
	// annotation.
	CodeAgentIdentityNotSelected = "AGENT_IDENTITY_NOT_SELECTED"
	// CodeAgentIdentityNotFound means the selected AgentIdentity does not exist.
	CodeAgentIdentityNotFound = "AGENT_IDENTITY_NOT_FOUND"
	// CodeAgentIdentityNotReady covers deleting, stale, missing and negative
	// Ready verdicts: the identity is present but is not usable for the current
	// generation.
	CodeAgentIdentityNotReady = "AGENT_IDENTITY_NOT_READY"
	// CodeInternal means the service failed for a reason the caller cannot fix.
	CodeInternal = "INTERNAL_ERROR"
)
