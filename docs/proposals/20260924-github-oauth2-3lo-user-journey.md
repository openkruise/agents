# GitHub OAuth2 3LO user journey for Agent Sandbox

This document is intended for developers who understand the basic concepts of OAuth2, OpenID Connect (OIDC), and JSON Web Tokens (JWTs). It follows a real user journey for Alice to explain how Agent Sandbox identifies a user, proves an Agent identity, and securely accesses the GitHub API after user authorization.

Alice experiences only three actions: opening the Agent, signing in, and clicking **Authorize** the first time the Agent accesses GitHub. Behind those actions, the system establishes three independent trust relationships. A Keycloak ID Token proves that the current user is Alice. An Agent Token in the Sandbox proves which Agent the current program represents. The identity provider exchanges both tokens for a short-lived Principal Token that proves Alice is delegating authority to that Agent. Finally, the credential provider uses the delegation proof to obtain Alice's GitHub Access Token so the tool can call the GitHub API.

```text
Alice signs in to Keycloak
        ↓
Agent obtains Alice's ID Token
        +
Sandbox already holds an Agent Token
        ↓
identity provider verifies both identities
        ↓
Issues a Principal Token
        ↓
credential provider finds or establishes Alice's GitHub authorization
        ↓
Returns a GitHub Access Token
        ↓
Agent tool calls the GitHub API
```

No single token applies throughout this chain. The ID Token, Agent Token, Principal Token, and GitHub Access Token serve user login, workload identity, internal delegation, and external resource access, respectively. They are not interchangeable and must not be sent to the wrong recipient.

## Platform preparation before Alice arrives

Alice does not create an OAuth client or need to understand issuers, callbacks, or Client Secrets. A platform administrator completes the following configuration:

1. Deploy Keycloak, create a Realm for Agent user identities, and select a user source. Users can be created directly in Keycloak or synchronized from LDAP, an enterprise directory, or an external identity provider (IdP).
2. Register the Agent application as a Keycloak client. Configure its `clientId`, `clientSecret`, allowed Redirect URI, and Authorization Code Flow. The Agent uses this client for user login and obtains an ID Token.
3. Create a platform-level GitHub OAuth App and register the callback URL used after GitHub authorization. The callback must point to the public credential provider endpoint, not to an ephemeral Sandbox.
4. Create `CredentialProvider/github-3lo`. Configure the GitHub vendor, OAuth App Client ID and Client Secret, callback base URL, and scopes. The credential provider resolves the vendor-specific authorization and token endpoints.
5. Use `AgentRole` and `AgentRoleBinding` to grant the target Agent permission to exchange Principal Tokens and use `CredentialProvider/github-3lo`.
6. Provide public configuration such as the Keycloak issuer, Agent login callback, and identity service endpoint to the Agent. In the initial release, the Client Secret must remain in a Kubernetes Secret and must not be sent to the browser. External key systems can integrate through future adapters.

The platform ultimately maintains two independent OAuth client configurations:

```text
Agent client in Keycloak
Purpose: Alice signs in to the Agent, and the Agent obtains Alice's ID Token

GitHub OAuth App
Purpose: Alice authorizes platform access to GitHub, and the credential provider
         obtains a GitHub Access Token and Refresh Token
```

Both configurations contain a Client ID, Client Secret, and callback, but they have different issuers, token purposes, and callback handlers. They must not be mixed.

## Participants and responsibilities

| Participant | Primary responsibility | Credentials held or processed |
|---|---|---|
| Alice's browser | Hosts login and GitHub authorization interactions and automatically sends the Agent Session Cookie | Session Cookie and a briefly visible Authorization Code |
| Keycloak | Authenticates Alice and issues an OIDC ID Token to the Agent | User account, ID Token, and optional login Refresh Token |
| Sandbox Controller | Requests and injects an Agent Token when identity is enabled | Agent identity configuration and Agent Token |
| Agent application | Maintains user sessions, selects and runs tools, and calls the identity SDK | Current user ID Token and Agent Token read source |
| identity provider | Verifies user and Agent identities, enforces authorization, and issues Principal Tokens | Keycloak trust configuration, Agent identities, and RBAC rules |
| credential provider | Manages third-party OAuth sessions and delegated credentials and returns resource Access Tokens | GitHub Client Secret, Access Token, and Refresh Token |
| GitHub Authorization Server | Presents authorization and exchanges an Authorization Code for tokens | GitHub user authorization, Authorization Code, Access Token, and Refresh Token |
| GitHub API | Returns GitHub resources that Alice can access | GitHub Access Token |

