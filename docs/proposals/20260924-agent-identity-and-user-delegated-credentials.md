# Agent identity and user-delegated credentials

> To understand this design and its complete call flow, first read [GitHub OAuth2 3LO user journey for Agent Sandbox](./20260924-github-oauth2-3lo-user-journey.md). That companion document explains identity and credential flows through an end-to-end user journey. This proposal defines the normative architecture, APIs, data models, and security constraints.

## 1. Summary

This proposal defines a community-oriented solution for Agent identity and user-delegated credentials. It addresses two problems:

1. Proving which Agent or Sandbox originated a request.
2. Allowing an Agent to obtain a token for a user's third-party resources on demand after the user grants authorization.

The initial scope covers Keycloak user login, Agent Tokens, Principal Tokens, and GitHub OAuth2 three-legged OAuth (3LO). It supports only web-based Agent applications that maintain their own user sessions: the user ID Token and Sandbox Agent Token converge in the same application process. An external portal paired with a headless Sandbox requires an additional one-time delegation protocol and is outside the initial scope.

## 2. Goals and scope

### 2.1 Goals

- Use Kubernetes custom resource definitions (CRDs) to declare trusted OpenID Connect (OIDC) issuers, Agent identities, role-based access control (RBAC), and OAuth providers.
- Issue a short-lived Agent Token for each Sandbox.
- Exchange a user ID Token and an Agent Token for a short-lived Principal Token.
- Use the Principal Token to retrieve or establish the user's GitHub OAuth delegation.
- Support token refresh, revocation, encrypted storage, and multi-replica consistency.
- Preserve extensibility across vendors, storage backends, and future machine-to-machine (M2M) capabilities.

### 2.2 Non-goals

- Treating a Keycloak token as a GitHub token.
- Allowing a large language model (LLM) to read, select, store, or refresh tokens.
- Returning a GitHub Refresh Token to a Sandbox.
- Implementing cross-process user delegation between an external portal and a headless Sandbox in the initial release.
- Mixing M2M into `GetResourceOAuth2Token`. Future M2M support uses a separate Client Credentials RPC that accepts only an Agent Token.

## 3. Architecture and core flow

### 3.1 Participants

| Participant | Responsibility |
|---|---|
| Keycloak | Authenticates end users and issues OIDC ID Tokens |
| sandbox-manager / agent-sandbox-controller | Requests, propagates, and refreshes Agent Tokens based on Sandboxes and AgentIdentities |
| Agent application | Maintains user sessions, calls the identity SDK, presents authorization pages, and retries in the background |
| agent-identity-provider | Verifies identities, issues Principal Tokens, and enforces Agent RBAC |
| credential provider | Manages OAuth sessions, third-party delegations, token refresh, and revocation |
| GitHub | Presents the authorization page and issues Authorization Codes and resource tokens |

Dependencies remain explicit: APIs and SDKs consume identity and credential capabilities. sandbox-manager and the controller integrate through neutral interfaces without depending on each other or leaking protocol models into Sandbox backend infrastructure.

### 3.2 Trust boundaries

1. Keycloak proves the end-user identity, not the Agent identity.
2. An Agent Token proves the workload identity, not the end-user identity.
3. A Principal Token proves that a user is delegating authority to an Agent.
4. A GitHub Access Token is used only for the GitHub API.
5. Client Secrets and Refresh Tokens are never returned to browsers or Sandboxes.
6. The LLM selects tools but does not handle tokens.

### 3.3 GitHub 3LO interaction overview

The complete interaction starts by establishing the Agent and user identities. The resulting Principal Token then authorizes the GitHub delegation flow.

