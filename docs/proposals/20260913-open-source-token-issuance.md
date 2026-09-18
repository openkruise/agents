---
title: Open-Source Token Issuance and ID Token Distribution
authors:
  - "@AnshulPatil2005"
reviewers:
  - "@zmberg"
  - "@BH4AWS"
creation-date: 2026-09-13
last-updated: 2026-09-13
status: provisional
see-also:
  - "/docs/proposals/20260713-traffic-access-token-jwt-verification.md"
  - "/docs/proposals/20260427-security-identity-provider.md"
---

# Open-Source Token Issuance and ID Token Distribution

## Summary

`20260713-traffic-access-token-jwt-verification.md` gave sandbox-gateway a complete
OIDC verifier and listed token issuance as out of scope. Nothing has filled that gap
since: `defaultTokenProvider.IssueToken` returns `uuid.NewString()` for both token
kinds, and `PropagateSecurityToken` is an empty function. A community deployment
therefore cannot produce a token that its own gateway would accept.

This proposal specifies the issuing half: signing key material and its lifecycle, an
OIDC discovery and JWKS endpoint so a gateway can bootstrap without an external
identity provider, signed access and ID token issuance through the existing
`IdentityProvider` seam, and delivery of the ID token into the sandbox runtime.

It does not restate the verification contract. Claim validation, JWKS rules and the
gateway's readiness behaviour are defined by `20260713` and are treated here as fixed
inputs.

## Goals

- Sign traffic access tokens with an asymmetric key, in the claim shape `20260713`
  already defines.
- Serve OIDC discovery and JWKS from the control plane, so the community default
  needs no external IdP.
- Support key rotation without invalidating tokens already in flight.
- Issue signed ID tokens on the same key material, refreshed by the existing
  `SecurityTokenRefreshReconciler` rather than a second scheduler.
- Deliver the ID token into the sandbox runtime, refresh it in place, and remove it
  when the sandbox is paused or deleted.
- Leave the UUID path as the default, with `security.agents.kruise.io/enable-jwt-auth`
  as the opt-in boundary.

## Non-Goals

- Changing the verification contract in `20260713`, including its claim requirements
  and its one-shot JWKS snapshot.
- Authorization policy. This proposal mints and delivers credentials; deciding what a
  caller may do with one needs its own proposal.
- Replacing an external identity provider where one exists. Keycloak and Dex remain
  supported through the unchanged `RegisterProvider` seam.
- Token revocation infrastructure. See Open Questions.

## Implementation Status

Parts of this design already exist as code and can be reviewed against it rather than
taken on trust. #772 implements `pkg/identity/jwtissuer`: signing across RSA, ECDSA
P-256/384/521 and Ed25519 with the algorithm derived from the key type, the RSA
2048-bit floor, PEM loading for PKCS#8, PKCS#1 and SEC1, thumbprint key IDs, retained
keys for the rotation overlap window, and the discovery and JWKS endpoints mounted
under the issuer URL path. Its tests drive the real `pkg/identity/oidc` verifier, so
the claim shape and JWKS rules described below are already checked by the consumer
that enforces them.

What that PR deliberately does not do, because it depends on decisions recorded under
Open Questions or on wiring outside the package: register an `IdentityProvider`, serve
the endpoints from sandbox-manager, honour `TokenOptions.RequestedValidity`, mint ID
tokens, or propagate anything into a sandbox.

The sections below therefore describe a mix of shipped and proposed behaviour. Signing
key material, key rotation, and discovery and JWKS are implemented. Access token
issuance is partly implemented and partly wiring. ID token issuance and distribution
are proposed only.

## Signing Key Material

The issuer loads a PEM-encoded asymmetric private key. PKCS#8, PKCS#1 and SEC1 are
accepted, since all three are emitted by tooling operators already use. RSA, ECDSA
P-256/384/521 and Ed25519 are supported, with the JWS algorithm derived from the key
type so a key and its algorithm cannot be mismatched.

RSA keys below 2048 bits are refused at load. RFC 7518 section 3.3 requires 2048 for
RS256, and the verifier does not enforce it: `algorithmSupportsKey` pairs on key type
and curve size and never inspects the RSA modulus. The issuer is the only side that
can decline to create the problem.