The initial release uses a web-based Agent application running in a Sandbox as its baseline. The same application maintains the Keycloak login session and reads the Sandbox Agent Token from a protected path, so it can exchange a Principal Token within one request. A headless Agent paired with an external portal follows the same token semantics but cannot directly forward a user ID Token across processes. That topology requires a one-time delegation handle or an equivalent future protocol and is outside the initial scope.

## Phase 1: The Sandbox obtains an Agent Token

When a Sandbox with identity enabled is created or claimed, the controller-side issuance path requests an Agent Token from the identity provider and propagates it to the Sandbox runtime through a controlled mechanism. After startup, the Agent reads the token from an agreed Token Source instead of storing long-lived static credentials in the image or application configuration.

```text
Create or claim Sandbox
        ↓
Controller determines logical Agent and Sandbox metadata
        ↓
Controller requests an Agent Token from the identity provider
        ↓
identity provider returns a short-lived Agent Token
        ↓
Controller propagates the token to the Sandbox runtime
        ↓
Agent reads from the Agent Token Source
```

The Agent Token answers:

```text
"Which Agent is the caller, and in which Sandbox workload is it running?"
```

It does not contain Alice's login state and cannot independently represent Alice. The implementation defines the exact claims. Conceptually, they include the Agent identifier, Sandbox identifier, issuer, audience, and expiration. A receiving service must validate the signature, `iss`, `aud`, and `exp`, then match the workload identity in the token against the current request context.

When the Agent Token expires, the Agent Token Source or workload identity component renews it. Tool code must not store or refresh the Agent Token directly.

## Phase 2: Alice signs in through Keycloak

When Alice first visits a web-based Agent, the complete sequence begins with the home page request:

```text
1. The browser opens the Agent home page: GET /
2. The Agent returns a login page with a Login button
3. Alice clicks Login
4. The browser requests the Agent endpoint: GET /login
5. The Agent reads the Keycloak issuer
6. The Agent retrieves the OIDC Discovery configuration
7. The Agent obtains authorization_endpoint from the configuration
8. The Agent returns HTTP 302 with a Location header
9. The browser automatically redirects to Keycloak
10. Alice signs in to Keycloak
11. Keycloak redirects to the Agent with an Authorization Code
12. The Agent backend sends the code to the Keycloak Token Endpoint
13. Keycloak returns an ID Token and any other required tokens
14. The Agent creates a local session and returns its Session ID through Set-Cookie
```

A typical configuration is:

```yaml
sso:
  clientId: sample-agent
  clientSecret: ${KEYCLOAK_CLIENT_SECRET}
  issuer: https://keycloak.example.com/realms/agent
  redirectUri: https://agent.example.com/callback
```

The `issuer` lets the Agent locate the standard OIDC Discovery URL:

```text
https://keycloak.example.com/realms/agent/.well-known/openid-configuration
```

The Agent obtains the Authorization Endpoint, Token Endpoint, and JWKS URL from the Discovery document instead of hard-coding a Keycloak login page in application logic.

### ID Token and session

A Keycloak ID Token usually uses JWT format. The following example shows a decoded JWT payload, not a complete token string:

```json
{
  "iss": "https://keycloak.example.com/realms/agent",
  "sub": "4f64c78c-6d3f-4c14-8b91-e758174b7932",
  "name": "Alice",
  "email": "alice@example.com",
  "aud": "sample-agent",
  "exp": 1790123456
}
```

The Agent uses `iss + sub` as the stable user identifier. Names, email addresses, and usernames can change and are unsuitable as primary user keys.

After login, the Agent typically stores this server-side mapping:

```text
Session ID
  → Alice's stable user identifier
  → Alice's ID Token
  → Login token expiration information
```

The browser stores only a Session Cookie such as:

```http
Set-Cookie: sample_agent_sid=login-1001; Path=/; HttpOnly; Secure; SameSite=Lax
```

The browser automatically includes it in subsequent requests:

```http
Cookie: sample_agent_sid=login-1001
```

The Agent uses the Session ID to find Alice's identity information. The ID Token must not be exposed to frontend scripts as a plaintext cookie.

## Phase 3: A user request triggers a tool call

Alice enters:

```text
show me my top 3 github repos
```

Responsibilities are divided as follows:

