---
title: Open-Source Signed Token Issuance and ID Token Distribution
authors:
  - "@HARSHRAJ2789"
reviewers:
  - "@TBD"
creation-date: 2026-08-10
last-updated: 2026-08-10
status: provisional
see-also:
  - "/docs/proposals/20260713-traffic-access-token-jwt-verification.md"
  - "/docs/proposals/20260427-security-identity-provider.md"
---

# Open-Source Signed Token Issuance and ID Token Distribution

## Summary

`20260713-traffic-access-token-jwt-verification.md` gave sandbox-gateway a complete
verifier and listed issuance as out of scope. Nothing has filled that gap. The community
default provider in `pkg/identity/common_provider.go` mints `uuid.NewString()` for both
token kinds, stamps the access token with a lifetime of `100 * 365 * 24 * time.Hour`, and
implements `PropagateSecurityToken` as a function that returns `nil`. A community
deployment therefore cannot produce a token its own gateway would accept.

This proposal describes an in-tree `IdentityProvider` that signs real JWTs with asymmetric
keys, an OIDC discovery and JWKS surface served by sandbox-manager so the gateway verifier
can bootstrap without an external identity provider, and a community propagator that
delivers the ID token into the sandbox as a credential file. It also describes how a
deployment that prefers Keycloak or Dex plugs in at the same seam.

The design adds no CRD fields and no changes to the `IdentityProvider` interface. Every
extension point it uses already exists: `RegisterProvider`, `RegisterSecurityTokenPropagator`,
the `TokenKind` selector from #671, and the transport plumbing from #734.

## Motivation

Three things are true at once, and together they define the work.

The verifier is finished and strict. `pkg/identity/oidc/verifier.go` requires `exp`, `iat`,
`nbf`, an `iss` that exactly matches the discovery document, a non-empty `sub`, and a nested
`sandbox` object whose `sandboxId` and `sandboxUid` the gateway filter compares against
`Route.ID` and `Route.UID`. It accepts RS256/384/512, PS256/384/512, ES256/384/512 and
EdDSA (`verifier.go:39-43`), rejects a JWK with an empty or duplicate `kid`, and rejects a
token whose header `alg` disagrees with the JWK.

The issuance side is a placeholder. `defaultTokenProvider.IssueToken` returns three UUIDs
and a timestamp. Nothing about it satisfies the verifier.

And there is already a working reference, in the test tree.
`test/e2b/assets/jwt-oidc-provider/main.go` serves `/.well-known/openid-configuration` and
`/jwks` over TLS and mints tokens the verifier accepts. It is 200 lines, RSA only, with
`{"alg":"RS256","kid":keyID}` hardcoded and a single static key.
`.github/workflows/e2e-sandbox-gateway-security.yaml` builds it and runs
`test/e2b/test_gateway_jwt_auth.py` against it.

So the community story today is: the data plane can verify, CI can prove the data plane
verifies against a test double, and no shipped code can mint. The gap between the fixture
and a production issuer is the substance of this proposal.

### Goals

- Sign access tokens and ID tokens with an asymmetric key held in a Kubernetes Secret.
- Serve OIDC discovery and JWKS from sandbox-manager so the gateway bootstraps with no
  external identity provider.
- Support key rotation through a publish, roll, flip overlap window that stays inside the
  verifier's existing contract.
- Deliver the ID token into the sandbox over the runtime transport the caller already
  resolved, refresh it in place, and remove it on pause and delete.
- Keep Keycloak and Dex viable by leaving `RegisterProvider` and the discovery
  configuration as the integration seam.
- Change no CRD schema and no API contract.

### Non-Goals

- Audience or subject-policy authorization. `20260713` defers this to a separate proposal
  and that boundary is preserved here.
- Automatic JWKS refresh on the request path. `20260713` states that a published verifier
  is never replaced for the process lifetime; rotation is handled by rollout, not refetch.
- Token revocation and introspection.
- Replacing the UUID path. It stays the default.
- Egress credential issuance, which `20260427-security-identity-provider.md` owns.

## Behavioral Contract

Two token kinds, two lifecycles, one provider.

**Access token, `TokenKindAccessToken`.** Minted during claim and clone when
`IsAccessTokenRequested` is true, which requires
`security.agents.kruise.io/enable-jwt-auth` to equal exactly `"true"`
(`sandbox_token_helper.go:63-68`). It travels through `IssueSandboxAccessToken` into the
transient `GetTrafficAccessToken` field and out on the E2B response. It is never persisted
to the CR.

