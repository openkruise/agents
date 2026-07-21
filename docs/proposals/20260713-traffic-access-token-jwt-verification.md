---
title: TrafficAccessToken JWT Verification via OIDC
authors:
  - "@czy"
reviewers:
  - "@TBD"
creation-date: 2026-07-13
last-updated: 2026-07-21
status: provisional
see-also:
  - "/docs/proposals/20260427-security-identity-provider.md"
  - "/docs/proposals/20260521-traffic-policy-and-security-profile.md"
---

# TrafficAccessToken JWT Verification via OIDC

## Summary

This proposal adds offline JWT verification for the `trafficAccessToken` used
to access a Sandbox through sandbox-gateway. When JWT authentication is enabled,
each gateway process loads the identity provider CA from Kubernetes, performs
OIDC discovery, downloads a JWKS snapshot, and locally verifies requests for
Sandbox routes that explicitly require traffic authentication.

The gateway binds signed `sandboxId` and `sandboxUid` claims to the selected
route, preventing a valid token for one Sandbox from being replayed against
another or against a replacement Sandbox with the same name. Token issuance
and delivery to clients remain owned by the identity integration and are not
changed by this proposal.

The process-wide JWT capability is disabled by default through Envoy filter
configuration. A Sandbox opts into JWT enforcement with the
`security.agents.kruise.io/enable-jwt-auth: "true"` annotation. Existing routes
without the annotation decode `RequireTrafficAuth` as `false`.

## Goals

- Verify JWT signatures using asymmetric public keys obtained through OIDC.
- Require and validate `exp`, `iat`, `nbf`, `iss`, and non-empty `sub` claims.
- Bind tokens to the current Sandbox ID and immutable Sandbox UID.
- Keep the request path offline after verifier initialization.
- Fail closed when a route requires JWT authentication but the JWT capability or
  verifier is unavailable.
- Preserve existing UUID authentication when JWT authentication is disabled.
- Keep configuration and dependencies out of sandbox-manager's controller-only
  feature-gate package.

## Non-Goals

- Audience or subject-policy authorization beyond requiring a non-empty `sub`.
- Authorization based on arbitrary signed `metadata` claims.
- Binding Pod name, namespace, or UID. These are not present in the shared
  gateway route model.
- Token introspection, revocation, or opaque-token support.
- Automatic CA or JWKS refresh. A gateway rollout is required after rotation.
- Returning replacement tokens issued by the security-token refresh controller.
- Issuing traffic tokens or adding a manager API that returns them to clients.
- Changing Sandbox claim, clone, or E2B create response behavior.
- Supporting more than one issuer per gateway process.

## Behavioral Contract

Gateway authentication has three valid process modes, combined with a
route-scoped traffic-auth requirement:

| `enable-auth` | `enable-jwt-auth` | `RequireTrafficAuth=false` | `RequireTrafficAuth=true` |
|---|---|---|---|
| `false` | `false` | Authentication disabled. | Fail closed with `503`. |
| `true` | `false` | Existing `x-access-token` UUID authentication. | Fail closed with `503`; do not fall back to UUID. |
| `true` | `true` | Skip gateway authentication and remove the traffic-token header. | Verify the JWT using the traffic-token header. |

`enable-jwt-auth=true` with `enable-auth=false` is invalid configuration.

`RequireTrafficAuth` is derived from the Sandbox annotation and propagated in
the internal Route model. Only the exact lowercase value `"true"` enables it;
missing or other values map to `false`. Historical Route payloads omit the
boolean and therefore keep the zero value `false`.

JWT mode replaces UUID authentication only at the gateway layer for protected
routes. It does not replace agent-runtime authentication: clients accessing an
endpoint that requires `x-access-token` must still provide the Sandbox runtime
token. The gateway leaves that header untouched and forwards it transparently.

The traffic-token header defaults to `x-traffic-access-token` and is
configurable. It must be a valid HTTP header name and must differ from
`x-access-token`. Its value is a compact JWT, not an `Authorization: Bearer`
value. The gateway removes this header after successful verification and from
unprotected routes while JWT mode is active, before forwarding the request to
the Sandbox.

## Token Claims

The initial claim shape is:

```json
{
  "exp": 1784102400,
  "iat": 1784098800,
  "nbf": 1784098800,
  "iss": "https://identity.example",
  "sub": "e2b:controlplane:client",
  "sandbox": {
    "sandboxId": "default--sample",
    "sandboxUid": "89d24507-936c-4a04-a958-b5d6a8277ed5"
  }
}
```

Verification is split into two stages:

1. `pkg/identity/oidc` validates the signature and registered claims.
2. `pkg/sandbox-gateway/filter` compares `sandbox.sandboxId` and
   `sandbox.sandboxUid` with `Route.ID` and `Route.UID`.

Required claim behavior:

| Claim | Validation |
|---|---|
| `exp` | Present and not expired, allowing configured clock skew. |
| `iat` | Present and not in the future beyond configured clock skew. |
| `nbf` | Present and not in the future beyond configured clock skew. |
| `iss` | Exactly matches the issuer returned by OIDC discovery. |
| `sub` | Present and non-empty. |
| `sandbox.sandboxId` | Exactly matches the selected route ID. |
| `sandbox.sandboxUid` | Exactly matches the selected route UID. |

The verifier parses but does not authorize `aud` or unknown custom claims.
Signature integrity must not be confused with authorization policy; future
metadata- or audience-based authorization requires a separate proposal.

## OIDC Initialization

Each sandbox-gateway process performs the following sequence after Envoy accepts
an enabled filter configuration:

1. Read the CA PEM from a ConfigMap using the controller-runtime API reader.
2. Create an HTTPS client rooted only in that CA, with TLS 1.2 or newer.
3. Fetch the configured OIDC discovery document without following redirects.
4. Read `issuer` and validate that `jwks_uri` is an absolute HTTPS URL.
5. Fetch and validate the JWKS.
6. Atomically publish an immutable verifier to filter workers.

Initialization retries with exponential backoff from one second to 30 seconds.
There are no per-request network calls. Once a verifier is published, it is not
replaced during the process lifetime.

The JWKS loader rejects:

- Empty sets, duplicate or empty `kid` values, and invalid keys.
- Symmetric keys and private keys.
- Keys whose `use` is not signing or whose declared `key_ops` omits `verify`.
- A JWK algorithm incompatible with its key type.

Supported signature algorithms are RSA PKCS#1, RSA-PSS, ECDSA, and Ed25519
algorithms supported by `go-jose`. `alg=none` and HMAC algorithms are rejected.

## Configuration

Envoy filter fields:

| Field | Default | Description |
|---|---|---|
| `enable-auth` | `false` | Enables gateway authentication. |
| `enable-jwt-auth` | `false` | Initializes the process-wide JWT capability. Requires `enable-auth`. |
| `traffic-access-token-header` | `x-traffic-access-token` | Header carrying the compact JWT. |

Gateway environment variables:

| Variable | Default |
|---|---|
| `OIDC_DISCOVERY_URL` | Required when JWT authentication is enabled |
| `OIDC_CA_CONFIGMAP_NAMESPACE` | Required when JWT authentication is enabled |
| `OIDC_CA_CONFIGMAP_NAME` | Required when JWT authentication is enabled |
| `OIDC_CA_CONFIGMAP_KEY` | `ca.crt` |
| `OIDC_CLOCK_SKEW` | `1m` |

Invalid local configuration causes Envoy filter configuration to fail rather
than silently degrading to UUID or unauthenticated access.

JWT capability is process-wide and cannot be configured in a per-route Envoy
filter configuration. Enforcement is route-scoped through the Sandbox
annotation and the resulting `Route.RequireTrafficAuth` boolean, not through
per-route Envoy filter fields.

## Token Acquisition Boundary

This change begins after a client has obtained a traffic access token. It does
not prescribe whether that token comes from an identity-provider SDK, a control
plane, or another trusted service. Sandbox-manager claim/clone processing and
the E2B create response remain unchanged.

## Readiness And Errors

The gateway system server exposes:

- `GET /healthz`: process liveness.
- `GET /readyz`: returns success when JWT is disabled or an enabled verifier is
  published; otherwise returns `503`.

The Deployment readiness probe uses `/readyz` on system port `7789`.

Request failures use these statuses:

| Condition | Status |
|---|---|
| JWT-required route but JWT mode or verifier is unavailable | `503 Service Unavailable` |
| Missing, malformed, expired, or invalid JWT | `403 Forbidden` |
| Sandbox ID or UID mismatch | `403 Forbidden` |