```text
LLM
  → Only decides to invoke the inspect_github_repos tool

inspect_github_repos
  → Knows that github-3lo credentials are required before accessing GitHub

identity SDK
  → Exchanges, caches, and expires Principal Tokens

identity and credential services
  → Determine whether Alice has authorized GitHub and manage third-party tokens
```

The LLM does not determine whether a token exists, access Client Secrets, or construct identity provider protocol requests.

The mapping between a tool and `CredentialProvider/github-3lo` must be explicit in code or configuration. `AgentRole` only determines whether the Agent may use the CredentialProvider. It does not tell an arbitrary tool which Provider to select.

## Phase 4: Exchange a Principal Token on demand

Successful Alice login does not need to issue a Principal Token immediately. The SDK exchanges one on demand when a tool first needs to call a downstream service on Alice's behalf.

```text
Current request session
  → Alice's ID Token

Agent Token Source
  → sample-agent's Agent Token

ID Token + Agent Token
  → ExchangePrincipalToken
  → Principal Token
```

The identity provider performs two sets of validation during the exchange.

For the ID Token:

1. Validate its signature against the trusted Keycloak Discovery and JWKS configuration.
2. Validate claims such as `iss`, `aud`, `exp`, and `nbf`.
3. Identify Alice from `iss + sub`.

For the Agent Token:

1. Validate the signature, issuer, audience, and expiration.
2. Confirm the target Agent and validate the Sandbox or workload binding.
3. Find the bound `AgentRole` through `AgentRoleBinding`.
4. Confirm that a rule permits `ExchangePrincipalToken`.

After validation, the identity provider issues a short-lived Principal Token. Conceptually, it states:

```text
Alice delegates authority to sample-agent
```

A conceptual payload follows. The final protocol defines the exact claim names:

```json
{
  "iss": "https://identity.example.com",
  "sub": "principal:<opaque-stable-id>",
  "aud": "credential-provider",
  "namespace": "team-a",
  "act": {
    "sub": "system:agent:team-a:sample-agent",
    "sandbox_uid": "<kubernetes-sandbox-uid>"
  },
  "iat": 1790120000,
  "exp": 1790120900
}
```

In this payload:

- `sub` is an opaque Principal ID derived consistently from Alice's Keycloak `issuer + subject`. The Principal Token does not contain original external identity fields.
- `act.sub` identifies the Agent acting for Alice.
- `aud` restricts the token to a specific downstream service.
- `exp` limits the delegation to a short lifetime.

### Why not always send the ID Token and Agent Token

Submitting both original tokens to every downstream service could express the same basic identities, but a Principal Token creates a clearer internal trust boundary:

1. The Keycloak ID Token has the Agent application as its `aud` and is unsuitable for broad propagation across internal services.
2. Downstream services do not need separate integrations with Keycloak and the Agent Token issuer.
3. The identity provider validates the original identities once and centralizes delegation and RBAC decisions.
4. Downstream services trust only the identity provider and validate a short-lived JWT issued for them.
5. Distinct `aud` values prevent the same token from being reused laterally across downstream services.

For example:

```text
For credential provider: aud = credential-provider
For egress gateway:      aud = egress-gateway
For sandbox gateway:     aud = sandbox-gateway
```

A Principal Token is not a token type mandated by OAuth2 or OIDC. It is the platform's internal security envelope for a user delegation to an Agent.

## Phase 5: Request Alice's GitHub credentials

A tool ultimately makes a call similar to:

```text
result, err := identityClient.GetResourceOAuth2Token(ctx, "github-3lo")
```

`GetResourceOAuth2Token` is a high-level credential API defined by the platform, not a standard OAuth2 or OIDC interface. Standard protocols define primitives such as Authorization Endpoints, Token Endpoints, Authorization Codes, and Refresh Tokens. They do not define an operation that looks up user authorization by `github-3lo`, initiates authorization when needed, and returns a usable token. The platform therefore provides an explicit API contract and SDKs for languages such as Go and Python.

The SDK performs these operations internally:

```text
1. Read Alice's session and ID Token from ctx
2. Read the Agent Token from the Agent Token Source
3. Look up the cached Principal Token for Alice
4. Exchange another Principal Token if none exists or it is near expiration
5. Request github-3lo credentials with the Principal Token
6. Return one of two mutually exclusive results:
   TokenReady: includes a usable Access Token
   AuthorizationRequired: includes authorizationUrl, sessionId, expiresAt, and reason
```