**ID token, `TokenKindIDToken`.** Minted when `IsIDTokenRequested` is true, which requires
a non-empty `security.agents.kruise.io/agent-name` annotation. It runs the full
`ProcessSandboxToken` lifecycle: issue, propagate, then record the expiration into the
`token-status` annotation, in that order, with the expiration recorded only after
propagation succeeds (`sandbox_token_helper.go:234-255`). `SecurityTokenRefreshReconciler`
re-arms from that annotation.

The provider is registered once during `init()`. Both registries are documented as unsafe
for runtime mutation (`pkg/identity/AGENTS.md`), so key rotation must happen inside the
provider, never by re-registering it.

## Token Claims

The access token uses the shape `20260713` already defines, because the verifier enforces
it:

```json
{
  "iss": "https://sandbox-manager.sandbox-system.svc:8443",
  "sub": "e2b:controlplane:client",
  "exp": 1760000000,
  "iat": 1759996400,
  "nbf": 1759996400,
  "sandbox": {
    "sandboxId": "default--sample",
    "sandboxUid": "89d24507-936c-4a04-a958-b5d6a8277ed5"
  }
}
```

`sandboxId` and `sandboxUid` come from the `SandboxInfo` projection the provider derives
from the sandbox object (`types.go:91-106`). The provider owns that projection by design:
`IssueToken` receives the sandbox verbatim rather than a pre-built `TokenRequest`
(`interface.go:46-52`).

**On `sub`.** The verifier requires only that it is non-empty, and `20260713`'s example is
the fixed string `e2b:controlplane:client`. That makes the token a bearer capability scoped
to one sandbox. It says nothing about who the caller is. This proposal recommends carrying
the authenticated principal from the API key that performed the claim, because a fixed
subject forecloses the RBAC work the issue title gestures at without a breaking claim change
later. It is flagged as a question for the maintainers, and the ordering matters: this is
the one decision that is expensive to reverse.

The ID token carries `iss`, `sub`, `aud` and the standard time claims, and is not verified
by the gateway. Its consumer is whatever external service the agent authenticates to, so
its `aud` belongs to deployment configuration. The next section explains why that sentence
is the one that has to be settled before item 2 of #659 can be written by anyone.

## The `aud` Decision, and Why Item 2 Is Blocked On It

Item 2 of #659 asks for an ID token carrying `iss`, `sub`, `aud`, `exp` and `iat`, "suitable
for sandbox-to-external-service authentication". `aud` is the only one of those five that is
currently claimed by two designs that mean different things by it, and the merged code reads
none of them.

**What the merged verifier does with `aud` today: nothing.** `pkg/identity/oidc` validates with

```go
claims.Claims.ValidateWithLeeway(jwt.Expected{Issuer: v.issuer, Time: time.Now()}, v.clockSkew)
```

`Expected` carries no `AnyAudience`, and go-jose checks the audience only when it does
(`jwt/validation.go`: `if len(e.AnyAudience) != 0`). The string `aud` does not appear anywhere
in `pkg/identity` outside tests. A token with any audience, or none at all, verifies
identically. #772 is consistent with that: it populates `Issuer`, `Subject`, `IssuedAt`,
`NotBefore` and `Expiry`, and leaves `Audience` unset, so the claim is omitted.

**Three readings are in flight.**

| Source | What `aud` names | Direction | Enforced where |
|---|---|---|---|
| #659 item 2 | the external service the agent calls | outbound | the external service |
| #697 | the target sandbox, as a SPIFFE ID | inbound | envd, rejecting non-matching `aud` |
| this document, above | deployment configuration | outbound | the external service |

The first and third agree. The second is the opposite direction: it uses `aud` to name the
sandbox as a resource server and rejects any token whose audience does not contain it.

**Why the direction matters more than the wording.** `aud` identifies the party expected to
*accept* a token. Under item 2 that party is outside the cluster and the sandbox is the
presenter. Under #697 that party is the sandbox and the caller is the presenter. Those are not
two conventions for one field; they are two fields sharing a name. Any code that validates
`aud` without knowing which token kind it holds is wrong for one of them, and `TokenKind`
already exists precisely to keep the two apart.