```text
Browser               Agent App                 Keycloak             identity provider          GitHub
   |                       |                         |                         |                      |
1. |                       |<---------- Agent Token -------------------------|                      |
   |                       |   via control plane and Agent Token Source       |                      |
   |                       |                         |                         |                      |
2. |-- Sign in ----------->|                         |                         |                      |
   |                       |                         |                         |                      |
3. |<-- Redirect URL ------|                         |                         |                      |
   |                       |                         |                         |                      |
4. |----------------------------------------------->|                         |                      |
   |              Authenticate with Keycloak        |                         |                      |
   |                       |                         |                         |                      |
5. |<========== Redirect with Authorization Code ==|                         |                      |
   |                       |                         |                         |                      |
6. |-- Login callback ---->|                         |                         |                      |
   |                       |                         |                         |                      |
7. |                       |-- Exchange code ------>|                         |                      |
   |                       |<-- ID Token ------------|                         |                      |
   |                       |                         |                         |                      |
8. |-- Request GitHub ---->|                         |                         |                      |
   |   functionality       |                         |                         |                      |
   |                       |                         |                         |                      |
9. |                       |---------------- ExchangePrincipalToken -------->|                      |
   |                       |                 Agent Token + ID Token           |                      |
   |                       |<--------------- Principal Token -----------------|                      |
   |                       |                         |                         |                      |
10.|                       |---------------- GetResourceOAuth2Token -------->|                      |
   |                       |                 Principal Token                  |                      |
   |                       |<--------------- AuthorizationRequired -----------|                      |
   |                       |                 AuthorizationURL                  |                      |
   |                       |                         |                         |                      |
11.|<-- URL / UI event ----|                         |                         |                      |
   |                       |                         |                         |                      |
12.|--------------------------------------------------------------------------------------------->|
   |                      Open AuthorizationURL; sign in to GitHub and authorize                   |
   |                       |                         |                         |                      |
13.|<================================ GitHub returns HTTP 302 ======================================|
   | Location: https://identity.example.com/oauth2/callback?code=...&state=...                      |
   |                       |                         |                         |                      |
14.|-------------------------------------------------------------------------->|                      |
   | Browser automatically sends GET callback(code, state)                    |                      |
   |                       |                         |                         |                      |
15.|                       |                         |                         |-- Exchange code ---->|
   |                       |                         |                         |<-- GitHub Token ------|
   |                       |                         |                         |                      |
16.|<----------------------- HTTP 200: Authorization complete; close page -----|                      |
   |                       |                         |                         |                      |
17.|                       |---------------- Retry token RPC ---------------->|                      |
   |                       |<--------------- TokenReady -----------------------|                      |
```

The GitHub redirect and browser callback are separate HTTP actions. GitHub returns `302 + Location` to the browser, which then automatically sends a new `GET` request to the fixed callback endpoint. GitHub does not call the callback directly from its server. The callback only exchanges the code, persists the delegation, and returns an HTTP 200 result page. It does not redirect to or notify the Agent. Background retries independently observe the delegation state and remain invisible to the user.

## 4. Identity and token model

| Credential | Issuer | Consumer | Purpose |
|---|---|---|---|
| Keycloak ID Token | Keycloak | Agent application, identity provider | Proves the end-user identity |
| Agent Token | identity provider | Sandbox Agent | Proves the Agent or Sandbox workload identity |
| Principal Token | identity provider | credential provider | Proves a short-lived user-to-Agent delegation |
| GitHub Authorization Code | GitHub | credential provider callback | Exchanged once for GitHub tokens |
| GitHub Access Token | GitHub | Agent tool | Calls the GitHub API |
| GitHub Refresh Token | GitHub, optional | credential provider | Refreshes the GitHub Access Token |
| Sandbox Gateway Access Token | identity provider | Sandbox caller | Accesses Sandbox Gateway; unrelated to GitHub tokens |

An Agent Token must bind trusted Sandbox information. Typical claims include `namespace`, `agent_identity_name`, and the immutable `sandbox_uid`. The control plane derives these values from the Sandbox and AgentIdentity. Requesters cannot self-assert them.