The key ID is the RFC 7638 JWK thumbprint of the public key rather than a configured
string. This is a pure function of the key, which gives two properties worth having:
several sandbox-manager replicas loading the same Secret publish the same `kid`
without coordinating, and new key material necessarily produces a new `kid` instead
of silently reusing the old one.

The expected deployment is a Kubernetes Secret projected into the pod as a volume,
read by path. Callers holding the Secret in memory can pass the data value directly.

## Key Rotation

`20260713` makes automatic JWKS refresh a non-goal, and the gateway snapshots the key
set once per process: `loadUntilReady` publishes the verifier, flips state to `Ready`
and then blocks until its context is cancelled. Nothing rebuilds it afterwards.

That constrains rotation to one safe ordering:

1. Publish the new public key in the JWKS while still signing with the old key.
2. Roll the gateway fleet, so every process snapshots a key set containing both.
3. Flip the signing key to the new one.

Retaining the previous public key is what makes step 3 safe. Reversing steps 1 and 3
fails every gateway that has not restarted.

If the verifier later gains a refresh, retention stops being the only thing preventing
a fleet of 401s and becomes a window for verifiers that have not refreshed yet.
Retention is correct under both models, so this design does not depend on that
decision.

## OIDC Discovery and JWKS

Two endpoints are served:

- `/.well-known/openid-configuration`, carrying at minimum `issuer` and `jwks_uri`.
- `/jwks.json`, carrying the active signing key followed by any retained keys.

Both are public by design. They expose only public key material and the issuer
identity, which is what lets a gateway bootstrap without credentials.

The endpoints are mounted under the path component of the issuer URL. An issuer behind
an ingress path prefix otherwise advertises a `jwks_uri` that the mux does not serve,
the gateway's fetch 404s, and `loadUntilReady` retries forever without ever becoming
ready.

The issuer URL must be an absolute HTTPS URL. It becomes the `iss` claim, and the
verifier compares it against the issuer advertised by discovery, so the two cannot
diverge.

## Access Token Issuance

An in-tree provider registered through `RegisterProvider` mints a signed JWT for
`TokenKindAccessToken`, flowing through the existing `IssueSandboxAccessToken` path
and out on the E2B response. No CR persistence is added.

Claims follow `20260713`: `iss`, `sub`, `iat`, `nbf`, `exp`, and a `sandbox` claim
carrying `sandboxId` and `sandboxUid`. `nbf` is set equal to `iat` rather than
backdated, because the verifier's default clock skew is one minute, which already
absorbs a replica running slightly ahead of a gateway.

Validity is per issuance. `TokenOptions.RequestedValidity` is resolved by
sandbox-manager and passed through the provider, so the issuer honours the caller's
policy rather than a lifetime fixed at construction.

## ID Token Issuance

`TokenKindIDToken` is minted on the same key material and signing path.

Refresh is driven by the existing `SecurityTokenRefreshReconciler`. Its lead-time and
jitter scheduling is unchanged and no second scheduler is introduced. The reconciler
already resolves the runtime transport per refresh, before issuance, because the Pod
IP and the advertised capability can change across a pause and resume cycle.

## ID Token Distribution

`PropagateSecurityToken` writes the credential into the sandbox runtime. The transport
question is already settled: the call carries the transport the caller resolved, and
the propagator registry documents `WriteFileWithRuntime` and `ChmodFileOnRuntime` as
the delivery path.

Two details matter in the implementation.

`WriteFileArgs.Permissions` is not transmitted today. The runtime applies its own
default, so an exact mode requires a follow-up chmod. Between the write and the chmod
the credential exists at the runtime default, and a failed chmod must remove the file
rather than leave a token readable at a wider mode than intended.

Cleanup on pause and delete has no in-tree caller today: `Filesystem().Remove` is
unused outside tests. Whether a credential survives a pause also depends on the pause
strategy and on where the configured path lands, since a path inside the container
filesystem disappears with the Pod while a path on a mounted volume does not.

## Open Questions

These need a decision before the corresponding implementation lands. Each carries a
recommendation, not a settled answer.

### `aud`

Three readings currently coexist and they disagree.

The traffic access token carries no `aud`, and the merged verifier agrees: it
validates with `jwt.Expected{Issuer, Time}` and no audience, so go-jose ignores the
claim entirely. Separately, the sandbox ingress proposal requires `aud` to name the
target sandbox with envd rejecting a mismatch. Item 2 of the issuance scope asks for
`aud` on the ID token in the outbound sense.