**This proposal's position, offered so the question has a concrete answer to argue with.**

1. **The ID token's `aud` is the external service**, per item 2, configured per deployment.
   That is the reading the RFC 7519 definition supports and the only one an external
   relying party can act on.
2. **The traffic access token should not carry `aud` at all.** Sandbox scoping is already
   done, by the `sandbox.sandboxId` and `sandbox.sandboxUid` claims that the merged verifier
   binds to the route. #697's SPIFFE ID in `aud` duplicates a binding that exists and works,
   and it does so in a field whose other user means the reverse.
3. **If #697's ingress authorization needs a claim of its own**, a distinct name costs one
   line and removes the ambiguity permanently. `sandbox` is already the precedent for a
   namespaced claim in this codebase.

**What is at stake if this is decided late.** Item 2 cannot be implemented without choosing,
because the choice determines whether the ID token issuer takes an audience from configuration
or derives it from the sandbox. Changing it afterwards is a breaking claim change for every
external service already validating the token, which is the one class of change this design
has no migration story for. The `sub` question raised earlier in this document has the same
property, and the two should be settled together.

I do not think this is mine to decide. It is written down here because item 2 is the last
unimplemented part of #659's scope and this is the thing standing in front of it.

## Key Material and Rotation

**Storage.** One Secret in the sandbox-manager namespace holding a PEM private key and its
`kid`. sandbox-manager is multi-replica, so every replica loads the same Secret and the
JWKS is identical everywhere. Per-replica keys with a union JWKS would need a shared publish
path and buy nothing here.

**Algorithms.** ECDSA P-256 as the default, with RSA and Ed25519 supported. The signing
algorithm is derived from the key type rather than configured separately, which is the only
way to guarantee the `algorithmSupportsKey` check at `verifier.go:306-324` cannot fail at
runtime on a valid key.

**`kid` derivation.** A stable function of the public key, so a restart that reloads the
same Secret publishes the same `kid` and existing tokens keep verifying. Deriving `kid`
randomly at startup would break every outstanding token on every rollout.

**Rotation.** The verifier snapshots JWKS once and never refetches, and `20260713` lists
automatic refresh as a non-goal. Given that, rotation has exactly one safe shape:

1. Add the new key to the Secret. Both keys are published in JWKS. Signing continues with
   the old key.
2. Roll sandbox-gateway. Every process now holds a snapshot containing both keys.
3. Flip the signing key to the new one.
4. After the longest outstanding token lifetime, remove the old key and roll again.

The hazard between steps 1 and 3 is real and worth stating: a gateway process that has not
been rolled holds a pre-rotation snapshot and rejects a new-key token with
`token references unknown kid` (`verifier.go:196`). That is why step 2 cannot be skipped,
and why this procedure belongs in operator documentation as well as in code.

Refetching JWKS on an unknown `kid` would remove the rollout step, at the cost of network
I/O on the request path and the "offline after initialization" goal. That is a change to the
verification contract and belongs in its own proposal.

## Discovery Endpoint Placement

The endpoint lives in sandbox-manager, on a dedicated HTTPS listener, serving
`/.well-known/openid-configuration` and the JWKS document.

This follows the repository's layering rules. Root `AGENTS.md`
puts routing, transport protocols and request models in the API layer (`pkg/servers/**`),
and confines concrete Kubernetes clients and Secret reads to Infra
(`pkg/sandbox-manager/infra/**`). So the HTTP surface is API-layer code, the Secret read is
an Infra capability exposed upward as a neutral interface, and the signing logic sits in
`pkg/identity` where the provider registry already lives. No API-to-Infra shortcut, and
`pkg/features` is not touched, since it is controller-only.

A separate identity component was considered and is discussed under Alternatives.

### Path mounting

The advertised `jwks_uri` and the path the mux actually serves must agree, and there is a
deployment shape where the obvious implementation makes them disagree.

`NewVerifier` validates `discovery.JWKSURI` as an absolute HTTPS URL and then fetches that
URL verbatim (`verifier.go:121-126`). It performs no relative resolution and never joins the
path against the issuer. So an issuer that composes `jwks_uri` as `issuer + "/jwks"` while
mounting its handlers at the root is correct only when the issuer URL has an empty path.
Put the same listener behind an ingress path prefix, for example
`https://host/identity`, and the discovery document advertises
`https://host/identity/jwks` while the mux answers only at `/jwks`. The advertised URL
404s, `fetch OIDC JWKS` fails, and because `loadUntilReady` retries indefinitely the gateway
never becomes ready and never surfaces a clearer reason than a repeated fetch error.