A Principal Token uses an opaque, stable `sub` derived from the normalized Keycloak `issuer + subject`. The original `issuer`, `subject`, username, and email are not included in the token. The `act` claim binds the current Agent and Sandbox, and `aud` restricts the receiving service. The Credential API must still read the latest AgentRoleBinding for every request; historical authorization cannot be embedded permanently in a token.

Agent Tokens and Principal Tokens use asymmetric signatures, `kid`, and JSON Web Key Sets (JWKS). They may share a rotating key set, but `token_type` and distinct audiences must prevent interchangeability.

## 5. Kubernetes API design

### 5.1 Common conventions

All resources are namespaced CRDs in `security.agents.kruise.io/v1alpha1`. References are restricted to the same namespace by default. The API defines five resource kinds:

| Kind | Purpose |
|---|---|
| `AgentAuthenticationConfig` | Declares a trusted OIDC issuer |
| `AgentIdentity` | Declares a logical Agent and its allowed user authentication sources |
| `AgentRole` | Declares permitted identity and credential actions |
| `AgentRoleBinding` | Binds an AgentRole to an AgentIdentity |
| `CredentialProvider` | Declares a third-party OAuth client and user federation policy |

All CRDs enable the status subresource. Common status fields include at least `observedGeneration` and `conditions`. `Ready=True` means the current generation passed validation and its dependencies are available. Status, events, and logs must not contain tokens, Authorization Codes, Client Secrets, complete `state` values, or Proof Key for Code Exchange (PKCE) verifiers.

### 5.2 AgentAuthenticationConfig

Key fields:

- `type` supports only `JWT` in the initial release.
- `jwt.discoveryUrl` must be an absolute HTTPS URL for an OIDC Discovery document.
- `jwt.allowedAudience` contains 1 to 32 unique values.
- The controller obtains `issuer` and `jwks_uri` from Discovery. ID Token validation requires an exact `iss` match.

```yaml
apiVersion: security.agents.kruise.io/v1alpha1
kind: AgentAuthenticationConfig
metadata:
  name: keycloak
  namespace: team-a
spec:
  type: JWT
  jwt:
    discoveryUrl: https://keycloak.example.com/realms/agents/.well-known/openid-configuration
    allowedAudience:
      - sample-agent
```

### 5.3 AgentIdentity

Key fields:

- `authenticationRefs` references at least one `AgentAuthenticationConfig` in the same namespace.

The initial release does not expose a `tokenPolicy` in `AgentIdentity`. Server-side policy controls the lifetimes of Agent Tokens and Principal Tokens.

```yaml
apiVersion: security.agents.kruise.io/v1alpha1
kind: AgentIdentity
metadata:
  name: sample-agent
  namespace: team-a
spec:
  authenticationRefs:
    - apiGroup: security.agents.kruise.io
      kind: AgentAuthenticationConfig
      name: keycloak
```

A Sandbox selects an AgentIdentity through an annotation:

```yaml
metadata:
  annotations:
    security.agents.kruise.io/agent-name: sample-agent
```

The name must resolve to a Ready AgentIdentity in the current namespace. Otherwise, identity issuance fails and must not fall back to another token.

### 5.4 AgentRole

Supported actions:

| Action | Resource | Meaning |
|---|---|---|
| `ExchangePrincipalToken` | `AgentIdentity/<name>` or `*` | Exchanges a Principal Token |
| `GetResourceOAuth2Token` | `CredentialProvider/<name>` | Gets a resource token or creates an authorization session |
| `RevokeResourceOAuth2Delegation` | `CredentialProvider/<name>` | Revokes a user delegation |

Rules have Allow semantics only. A request is denied when no rule matches. The initial flow requires only `ExchangePrincipalToken` and `GetResourceOAuth2Token`. Grant `RevokeResourceOAuth2Delegation` only when an Agent is allowed to revoke a delegation directly. Actions on CredentialProvider resources must name the exact resource; wildcards are not allowed.