Recommendation: the traffic access token keeps carrying none, because
`sandbox.sandboxId` already performs that binding and the verifier already enforces
it, and `aud` belongs to the ID token only. Adopting the ingress model instead changes
the `20260713` verification contract, which is a larger decision than issuance.

### `sub` semantics

`20260713` gives `e2b:controlplane:client` as its example and the verifier only
requires `sub` to be non-empty. Because the token is minted at claim time and returned
to whoever called the API, what it encodes today is a bearer capability scoped to one
sandbox rather than a caller identity.

Recommendation: carry the authenticated principal. The claiming API key is already on
the object at issuance time as `AnnotationOwner`, and the gateway already surfaces it
as `Route.Owner`, so the cost is near zero. Shipping a fixed control-plane subject
forecloses binding policy to a caller later without a breaking claim change.

### Where the issuer runs

An HTTPS listener inside sandbox-manager keeps the deployment footprint unchanged but
is multi-replica, so the signing key must be shared. A thumbprint-derived `kid` makes
one shared Secret workable without a coordinated publish path.

Recommendation: in-process in sandbox-manager, one Secret, unless per-replica key
isolation is wanted.

### Revocation on pause and delete

No `jti` is minted. For a short-lived bearer token bound to one sandbox this is
defensible, and adding one implies a revocation store nothing maintains. The
consequence is that a leaked token stays valid for the rest of its lifetime unless the
signing key is rotated and the retained key dropped.

Recommendation: no `jti` for now, with credential removal on pause and delete treated
as hygiene rather than as revocation. If revocation is required, it needs its own
proposal.

## Compatibility And Upgrade

The UUID path stays the default. `enable-jwt-auth` remains the per-sandbox opt-in and
no existing deployment changes behaviour by upgrading.

One rollout hazard is worth recording because it belongs to the merged verification
path rather than to issuance. `EnableJWTAuth` is process-wide while
`RequireTrafficAuth` is per sandbox, and the two mixed states fail in opposite
directions: enabling JWT mode on the gateway first leaves sandboxes that have not
opted in with the token header stripped and no constant-time comparison reached, while
annotating sandboxes first returns 503 until the gateway is flipped. A phased rollout
is therefore not currently expressible, and the cutover is per gateway process.

Deployments already registering an external provider are unaffected: `RegisterProvider`
is unchanged and the in-tree provider is used only when no other is registered.

## Risks

- A weak or mismanaged signing key compromises every token. Mitigated by refusing
  undersized RSA keys at load and by keeping private material out of every error path
  and log line.
- Rotation performed in the wrong order produces a fleet-wide outage. Mitigated by
  retained keys and by documenting the ordering as an operator procedure.
- The write-then-chmod window leaves a credential briefly at the runtime default mode.
  Mitigated by removing the file when the chmod fails.

## Test Plan

Issuance is verified against the real `pkg/identity/oidc` verifier rather than a mock:
an HTTPS test server publishes genuine discovery and JWKS documents, the CA is supplied
through a ConfigMap exactly as the gateway reads it, and the production verifier
validates the token. This is what keeps the claim shape and the JWKS rules honest,
because they are checked by the actual consumer.

Coverage includes every supported key type, the rotation overlap window including the
case a gateway holding a pre-rotation snapshot must reject, an issuer behind a path
prefix, and the assertion that private key material never appears in the published
JWKS.

End to end coverage runs claim through gateway verification to propagation and
refresh, built incrementally as each piece lands.

## Alternatives

**Require an external identity provider.** Keycloak or Dex can already be wired
through `RegisterProvider`. This is the right answer for deployments that have one,
but it leaves the community default unable to produce a token its own gateway accepts,
which is the gap this proposal exists to close. The two are complementary rather than
exclusive.

**Serve discovery and JWKS from a sidecar.** Avoids sharing a signing key across
sandbox-manager replicas, at the cost of a new component to deploy and operate.

## Implementation History

- 2026-09-13: initial draft.

Implementation is sequenced so each stage is testable against the real verifier before
the next depends on it: signing key material and the JWKS endpoint first, then access
token issuance, then ID token issuance and refresh, then propagation and cleanup, with
end to end coverage built alongside rather than deferred. See Implementation Status for
what exists today.