The implementation therefore derives both handler paths from the issuer URL's own path
rather than mounting at the root, so the advertised URL and the served path are the same
string by construction. A startup self-check that fetches its own discovery document and
JWKS through the advertised URLs turns this class of misconfiguration into a fast failure
at boot instead of a gateway that silently never becomes ready.

@AnshulPatil2005 hit this while building #772 and reported it on this proposal; the fix
there is the same one described here. Reproduced against the real verifier: a discovery
document advertising a root `jwks_uri` while the mux serves under `/identity` fails with
`fetch OIDC JWKS: unexpected HTTP status 404 Not Found`.

### Bootstrap ordering

Serving discovery from sandbox-manager makes gateway readiness depend on sandbox-manager
being up. That dependency is already survivable. `Manager.loadUntilReady`
(`pkg/sandbox-gateway/jwtauth/manager.go:232-267`) retries the verifier load indefinitely
with exponential backoff from one second to a thirty-second cap, and only transitions to
`Ready` once a non-nil verifier is published. So a gateway starting against an unavailable
sandbox-manager stays unready and keeps trying.

Two consequences worth stating. On a cold cluster start, gateway readiness lags
sandbox-manager by up to the backoff cap, which is expected. And because the snapshot is taken per process and then held for the
process lifetime (the loop blocks on `<-ctx.Done()` after publishing), a sandbox-manager
restart does not disturb a gateway that has already loaded; it only delays a gateway that
has not.

@H3xKatana raised this ordering question on #659 on 10 August, asking whether the gateway
retries or whether the snapshot survives a restart. The answer is the first: it retries, and
the snapshot does not need to survive because it is never refetched.

## Serving CA Distribution

The gateway verifier reads its trust anchor from a ConfigMap named by
`OIDC_CA_CONFIGMAP_NAMESPACE`, `OIDC_CA_CONFIGMAP_NAME` and `OIDC_CA_CONFIGMAP_KEY`, with
the key defaulting to `ca.crt` (`pkg/identity/oidc/options.go:28-40`). Separately,
`pkg/identity` already has a CA distribution mechanism: `CABundleSpec` treats a Secret in
`sandbox-system` as authoritative, copies it into the target namespace, and mounts it into
selected sandbox containers (`ca_cert_spec.go:44-57`).

These are two paths to the same kind of material, and a third would be one too many.

Two options, and this is the section most in need of a maintainer decision:

1. **Reuse the existing bundle registry.** Register the issuer's serving CA through
   `CABundleSpec` so `sandbox-system` stays the single authoritative source, and
   additionally project it into the ConfigMap the gateway's `OIDC_CA_CONFIGMAP_*`
   variables already point at. One source of truth, one extra projection.
2. **Keep them independent.** Populate the gateway's ConfigMap separately, for example
   from cert-manager, and leave `CABundleSpec` to the sandbox-facing bundles it serves
   today.

This proposal recommends the first, because the alternative leaves two mechanisms that
must agree without anything enforcing that they do. It is recorded as a recommendation
instead of a decision: @PRAteek-singHWY raised it as question 4 on #659 and it has not
been answered, and the answer changes what the implementation registers at startup.

## Token Lifetime and Re-Issue

A signed token with a real `exp` creates a problem the UUID placeholder hid. The access
token is minted once at claim, returned on the E2B response, and never persisted. #648
lists a manager API returning traffic tokens as a non-goal, and
`SecurityTokenRefreshReconciler` drives only the ID token path. So a sandbox that outlives
its access token has no recovery path.

Draft PR #742 by @chengzhycn addresses exactly this, adding refresh endpoints and a fresh
token on connect, making validity a sandbox-manager policy with a one hour default and a
twenty-four hour cap. It does not reference #659. @AnshulPatil2005 pointed this out on
#659 on 9 August.

This proposal therefore does not redesign lifetime. It states the dependency: the in-tree
issuer accepts a requested lifetime from the caller rather than choosing one, so that
whatever #742 settles governs. If #742 does not land, the fallback is a lifetime pinned to
the sandbox's maximum lifetime, which is safe only for short-lived sandboxes and should be
documented as such.