```yaml
apiVersion: security.agents.kruise.io/v1alpha1
kind: AgentRole
metadata:
  name: sample-agent-role
  namespace: team-a
spec:
  rules:
    - actions:
        - ExchangePrincipalToken
      resources:
        - "*"
    - actions:
        - GetResourceOAuth2Token
      resources:
        - CredentialProvider/github-3lo
```

### 5.5 AgentRoleBinding

`agentRoleRef` and `agentIdentityRef` reference an AgentRole and AgentIdentity in the same namespace. Both references are immutable after creation. Each binding targets one AgentIdentity. Create multiple bindings when the same AgentIdentity needs multiple roles; their Allow rules are combined. After a binding is deleted, new requests immediately use the updated authorization state.

```yaml
apiVersion: security.agents.kruise.io/v1alpha1
kind: AgentRoleBinding
metadata:
  name: sample-agent-binding
  namespace: team-a
spec:
  agentRoleRef:
    apiGroup: security.agents.kruise.io
    kind: AgentRole
    name: sample-agent-role
  agentIdentityRef:
    name: sample-agent
```

### 5.6 CredentialProvider

Key fields:

- `type` supports only `OAuth2` in the initial release.
- `grantType` supports only `UserFederation` in the initial release.
- `discovery.issuer`, `discovery.authorizationEndpoint`, and `discovery.tokenEndpoint` are configured by the user. All three fields must be absolute HTTPS URLs.
- `clientSecret.secretKeyRef.name` and `key` reference a Kubernetes Secret in the same namespace.
- `callbackBaseUrl` must be an absolute HTTPS URL without a query, fragment, or user information.
- The controller generates the read-only `status.callbackUrl={callbackBaseUrl}/oauth2/callback` field.
- `defaultScopes` must contain 1 to 32 unique values. Requested scopes must be a subset.

```yaml
apiVersion: security.agents.kruise.io/v1alpha1
kind: CredentialProvider
metadata:
  name: github-3lo
  namespace: team-a
spec:
  type: OAuth2
  oauth2:
    grantType: UserFederation
    discovery:
      issuer: "https://github.com"
      authorizationEndpoint: "https://github.com/login/oauth/authorize"
      tokenEndpoint: "https://github.com/login/oauth/access_token"
    client:
      clientId: <github-oauth-app-client-id>
      clientSecret:
        secretKeyRef:
          name: github-oauth-secret
          key: client-secret
    userFederation:
      callbackBaseUrl: "https://identity.example.com"
      defaultScopes:
        - repo
        - "read:user"
```

### 5.7 Validation and deletion

Admission validates structure, reference types, URLs, actions and resources, and OAuth2 field combinations. It does not perform network calls. The controller verifies dependency availability, loads Secrets, and updates Provider readiness.

- Deleting an AuthenticationConfig, Role, or AgentIdentity does not cascade to referring resources. Those resources transition to `Ready=False`.
- Deleting a RoleBinding immediately stops granting permissions to new requests.
- Deleting a CredentialProvider invalidates sessions and removes local delegations and caches. A failed vendor revocation is recorded in the audit log but does not block deletion indefinitely.
- Deleting the Secret makes the Provider unavailable.

## 6. Protocol flows

### 6.1 Agent Token

When a Sandbox is created, claimed, resumed, cloned, or reconstructed at runtime, the control plane reads its namespace, AgentIdentity, and Sandbox UID from the trusted Sandbox object. It asks the identity provider to issue a short-lived Agent Token and propagates the token through a protected file, Container Storage Interface (CSI), or runtime RPC. The default read path is:

```text
/var/opt/sandbox/agent-token
```

Tokens must not be written to Sandbox CRs, annotations, events, logs, or ordinary environment variables. A clone must receive an independent token issued for its new Sandbox UID. The token is reissued and atomically replaced before expiration and after resume.

### 6.2 User login and Principal Token

