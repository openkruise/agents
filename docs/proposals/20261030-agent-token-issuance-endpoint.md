---
title: Agent Token Issuance Endpoint
authors:
  - "@jicheng"
reviewers:
  - "@TBD"
creation-date: 2026-10-30
last-updated: 2026-10-30
status: provisional
---

# Agent Token Issuance Endpoint

## Summary

This proposal defines how the control plane obtains Agent Tokens: it asks the
`agent-identity-provider` process, which holds the signing key and re-reads the
Sandbox. The request is the `IssueAgentToken` contract from
[Agent Identity and User-Delegated Credentials](./20260924-agent-identity-and-user-delegated-credentials.md)
section 7.2, carried as HTTP+JSON on the issuer's existing TLS listener and
authenticated with the same client certificate the control plane already uses
for runtime mTLS.

## Motivation

The issuer added in `pkg/agentidentity` mints tokens but has no caller: nothing
in the control plane can ask it for one. Sandboxes select an AgentIdentity
through the `security.agents.kruise.io/agent-name` annotation, and the claim,
resume and clone paths issue an ID token for them.

Two placements are possible:

- Signing in each control-plane process. This puts the private key in every
  `sandbox-manager` and `agent-sandbox-controller` replica and removes the
  server-side check of which identity a Sandbox may assume.
- Signing in the identity provider. This matches proposal section 6.1 and 7.2:
  one component owns the key, the policy and the read of the Sandbox.

This proposal takes the second. Control-plane processes only run an
`identity.IdentityProvider` client; they hold no signing material.

## API Contract

No new Kubernetes API field. The wire contract is shared by both sides in
`pkg/identity/agenttoken`:

```
POST /v1/agent-tokens
Content-Type: application/json

{"namespace":"team-a","sandboxName":"sbx-1","sandboxUid":"..."}

200 {"agentToken":"...","expiresAt":"2026-10-30T01:00:00Z","requestId":"..."}
4xx {"code":"AGENT_IDENTITY_NOT_READY","message":"...","requestId":"..."}
```

The fields match the proposal's `IssueAgentTokenRequest` and
`IssueAgentTokenResponse`. `expiresAt` is RFC 3339.

### Authentication

The issuance listener is the TLS listener that already serves discovery and
JWKS. It verifies a client certificate when one is presented
(`VerifyClientCertIfGiven`) against an operator-supplied CA, and the issuance
handler refuses a request without a verified chain.

Requiring a certificate at the TLS layer instead would also exclude the JWKS,
which every verifier fetches without control-plane credentials. The
route-scoped check keeps discovery public without a second port or listener.

The operator-supplied CA is expected to be the CA that signs the runtime client
certificates, so the control plane reuses the `--runtime-client-cert-secret` /
`--runtime-client-cert-dir` bundle it already loads. A deployment that has not
enabled runtime TLS cannot enable remote issuance.

### Trust

The request body is not a trust source. The handler reads the named Sandbox
from the API, compares its UID with the request, then reads the AgentIdentity
named by the annotation and requires `Ready=True` for its current generation.
Missing, deleting, stale and negative verdicts all fail closed. The token
lifetime is chosen by the server.

The refusal codes are `UNAUTHENTICATED`, `METHOD_NOT_ALLOWED`,
`INVALID_REQUEST`, `SANDBOX_NOT_FOUND`, `SANDBOX_UID_MISMATCH`,
`AGENT_IDENTITY_NOT_SELECTED`, `AGENT_IDENTITY_NOT_FOUND`,
`AGENT_IDENTITY_NOT_READY` and `INTERNAL_ERROR`.

## Scope

`TokenKindAccessToken` (the Sandbox Gateway JWT) keeps the community default
placeholder inside the client provider. Gateway issuance has no community
endpoint yet, so this change must not alter the gateway path. When that
endpoint exists it can be added to the same contract.

The identity provider is opt-in: without `--signing-key-file` it runs only the
AgentIdentity controllers, and without `--tls-client-ca-file` the issuance path
stays unregistered. Control-plane processes opt in with
`--identity-provider-url`.

## Alternatives Considered

- **ConnectRPC/gRPC as written in the proposal.** The repo has no protobuf
  toolchain for this service and the contract is three strings and a token.
  HTTP+JSON keeps the client and the handler small and testable with the
  standard library, and the wire shape can move to protobuf later without
  changing the request semantics.
- **Issue a Kubernetes ServiceAccount JWT and validate it.** Requires the
  identity provider to fetch TokenReview or the API server's public key, and
  duplicates an authentication mechanism the runtime mTLS bundle already
  provides.
- **Per-process signing with the same key.** Removes the round trip, but moves
  key material into every replica and turns the AgentIdentity readiness check
  into a client-side check that a compromised caller can skip.

## Test Plan

- Handler: success claim contents, missing certificate, wrong method, malformed
  and extended bodies, missing Sandbox, UID mismatch, no selected identity,
  missing / stale / not-ready / deleting identity.
- Client: TLS handshake with the shared CA, request path and body, response
  mapping, refusal-body mapping, empty token, AccessToken fallback without a
  service call, and configuration validation.
- `AddIssuer`: a client CA without a signing key, a missing CA file, a CA file
  with no certificates, and a full configuration.