## ID Token Distribution

#734 already chose the channel. `PropagateSecurityToken` takes
`rtOpts ...agentsruntime.Option`, and `interface.go:58-65` requires an implementation that
touches the runtime to forward it so propagation rides the same transport as the rest of the
flow. `security_token_propagator.go:36-45` names `WriteFileWithRuntime` and
`ChmodFileOnRuntime` as the calls, and `client.go:98-101` exposes `Filesystem()` and
`Process()` as capability groups.

So the community propagator is registered through `RegisterSecurityTokenPropagator` during
`init()` and:

1. Writes the ID token to a credential path inside the sandbox using `Filesystem().Write`,
   forwarding `rtOpts` verbatim.
2. Applies a restrictive mode with `Process().Chmod`, since `WriteFileArgs.Permissions`
   does not travel in a header the runtime honours today. The section below states what
   that costs and what removing the second call is waiting on.
3. Runs again on refresh rather than only at claim. `refresher.go:87-97` already resolves
   the transport per refresh, before issuance, because the Pod IP and the advertised
   capability change across pause and resume.

**Cleanup is implemented, and it did not wait for hooks.** An earlier draft of this document
left removal as an open half depending on #743, whose `prePause` and `preTerminate` hooks
looked like the natural seam. #786 implements it directly instead, and the reason is worth
stating because it decides where this belongs for anyone extending it.

The blocker named here was the hard-coded five-second grace period on the pause delete
(`util.go:113`, `GracePeriodSeconds: 5`), which makes any cleanup racing pod termination
unreliable. #786 removes the race: the call runs *before* the delete is
issued, while the runtime is still reachable, so the grace period never applies. `prePause`
would place it at the same point, which is why the hooks are useful for other work and are not
a prerequisite for this one.

Three lifecycle events end a credential's validity, and they disagree on failure on purpose.
Recycle and pause stop, because the sandbox survives and a credential that outlives its claim
is the failure this exists to prevent. Delete logs and continues, because wedging a sandbox
behind its finalizer on a runtime that may never answer is worse than a file on a Pod being
destroyed. That asymmetry is the design decision; a hook framework would still need it
supplied.

Removal is attempted three times with a one-second backoff, which bounds the latency this adds
to a pause while absorbing a transient runtime failure. Beyond that the event's own policy
applies.

**What remains open** is the propagator itself, not the removal. Cleanup runs through a
registered `SecurityTokenCleaner`, the community default registers none, and #787 supplies the
file-based implementation. Until a cleaner is registered the call is inert by construction,
which is the correct behaviour for a community deployment that never propagated anything.

## The Credential File Mode, and What It Is Waiting On

Step 2 above applies the mode with a second call. That is a workaround, and the pieces needed
to remove it are already written and scattered across three places that do not reference each
other.

`WriteFileArgs` carries a `Permissions` field today. `filesystem.go:83-89` documents it as
**not transmitted**, and says it "becomes effective without a call-site change once the runtime
honors an explicit file-mode header". `process.go:208-209` calls `ChmodFileOnRuntime` "a temporary
measure to enforce file permissions until the agent-runtime (envd) natively honors the
X-File-Mode header". So the control plane already declares the workaround temporary and names
the header that ends it.

The client half exists. **#485** adds `HeaderFileMode = "X-File-Mode"` and sends the mode as a
four-digit octal string on the multipart upload, in sixty-nine lines. It has been open since
1 June with no review.

The runtime half is absent from the design. **#669** specifies the agent-runtime's HTTP
surface, and its `POST /files` row lists multipart and octet-stream bodies, gzip encoding,
parent-directory creation, and ownership by resolved user. It does not mention mode. The
`0600` and `0700` values elsewhere in that document are the helper socket and its directory.

**What this costs, concretely.** A credential written without a mode lands at the runtime
default, which `filesystem.go:84-85` gives as 0644 derived from umask. Between the write
returning and the chmod landing it is readable by anything else in the sandbox. #787 bounds
the failure by removing the file when the chmod fails, so a credential that could not be
protected is not left behind. The window itself only closes from the runtime side.

**The position this proposal takes.** The mode belongs on the write. `POST /files` already
resolves a user and applies ownership, so applying a mode extends that path, and #485 shows the
client change is small. Until that lands the two-call sequence
stays, and it should stay documented as temporary in both places that currently say so.