Error responses do not expose cryptographic details to clients. Detailed
verification errors remain in structured gateway logs.

## RBAC

The gateway requires `get` access to the configured CA ConfigMap in the
identity-provider namespace. Operators enabling JWT authentication must grant
that permission to the `sandbox-gateway` ServiceAccount. A namespaced Role
restricted with `resourceNames` is recommended. The permission is not included
in the default kustomization because the identity-provider namespace and
ConfigMap are deployment-specific. `config/sandbox-gateway/jwt-auth-rbac.yaml`
provides a ready-to-adapt sample for a same-namespace CA ConfigMap. Operators
using a cross-namespace identity provider must change the Role and RoleBinding
namespace and the allowed `resourceNames`, while keeping the RoleBinding subject
pointed at the sandbox-gateway ServiceAccount.

## Compatibility And Upgrade

- Authentication remains disabled by default in the shipped ConfigMap.
- Existing UUID mode has unchanged request and response behavior.
- In JWT mode, routes without the opt-in annotation skip gateway UUID and JWT
  validation. Operators must account for this when migrating from UUID mode.
- Gateways must be upgraded before operators create annotated routes because old
  gateway versions ignore `RequireTrafficAuth`.
- Sandbox-manager APIs and Sandbox claim/clone behavior are unchanged.
- No CRD, generated client, or protobuf changes are required.
- Enabling JWT requires the CA ConfigMap, identity-provider connectivity during
  startup, the required RBAC, and a gateway rollout.
- CA or signing-key rotation requires a gateway rollout because trust material
  is intentionally loaded once.

## Risks

| Risk | Mitigation |
|---|---|
| Identity provider unavailable during startup | Retry with capped exponential backoff and keep readiness false. |
| Unknown key after signing-key rotation | Fail closed and roll gateway pods after publishing the new JWKS. |
| Algorithm confusion | Allow only asymmetric algorithms and require key/algorithm compatibility. |
| Cross-Sandbox replay | Bind both Sandbox ID and immutable UID to the selected route. |
| Traffic token leaks to workloads | Remove the configured token header on successful JWT verification and from unprotected routes while JWT mode is active. |
| Initial token eventually expires | Clients must obtain a replacement through the identity system or recreate the Sandbox; refresh-token delivery is future work. |

## Test Plan

Unit tests cover:

- Environment configuration, provider-independent defaults, and validation.
- CA ConfigMap errors, TLS discovery, JWKS loading, response limits, and
  immutable key snapshots.
- Valid and invalid RSA/ECDSA JWTs, unsupported algorithms, signatures, key
  selection, required claims, issuer, skew, token size, and Sandbox binding.
- JWT manager startup ordering, retries, atomic publication, readiness,
  cancellation, idempotency, and concurrent readers.
- Filter configuration, UUID compatibility, JWT success/failure, unavailable
  verifier, route mismatch, custom headers, and header removal.
- Health and readiness handlers.

The JWT E2E profile uses a local HTTPS OIDC discovery/JWKS provider and covers:

- The in-cluster test issuer minting a token for the created Sandbox ID/UID.
- An unannotated Sandbox route succeeding without a traffic JWT in JWT mode.
- A valid JWT and the Sandbox runtime access token succeeding together.
- Missing, malformed, and expired JWTs returning `403`.
- A token issued for Sandbox A being rejected for Sandbox B.

## Alternatives

### Envoy `jwt_authn`

Envoy's native filter can validate JWTs, but the gateway must obtain its CA from
Kubernetes and bind claims to the dynamically selected Sandbox route. Keeping
verification in the existing Go filter provides both capabilities in one path.

### Per-Request Introspection

Calling the identity provider for every request would simplify revocation but
adds latency and creates a traffic-path availability dependency. Offline
verification is preferred for the initial implementation.

## Implementation History

- [x] 2026-07-13: Initial proposal drafted.
- [x] 2026-07-15: Finalized the initial ID/UID-only binding and static JWKS scope.
- [x] 2026-07-15: Added OIDC verifier, asynchronous manager, filter integration,
  readiness, RBAC, and unit/E2E coverage.
- [ ] Community review and follow-up design for key/token rotation.