A web-based Agent uses the Keycloak Authorization Code Flow to establish a user session. The login `state`, OIDC `nonce`, and PKCE values remain completely separate from the subsequent GitHub OAuth session.

When an external credential is first required:

```text
Keycloak ID Token + Agent Token
  → ExchangePrincipalToken
  → Validate user, Sandbox, AgentIdentity, and AgentRole
  → Principal Token
```

The user ID Token and Agent Token must converge in the Agent application process. An SDK client may be shared across the process, but the current user session must be passed through `context.Context` or an explicit parameter. It must not be stored in a shared mutable field.

### 6.3 Get a GitHub token

`GetResourceOAuth2Token` returns one of two mutually exclusive results:

```text
TokenReady
  → Access Token, token type, expiration time, and granted scopes

AuthorizationRequired
  → Authorization URL, expiration time, and reason
```

`AuthorizationRequired` is a normal business result, not an authentication error. The SDK returns it to the Agent application. The application opens the authorization page and starts background retries. The SDK does not open a browser.

Background retries must:

- Call the same RPC with the same user context.
- Reuse an unexpired Pending Session for the same Principal, Provider, and scope set.
- Use backoff with jitter.
- Stop at the earlier of the response `expires_at` and the request context deadline.
- Stop on `TokenReady`, cancellation, authorization timeout, or a non-retryable error.
- Never poll the callback endpoint or retry indefinitely.

### 6.4 OAuth session and callback

A session stores at least:

```text
state hash
PKCE verifier
namespace
AgentIdentity
Sandbox name + immutable Sandbox UID
opaque Principal ID
CredentialProvider
requested scopes
redirect URI
created-at / expires-at
status / version
```

A platform-level GitHub OAuth App uses one fixed callback:

```text
https://identity.example.com/oauth2/callback
```

The `state` value must have high entropy, a short lifetime, and single-use semantics. The server stores only its digest and uses the digest to find the session. Namespace, Principal, AgentIdentity, Sandbox UID, and Provider values must be restored from the session rather than accepted from callback query parameters.

Callback state machine:

```text
Pending
  --Validate and consume state--> Exchanging
  --Code exchange and encrypted persistence succeed--> Completed
  --User denial, expiration, or exchange failure--> Failed
```

Only `Pending` can transition to `Exchanging`. A replay must not call the GitHub token endpoint again. Before completion, the service must also confirm that the Sandbox and AgentIdentity remain valid and recheck `GetResourceOAuth2Token` permission against the latest AgentRoleBinding.

On success, the callback returns a short `200 OK` result page that states, "Authorization complete. This page can be closed." The page must not contain tokens, the Authorization Code, the original `state`, or internal errors. It does not accept a client-provided redirect destination and does not redirect to or notify the Agent application.

### 6.5 Token refresh and revocation

Every delegation read checks the state, scopes, and Access Token expiration:

- If the token is valid, return `TokenReady`.
- If the token is near expiration and a Refresh Token exists, perform a singleflight refresh and update it atomically.
- If no Refresh Token exists or the vendor returns `invalid_grant`, mark the delegation as requiring reauthorization and return a new `AuthorizationRequired` result.
- If GitHub reports that authorization is invalid, remove the local delegation and require the user to authorize again.

Revocation first invalidates the local delegation and clears its cache, then makes a best-effort call to the vendor revocation endpoint. An external revocation failure does not restore the local delegation. Retry it in the background with a finite limit and record the result in the audit log.

## 7. Service API contract

The initial release uses Protocol Buffers and ConnectRPC. The OAuth callback is a separate HTTP GET endpoint. Every RPC returns a `request_id`, and time fields use `google.protobuf.Timestamp`.

### 7.1 Sensitive fields

The project defines a common protobuf FieldOptions extension:

```proto
package security;

import "google/protobuf/descriptor.proto";

extend google.protobuf.FieldOptions {
  bool sensitive = 51001;
}
```