This is a sequencing question, not a disagreement. The client change, the control-plane
comments, and the runtime specification are three artifacts that each assume one of the others
has settled it.

## Integrating Keycloak and Dex

The seam is unchanged. A deployment pointing at Keycloak registers its own
`IdentityProvider` through `RegisterProvider` during `init()` and sets
`OIDC_DISCOVERY_URL` to the Keycloak realm. The gateway does not care which issuer it is
talking to, only that discovery and JWKS satisfy the rules at `verifier.go:257-295`.

What this proposal adds for those users is not a Keycloak client. It is the guarantee that
the claim shape, the rotation procedure and the propagation path are documented and
exercised by tests, so a Keycloak realm can be configured to match rather than reverse
engineered from the verifier source.

The one sharp edge: the gateway supports a single issuer per process, so moving between the
in-tree issuer and an external one is a flag day requiring a rollout. That belongs in the
operator documentation.

## Compatibility and Upgrade

The UUID path stays the default. Nothing changes for a deployment that does not set
`enable-jwt-auth`, and `IsAccessTokenRequested` remains a strict equality test against
`"true"` so `"1"` and `"True"` stay out of the issuance path.

The in-tree issuer is selected by configuration, not by replacing `defaultTokenProvider`
unconditionally. A deployment that already registers an enterprise provider through
`RegisterProvider` is unaffected.

### The annotation is not a gradual rollout control

`enable-jwt-auth` appears twice with different scopes, and the interaction between them
means neither ordering of a phased migration is safe. This section previously described
enabling the annotation sandbox by sandbox as a gradual rollout. That is wrong, and the
correction matters more than the issuance design it sits next to.

The gateway filter config carries process-wide `EnableAuth` and `EnableJWTAuth` flags,
with `Validate()` requiring `EnableAuth` whenever `EnableJWTAuth` is set
(`filter/config.go:58-62`, `:82`). Separately, each route carries `RequireTrafficAuth`,
derived per sandbox from the annotation (`sandboxroute/route.go:115`, field at `:43`). `authenticate()`
(`filter/filter.go:157-187`) combines them:

| Gateway `EnableJWTAuth` | Sandbox annotation | Behaviour |
|---|---|---|
| off | off | UUID constant-time compare, when `EnableAuth` is set and the route has a token |
| off | **on** | **503** `jwt_verifier_not_ready` (`filter.go:159-160`, reply built at `:226-236`) |
| on | on | JWT verification |
| on | **off** | **Traffic token header deleted, `Continue` returned** (`filter.go:164-167`) |

The two mixed states are the problem, in opposite directions.

Annotating sandboxes before flipping the gateway takes each one offline as it is annotated:
the route now requires traffic auth, the gateway has no verifier, and every request gets a
503 until the flip.

Flipping the gateway before annotating is worse, because it fails open rather than closed.
A route without `RequireTrafficAuth` never reaches the constant-time compare at
`filter.go:175`. Its traffic token header is stripped and the request continues
unauthenticated. Sandboxes that were UUID-authenticated a moment earlier are not
authenticated at all, and nothing in the request path logs it.

So the migration is a per-process cutover, not a per-sandbox one. The supported sequence is
to prepare issuance first (signing Secret, discovery listener, `OIDC_DISCOVERY_URL` and the
CA ConfigMap), then move a gateway and the sandboxes routed through it together, either by
annotating and flipping in one operation or by standing up a second gateway fleet already in
JWT mode and shifting routes onto it. Rollback carries the same constraint in reverse.

This proposal does not change that behaviour, which belongs to #648 as merged. It documents
it, because an operator following the earlier version of this section would have created
either an outage or an authentication gap. Whether `authenticate()` should instead fall back
to the UUID compare when JWT mode is on and a route has not opted in is a question for
@furykerry and @zmberg; it would make a phased rollout possible and is a change to the
verification contract rather than to issuance.

Reported by @AnshulPatil2005 on #659.

## Test Plan

**Unit, `pkg/identity`.** Mint a token and run it through the real `pkg/identity/oidc`
verifier rather than a mock: stand up an httptest TLS server publishing discovery and JWKS,
give `oidc.NewVerifier` the CA through a fake-client ConfigMap, and let it perform discovery
the way the gateway does. Negative cases must actually fail: drop `nbf`, mismatch `iss`,
change `kid`, present a JWK with `use` set to something other than `sig`, present two JWKs
sharing a `kid`, and present `key_ops` that does not permit verify.