`AuthorizationRequired` is a normal business result, not `401` or a general service error. The `reason` distinguishes initial authorization from reauthorization. The SDK returns this result to the application layer but does not open a browser. `sessionId` only correlates the authorization process and is not required when the Agent retries the RPC in the background.

The request to the credential provider conveys:

```text
Principal Token: Alice delegates authority to sample-agent
CredentialProvider: github-3lo
```

At this stage, Alice's original ID Token and Agent Token are normally no longer sent to the credential provider. They are sent only to the identity provider when a Principal Token is missing or must be renewed.

The method signature above is illustrative SDK usage. The normative headers, request fields, and response structure are defined in [Agent identity and user-delegated credentials](./20260924-agent-identity-and-user-delegated-credentials.md#7-service-api-contract) and must not be inferred from the pseudocode.

## Phase 6: The credential provider checks authorization records

The credential provider validates the Principal Token signature, `iss`, `aud`, and `exp`, then confirms that the Agent may perform:

```yaml
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

It then queries the delegation record by:

```text
Namespace
Alice's opaque, stable Principal ID
CredentialProvider: github-3lo
```

AgentIdentity and Sandbox are not part of the delegation storage key. They are restored from the current Principal Token and authorized against the latest AgentRoleBinding. This prevents Alice and Bob from sharing GitHub credentials, while also preventing an unauthorized Agent from obtaining Alice's token merely by knowing the `github-3lo` name.

The query returns one of two mutually exclusive typed-result branches:

```text
Valid credentials exist
  → TokenReady
  → Return or refresh the GitHub Access Token

No valid credentials exist
  → Create a single-use OAuth authorization session
  → AuthorizationRequired
  → Return authorizationUrl, sessionId, expiresAt, and reason
```

## Phase 7: Complete GitHub authorization on first use

On the first call, the credential provider creates a short-lived OAuth session for Alice and stores:

```text
digest of a random and unpredictable state
Alice's opaque, stable Principal ID
requesting AgentIdentity
Sandbox name and immutable UID
CredentialProvider/github-3lo
requested scopes
GitHub Callback URL
creation and expiration times
PKCE code_verifier
```

It then builds a GitHub Authorization URL from `CredentialProvider/github-3lo` and returns it to the Agent frontend. The browser opens the URL, and Alice clicks **Authorize** on GitHub.

### Which service receives the GitHub callback

GitHub does not return the Authorization Code to an ephemeral Sandbox. The Redirect URI registered for the GitHub OAuth App points to the credential provider's stable public endpoint, for example:

```text
https://identity.example.com/oauth2/callback
```

One fixed endpoint allows a platform-level GitHub OAuth App to serve multiple namespaces and users. The credential provider hashes the high-entropy `state` value and finds the unique OAuth session. It then restores the namespace, CredentialProvider, Alice, AgentIdentity, and Sandbox UID from that session. The callback query must not supply this context. A production deployment may place a dedicated Callback Gateway in front of the endpoint, but the processing semantics remain unchanged.

A GitHub callback resembles:

```text
GET /oauth2/callback?code=<authorization-code>&state=<state>
```

The credential provider must:

1. Hash the callback `state` and find the previously created OAuth session.
2. Verify that `state` matches and is unused and unexpired.
3. Restore Alice, AgentIdentity, Sandbox UID, and the `github-3lo` binding from the session. Recheck `GetResourceOAuth2Token` permission against current cluster state and the latest AgentRoleBinding.
4. Send the Authorization Code and matching PKCE `code_verifier` to the GitHub Token Endpoint.

GitHub returns:

```text
Access Token
Refresh Token
Access Token expiration time
Refresh Token expiration time
Granted scopes
```

This behavior requires the GitHub OAuth App to enable the token-lifetime capability that returns Refresh Tokens.

The credential provider stores the complete credential set in encrypted form:

```text
Namespace + Alice's opaque, stable Principal ID + github-3lo
  → GitHub Access Token
  → GitHub Refresh Token
  → Access Token expiration time
  → Refresh Token expiration time
  → Scopes and authorization state
```

A delegation record does not grant permanent access to every Agent. Each read restores the current Agent and Sandbox from the Principal Token and reauthorizes them against the latest AgentRoleBinding.

A Refresh Token is a sensitive, long-lived renewal credential. Cache an Access Token with its expiration to avoid refreshing it on every GitHub call. No token may be written to logs, and Base64 encoding is not encryption.

### Do not confuse the two callbacks

The complete flow contains two different callbacks:

| Callback | Source | Recipient | Purpose |
|---|---|---|---|
| Keycloak login callback | Keycloak | Web-based Agent application in the initial release | Exchanges a code for Alice's ID Token and establishes a login session |
| GitHub OAuth callback | GitHub | Stable public endpoint of the credential provider | Exchanges a code for Alice's GitHub Access Token and Refresh Token and stores the delegation |

After the GitHub OAuth callback completes, the credential provider displays a short result page without redirecting to the Agent. The success page states only that authorization is complete and the page can be closed. It does not include the Authorization Code, GitHub token, `state`, or internal error details. Agent retries continue independently in the background and require no further action from Alice.

## Phase 8: The Agent retries and resumes the tool call

After receiving `AuthorizationRequired` for the first time and opening the authorization page, the Agent starts background retries with a deadline. After the GitHub callback completes, the next retry obtains the GitHub token:

```text
Agent retries GetResourceOAuth2Token(ctx, "github-3lo") in the background
        ↓
SDK ensures that the Principal Token is valid
        ↓
credential provider checks Alice's GitHub delegation
        ↓
Still Pending: return AuthorizationRequired; Agent backs off and retries
Callback complete: return TokenReady(GitHub Access Token)
        ↓
Agent automatically resumes the original tool call
```

Background retries must reuse the same unexpired Pending Session, use backoff with jitter, and stop at the earlier of `expiresAt` and the request context deadline. They stop immediately after `TokenReady`, request cancellation, authorization timeout, or a non-retryable error. They must not continue indefinitely.

The tool calls the resource API with the GitHub Access Token:

```http
GET /user/repos HTTP/1.1
Host: api.github.com
Authorization: Bearer <GitHub Access Token>
```

Token flows must remain distinct:

```text
Principal Token
Agent → identity/credential provider
Purpose: Proves that Alice delegates authority to sample-agent

GitHub Authorization Code
GitHub → credential provider callback
Purpose: Exchanged once for GitHub tokens

GitHub Access Token
Agent tool or controlled egress → GitHub API
Purpose: Accesses GitHub resources authorized by Alice

GitHub Refresh Token
Stored only by the credential provider
Purpose: Renews the GitHub Access Token
```

The Principal Token is not sent to GitHub. A GitHub Access Token cannot replace an Agent Token or Principal Token.

## Phase 9: Subsequent calls and token renewal

Later requests from Alice normally do not show another authorization page:

```text
1. SDK obtains or renews a Principal Token
2. credential provider finds Alice's github-3lo record
3. Access Token is unexpired: return it directly
4. Access Token is expired: refresh it with the Refresh Token
5. Save the new Access Token and any rotated Refresh Token returned by GitHub
6. Return a usable Access Token
7. Tool calls the GitHub API
```

Alice clicks **Authorize** again only when the Refresh Token expires or is revoked, the granted scopes are insufficient, or the GitHub user revokes application authorization.

### Expiration handling for five token types

| Token | Recommended lifetime | Handler after expiration | Handling method |
|---|---:|---|---|
| Keycloak ID Token | Defined by Keycloak client policy | Agent login session | Renew through the login Refresh Token or single sign-on (SSO) session; require login again on failure |
| Agent Token | Short-lived; defined by workload identity policy | Agent Token Source | Renew through the identity system; business tools do not handle it directly |
| Principal Token | Recommended 5–15 minutes | identity SDK | Exchange another token with a valid ID Token and Agent Token; generally no Refresh Token |
| GitHub Access Token | Defined by GitHub policy | credential provider | Return while valid; refresh with the GitHub Refresh Token after expiration |
| GitHub Refresh Token | Longer-lived than the Access Token | credential provider | Store securely and handle rotation; require reauthorization after invalidation |

The SDK can read `exp` from the Principal Token JWT and exchange a replacement shortly before expiration, such as within a 30-second safety window. If a downstream service returns an explicit token-expiration `401`, clear the cache, exchange a new token, and retry only once to prevent an infinite loop.

## Recommended identity SDK design

An Agent maintains sessions but normally does not create a separate low-level client for each session. One concurrency-safe `identityClient` is shared by the Agent process, and the current session is passed through `context.Context` for each call:

```text
identityClient := identity.NewClient(
    identity.WithEndpoint(identityEndpoint),
    identity.WithAgentTokenSource(agentTokenSource),
)
```

Request handling example:

```text
func handleRequest(w http.ResponseWriter, r *http.Request) {
    session := sessionStore.Get(r)
    ctx := identity.WithUserSession(r.Context(), session)

    result, err := identityClient.GetResourceOAuth2Token(ctx, "github-3lo")
    if err != nil {
        // Handle login expiration, permission denial, or service failure.
        return
    }

    switch result.Kind {
    case identity.TokenReady:
        // Use result.AccessToken to call the GitHub API.
    case identity.AuthorizationRequired:
        // Send result.AuthorizationURL to the frontend and start bounded
        // background retries in the Agent.
    }
}
```

Responsibilities are divided as follows:

```text
Agent application
  → Maps Cookie to Session
  → Adds the current Session to ctx

identity SDK
  → Reads the current user ID Token from ctx
  → Reads the Agent Token from the Agent Token Source
  → Exchanges, caches, and renews Principal Tokens
  → Calls the credential provider
  → Re-exchanges a Principal Token at most once after expiration

Tool code
  → Declares that it needs github-3lo
  → Uses the returned GitHub Access Token to call GitHub
```

A Principal Token cache key includes at least:

```text
User: iss + sub
Agent identity
Target audience
```

It may also include the Session ID so Alice's logout immediately clears the current session cache. The current user must never be stored in an ordinary mutable field on the shared client. Otherwise, concurrent requests from Alice and Bob could use the wrong identity.

When multiple Agent instances share sessions or require horizontal scaling, sessions and authorization state belong in shared storage that provides consistency and encryption, not only in the memory of one process.

## Complete sequence

```mermaid
sequenceDiagram
    actor Alice
    participant Browser as Alice Browser
    participant Agent as Web Agent App
    participant Keycloak
    participant Identity as identity provider
    participant Credential as credential provider
    participant GitHubAuth as GitHub OAuth
    participant GitHubAPI as GitHub API

    Note over Agent,Identity: Sandbox already obtained an Agent Token through the controller

    Alice->>Browser: Open Agent home page
    Browser->>Agent: GET /
    Agent-->>Browser: Return page with Login button
    Alice->>Browser: Click Login
    Browser->>Agent: GET /login
    Agent-->>Browser: 302 redirect to Keycloak
    Browser->>Keycloak: Sign in as Alice
    Keycloak-->>Browser: Redirect to Agent with login code
    Browser->>Agent: GET /callback?code=...
    Agent->>Keycloak: Exchange code for tokens
    Keycloak-->>Agent: Alice ID Token
    Agent-->>Browser: Set-Cookie: Session ID

    Alice->>Agent: Request GitHub repositories
    Agent->>Agent: LLM selects inspect_github_repos
    Agent->>Identity: ID Token + Agent Token
    Identity->>Identity: Validate identities, workload, and RBAC
    Identity-->>Agent: Principal Token

    Agent->>Credential: Principal Token + github-3lo
    Credential->>Credential: Query Alice's authorization record

    alt Alice authorizes for the first time
        Credential-->>Agent: AuthorizationRequired(URL, SessionID, expiry, reason)
        Agent-->>Browser: Open GitHub authorization page
        Note over Agent,Credential: Agent starts bounded background retries
        Browser->>GitHubAuth: Alice clicks Authorize
        GitHubAuth-->>Browser: Redirect to credential provider with code + state
        Browser->>Credential: GET /oauth2/callback?code=...&state=...
        Credential->>GitHubAuth: Exchange code for tokens
        GitHubAuth-->>Credential: Access Token + Refresh Token
        Credential->>Credential: Encrypt and store Alice's GitHub credentials
        Credential-->>Browser: HTTP 200 authorization-complete page; may close
        Agent->>Credential: Retry Principal Token + github-3lo in background
    else Alice has already authorized
        Credential->>Credential: Check or refresh Access Token
    end

    Credential-->>Agent: TokenReady(GitHub Access Token)
    Agent->>GitHubAPI: Bearer GitHub Access Token
    GitHubAPI-->>Agent: Alice's repository data
    Agent-->>Alice: Display result
```

## Trust boundaries and security requirements

1. Only the Agent login endpoint needs to understand the Keycloak ID Token. Downstream components must not independently reimplement Keycloak login semantics.
2. The identity provider is the sole issuer of internal Principal Tokens. Downstream services validate signatures against its fixed issuer and JWKS.
3. Every downstream service must strictly validate its own `aud`; signature and expiration validation alone are insufficient.
4. ID Tokens, Agent Tokens, Principal Tokens, and GitHub tokens must travel over Transport Layer Security (TLS).
5. Client Secrets, Access Tokens, and Refresh Tokens must not appear in logs, events, or ordinary CR fields.
6. OAuth `state` must have high entropy, single-use semantics, a short lifetime, and binding to the original request. OIDC login must also validate `nonce` and use PKCE when supported.
7. The Redirect URI must exactly match the value registered with the GitHub OAuth App. The callback result page must not redirect to a client-provided address.
8. GitHub credentials must be encrypted at rest. Key Management Service (KMS) envelope encryption or an equivalent key-management scheme is recommended.
9. When multiple users share a Sandbox, sessions, Principal Token caches, and credential queries must all be isolated by user.
10. User logout, administrator revocation, AgentRole changes, or GitHub authorization revocation must promptly invalidate related caches and delegations.

## Common misconceptions

### The "3" in 3LO means three token types

It does not. The three parties in 3LO generally refer to the user, OAuth client, and authorization or resource server. The number is unrelated to how many token types appear in the system.

### A JWT payload is an ID Token

It is not. A JWT wraps `Header.Payload.Signature`; the payload is only its data section. ID Tokens, Agent Tokens, and Principal Tokens may all use JWT format but serve different purposes.

### A Principal Token can call GitHub

It cannot. A Principal Token proves an internal delegation. The GitHub API accepts only an Access Token issued by GitHub.

### A GitHub Refresh Token can call the GitHub API

It cannot. A Refresh Token is sent only to the GitHub Token Endpoint to obtain a new Access Token.

### AgentRole automatically selects github-3lo

It does not. AgentRole determines whether access is allowed. Tool code or configuration must still explicitly select `CredentialProvider/github-3lo`.

### Each session needs a separate identity client

It does not. Share one concurrency-safe client, pass the current session through `ctx`, and isolate caches by user, Agent, and audience.

### A Principal Token is a standard OAuth token

It is not. It is the platform's internal delegation credential. Its exchange semantics can draw from Token Exchange, but the platform must define its claims, API, and authorization rules.

## Boundary between standard protocols and platform-defined capabilities

Standard OAuth2 and OIDC cover:

- OIDC Discovery and JWKS.
- Authorization Code Flow.
- PKCE.
- Token Endpoint.
- Access Tokens and Refresh Tokens.
- Protocol foundations for standard Token Exchange.

The platform must define and implement:

- Agent Token issuance and injection.
- Delegation binding between a user ID Token and an Agent Token.
- Principal Token claims, audiences, and lifetimes.
- Authorization rules for `ExchangePrincipalToken`.
- The `GetResourceOAuth2Token("github-3lo")` API and SDK.
- `CredentialProvider` configuration parsing.
- OAuth session handling, callback routing, and a safe result page.
- Encryption, storage, refresh, revocation, and auditing for third-party credentials.

The repository already provides identity issuance abstractions, JWT validation, Gateway Tokens, and related foundations. It does not yet implement a complete third-party OAuth delegation API. Calls in this document explain the user journey. The companion proposal defines normative service contracts, CRDs, error semantics, and security constraints.

## Flow summary

The complete flow can be condensed into nine steps:

```text
1. The platform configures Keycloak, a GitHub OAuth App, CredentialProvider,
   and Agent RBAC
2. The controller requests and injects an Agent Token for the Sandbox
3. Alice signs in through Keycloak; the Agent session stores Alice's ID Token
4. The LLM selects a tool that requires GitHub
5. The SDK exchanges Alice's ID Token + Agent Token for a Principal Token
6. The SDK requests CredentialProvider/github-3lo with the Principal Token
7. On first use, Alice authorizes on GitHub; the credential provider stores
   the Access Token and Refresh Token
8. Background retries obtain a valid GitHub Access Token and resume the
   original tool call
9. The tool calls the GitHub API with the GitHub Access Token; the credential
   provider refreshes it on subsequent calls
```

The four essential statements are:

```text
An ID Token proves who the user is.
An Agent Token proves which Agent is calling.
A Principal Token proves that this user delegates authority to this Agent.
A GitHub Access Token lets a tool access GitHub resources authorized by that user.
```