Fields containing tokens, Authorization Codes, complete `state` values, PKCE verifiers, and Client Secrets must be annotated with `(security.sensitive) = true`. The logging interceptor redacts fields based on their descriptors. Continuous integration (CI) tests ensure that newly added sensitive fields are annotated.

### 7.2 IssueAgentToken

This RPC is available only to trusted control-plane callers. They authenticate with mutual TLS (mTLS), a Kubernetes ServiceAccount JSON Web Token (JWT), or an equivalent service identity. Sandbox information in the request body is not a trust source.

```proto
message IssueAgentTokenRequest {
  string namespace = 1;
  string sandbox_name = 2;
  string sandbox_uid = 3;
  string agent_identity_name = 4;
}

message IssueAgentTokenResponse {
  string agent_token = 1 [(security.sensitive) = true];
  google.protobuf.Timestamp expires_at = 2;
  string request_id = 3;
}
```

The server reads the Sandbox from the Kubernetes API or a trusted cache, validates its UID, namespace, and AgentIdentity, and determines token lifetime from server-side policy.

### 7.3 ExchangePrincipalToken

The Agent Token is sent in `Authorization: Bearer <agent-token>`, and the user ID Token is included in the request body. The caller cannot override the Agent or Sandbox identity.

```proto
message ExchangePrincipalTokenRequest {
  string id_token = 1 [(security.sensitive) = true];
  string audience = 2;
}

message ExchangePrincipalTokenResponse {
  string principal_token = 1 [(security.sensitive) = true];
  google.protobuf.Timestamp expires_at = 2;
  string request_id = 3;
}
```

The initial release permits only `credential-provider` as the `audience`.

### 7.4 GetResourceOAuth2Token

This RPC accepts only a Principal Token:

```http
Authorization: Bearer <principal-token>
```

```proto
message GetResourceOAuth2TokenRequest {
  string credential_provider_name = 1;
  repeated string requested_scopes = 2;
}

message GetResourceOAuth2TokenResponse {
  oneof result {
    OAuth2TokenReady ready = 1;
    OAuth2AuthorizationRequired authorization_required = 2;
  }
  string request_id = 3;
}

message OAuth2TokenReady {
  string access_token = 1 [(security.sensitive) = true];
  string token_type = 2;
  google.protobuf.Timestamp expires_at = 3;
  repeated string granted_scopes = 4;
}

message OAuth2AuthorizationRequired {
  string authorization_url = 1 [(security.sensitive) = true];
  google.protobuf.Timestamp expires_at = 2;
  string reason = 3; // FIRST_AUTHORIZATION or REAUTHORIZATION_REQUIRED
}
```

When `requested_scopes` is omitted, the Provider's `defaultScopes` apply. Explicit scopes must be a subset.

### 7.5 OAuth callback

```http
GET /oauth2/callback?code=...&state=...
GET /oauth2/callback?error=...&error_description=...&state=...
```

The callback restores the session from `state`, performs a single-use state transition, exchanges the code, and persists the encrypted result. It does not accept namespace, Provider, Principal, Agent, or Sandbox query parameters.

### 7.6 RevokeResourceOAuth2Delegation

This RPC is an optional production-readiness capability. Implement and grant `RevokeResourceOAuth2Delegation` only when Agents must be allowed to revoke user delegations directly.

```proto
message RevokeResourceOAuth2DelegationRequest {
  string credential_provider_name = 1;
}

message RevokeResourceOAuth2DelegationResponse {
  bool revoked = 1;
  string request_id = 2;
}
```

Repeated revocation returns `revoked=true`, preserving idempotency.

### 7.7 Error semantics