I built that harness against `5d378bf` to check this proposal's own claims. Sixteen
assertions, all passing. Two results are load-bearing here. An **ES256**
token in the `20260713` claim shape verifies end to end, which is the evidence behind
recommending ECDSA P-256 as the default and is something the RSA-only CI fixture does not
demonstrate. And a verifier holding a pre-rotation JWKS snapshot rejects a new-key token with
`token references unknown kid`, while the same verifier built from a two-key snapshot accepts
both, which is the rotation procedure above reduced to a test.

@AnshulPatil2005's draft #772 already does this in-tree in `pkg/identity/jwtissuer`,
including a rotation overlap test. This proposal treats that as the reference for the unit
layer; my harness is scratch and stays out of the tree.

**Unit, propagation.** Assert `rtOpts` reaches the runtime client unmodified, that a refresh
rewrites the credential rather than skipping it, and that a propagation failure prevents the
expiration annotation from being recorded, which is the invariant `pkg/identity/AGENTS.md`
states.

**E2E.** Point `test/e2b/test_gateway_jwt_auth.py` at the in-tree issuer instead of
`test/e2b/assets/jwt-oidc-provider`. That closes the loop CI currently leaves open, where
the verifier is proved against a test double. Cover claim, gateway verification, propagation,
refresh, and rejection after expiry. Extend the algorithm coverage beyond RS256 while the
fixture is being replaced.

**Coverage floor.** The subsystem currently sits at 95.4% for `pkg/identity`, 99.5% for
`pkg/identity/oidc` and 93.0% for `securitytokenrefresh`. New code should not lower those.

## Risks

**A signed token with a real expiry is a behaviour change for anyone who was relying on the
hundred-year placeholder.** Mitigated by the opt-in annotation, and by the lifetime coming
from the caller.

**Rotation without the gateway rollout produces a silent authentication outage.** Mitigated
by documentation and by making the overlap window the only supported procedure. Not
mitigated by code, which is a weakness worth stating.

**A shared signing Secret is a single high-value credential.** Mitigated by RBAC scoping the
read to sandbox-manager and by never logging key material, which `pkg/identity/AGENTS.md`
already requires.

**The propagated credential is a file inside an untrusted sandbox.** It is scoped to that
sandbox and short-lived, but anything running in the sandbox can read it. That is inherent
to the delivery model, and it is why cleanup on pause matters.

## Alternatives

**A standalone identity component.** Cleaner isolation of the signing key, at the cost of a
new deployable, new RBAC, and a new failure domain in the bootstrap path. Rejected for the
first iteration because the operational cost lands on every community user, including those
who never enable JWT auth.

**Promoting `test/e2b/assets/jwt-oidc-provider` directly.** Tempting, since it works. It is
RSA-only, single-key, signs with `rsa.SignPKCS1v15` by hand, and reads its key from a file
path. Each of those is a deliberate simplification for a test fixture and a defect in a
shipped component.

**Environment variable injection for the ID token.** Cannot be updated without restarting
the sandbox, and #734 has already routed propagation through the runtime transport, so this
would be a second delivery path next to the chosen one.

**JWKS refetch on unknown `kid`.** Removes the rollout step from rotation. Contradicts the
offline-after-initialization goal in `20260713` and changes the verification contract, so it
needs its own proposal.

## Implementation History

- 2026-07-17: #659 opened by @furykerry
- 2026-07-21: #671 merged, `TokenKind`-based issuance interface
- 2026-07-23: #648 merged, gateway JWT verification via OIDC
- 2026-07-25: #659 scope refined after both merges
- 2026-08-07: #734 merged, security tokens delivered over the resolved runtime transport
- 2026-08-09: #772 opened as a draft signed JWT issuer
- 2026-08-10: this proposal opened
- 2026-08-10: path-mounting failure mode added, reported by @AnshulPatil2005 from #772
- 2026-08-13: the `aud` collision with #697 added, with the three readings of item 2
- 2026-08-14: cleanup restated as implemented in #786; credential file mode added, tracing
  #485 and #669 against the two control-plane comments that name `X-File-Mode`