| Connect code | Typical reason | Handling |
|---|---|---|
| `invalid_argument` | `INVALID_SCOPE` | Correct the request; do not retry |
| `unauthenticated` | `TOKEN_MISSING`, `TOKEN_EXPIRED`, `TOKEN_INVALID` | Obtain valid identity credentials and retry at most once |
| `permission_denied` | `AGENT_ACTION_DENIED` | Do not retry |
| `not_found` | `AGENT_IDENTITY_NOT_FOUND`, `PROVIDER_NOT_FOUND` | Do not retry |
| `failed_precondition` | `PROVIDER_NOT_READY`, `SANDBOX_INACTIVE` | Wait for configuration recovery |
| `aborted` | `SESSION_ALREADY_CONSUMED`, `CONCURRENT_REFRESH` | Back off only for explicitly retryable cases |
| `resource_exhausted` | `RATE_LIMITED` | Back off according to server guidance |
| `unavailable` | `OIDC_UNAVAILABLE`, `VENDOR_UNAVAILABLE`, `STORE_UNAVAILABLE` | Use bounded backoff |
| `internal` | `INTERNAL_ERROR` | Do not expose internal details |

`authorization_required` is a typed result, not an error code.

## 8. Storage and consistency

### 8.1 Delegation key

A user delegation is isolated by:

```text
namespace
opaque Principal ID
CredentialProvider
```

The initial storage key omits AgentIdentity. Multiple authorized Agents in the same namespace can reuse a delegation for the same user and Provider. Every read is still authorized against the current Principal Token and latest AgentRoleBinding.

### 8.2 Default implementation

The initial implementation persists OAuth sessions and user delegations in Kubernetes Secrets:

- Sessions and delegations use separate Secrets with stable types and labels.
- Tokens, PKCE verifiers, and external account identifiers are encrypted with authenticated encryption with associated data (AEAD) before storage.
- A dedicated identity provider Secret supplies the root encryption key and is not mounted into Sandboxes.
- Ciphertext records include `key_id`, nonce, algorithm, and schema version.
- `resourceVersion` and compare-and-swap (CAS) operations handle races among callbacks, refreshes, and revocations.
- RBAC grants access to these Secrets only to the identity provider ServiceAccount.
- Base64 is not encryption.

Large-scale deployments may provide a database or Vault Store adapter but must preserve the same encryption, concurrency, and deletion semantics.

### 8.3 Cache

- Informer events invalidate the RBAC cache, which also uses a short time to live (TTL).
- Delegation cache keys include namespace, Principal, Provider, and scope.
- Persistent Store versions provide multi-replica consistency. Local singleflight is only an optimization.
- Refresh Tokens are not stored in ordinary caches.
- Revocation commits the persistent state before clearing caches.

## 9. Authorization and security requirements

Before returning a GitHub token, the service must confirm all of the following: the Principal Token is valid; the Agent and Sandbox match cluster state; the AgentIdentity exists; the AgentRoleBinding is current; the Role explicitly permits the target Provider; and the user delegation and scopes are valid. Failure of any check prevents token disclosure.

The implementation must satisfy these security requirements:

1. Validate every JWT signature, issuer, audience, time claim, and allowed algorithm.
2. Give OAuth `state` high entropy, a short lifetime, and single-use semantics. Store the PKCE verifier only on the server.
3. Require the callback URI to exactly match the value registered with the GitHub OAuth App.
4. In the initial release, allow Client Secrets to reference only Kubernetes Secrets in the same namespace.
5. Never write tokens, codes, complete `state` values, verifiers, or Client Secrets to logs, status, or events.
6. Never return Refresh Tokens to Sandboxes.
7. Isolate delegations by namespace and Principal, and require exact CredentialProvider authorization.
8. Obtain OIDC endpoints from trusted managed configuration and OAuth endpoints from the user-defined `CredentialProvider.spec.oauth2.discovery` configuration. Enforce HTTPS, address validation, egress restrictions, and response size limits.
9. Use atomic state transitions and concurrency control for token storage updates.
10. Generate audit records without sensitive values for all issuance, exchange, authorization, refresh, and revocation operations.

## 10. Modules and repository integration

Suggested modules:

- `api/security/v1alpha1`: five CRDs.
- `cmd/agent-identity-provider`: dependency assembly, process startup, and health checks.
- identity: Agent Tokens, Principal Tokens, and JWKS.
- authorization: AgentRole and AgentRoleBinding evaluation.
- credential: Provider registry, user-defined OAuth endpoint configuration, OAuth sessions, callbacks, and delegations.
- storage: Session and Delegation interfaces plus the default Kubernetes Secret adapter.
- SDK: Connect client, Token Source, Principal Token cache, and typed results.

Reconcilers use informer-backed clients to read CRDs and Secrets, resolve dependencies, update status, and refresh caches. Admission webhooks do not perform network calls. OIDC Discovery, JWKS, and Provider initialization use timeouts, backoff, jitter, and idempotent operations.

The repository already contains an `IdentityProvider` abstraction, Sandbox token issuance and propagation entry points, token types, and lifecycle integration points. It does not yet implement a complete user-delegated OAuth API. The existing `TokenKindIDToken` maps to the Sandbox Agent Token. `TokenKindAccessToken` still represents a Sandbox Gateway Access Token and must not map to a GitHub token.

## 11. Release scope and implementation order

This capability is disabled by default and does not add a global feature gate. Only Sandboxes that explicitly set `security.agents.kruise.io/agent-name` enter the secure identity path. Once selected, the path must fail closed and must not fall back to a random token when the identity provider is unavailable.

Recommended implementation phases:

1. Agent identity: AgentIdentity, Agent Token, and Claim, Resume, and Clone integration.
2. Principal identity: AuthenticationConfig, Keycloak, Principal Token, Role, and Binding.
3. GitHub 3LO: CredentialProvider, session, `state`, PKCE, callback, code exchange, encrypted delegation, and typed results.
4. Production readiness: refresh, revocation, concurrency control, key rotation, auditing, and high availability.
5. Extensions: a separate M2M RPC; GitLab, Google, and Slack support; and external Store or Vault adapters.

Recommended deployment order: install CRDs, webhooks, and RBAC; deploy the identity provider and its signing and encryption keys; create all five CR types and wait until they are Ready; configure sandbox-manager and controller clients; enable AgentIdentity for a small traffic slice; and finally enable the production callback for the GitHub OAuth App.

## 12. Testing and acceptance

Tests cover at least:

- CRD defaulting, validation, references, and Ready status.
- OIDC and JWKS validation and rotation.
- Agent and Principal claims, Sandbox UID mismatches, and permission denials.
- `IssueAgentToken → ExchangePrincipalToken → GetResourceOAuth2Token`.
- Initial authorization, Pending Session reuse, background retries, user denial, expiration, and callback replay.
- Token isolation under concurrent access by multiple users, Agents, and namespaces.
- Token refresh, `invalid_grant`, revocation, and CAS races.
- Redaction of Secret ciphertext, logs, status, events, and RPC error details.
- Claim, Clone, Resume, token refresh, and runtime reconstruction.
- Unchanged behavior for existing Sandboxes that do not enable identity features.

For acceptance, the user only completes Keycloak login and the initial GitHub authorization. The Agent must automatically receive `TokenReady` in the background and resume the original tool call. Deleting a Binding, delegation, Provider, AgentIdentity, or Sandbox must cause new requests to fail closed.

## 13. Related design

This proposal is a concrete implementation of the broader ideas in [Agent dynamic identity: SPIFFE workload identity and OAuth 2.0 token exchange](./20260727-agent-dynamic-identity-spiffe-token-exchange.md). That design defines the overall direction for dynamic Agent identity, SPIFFE workload identity, and OAuth 2.0 Token Exchange. This proposal narrows the initial implementation to Keycloak user identity, Agent Tokens, Principal Tokens, GitHub OAuth2 3LO, and the corresponding CRDs, service APIs, storage model, and security constraints.
