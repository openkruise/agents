---
title: TLS Connection Actions for SecurityProfile
authors:
  - "@Kuromesi"
reviewers: []
creation-date: 2026-10-26
last-updated: 2026-10-26
status: provisional
---

# TLS Connection Actions for SecurityProfile

## Summary and delivery scope

Add `spec.rules[].actions.tls.action` with `Deny` and `Passthrough` to SecurityProfile and
GlobalSecurityProfile. Both use the same SecurityProfileSpec. Presence of
`actions.tls` identifies an explicit TLS connection rule; its absence keeps a
rule in the existing HTTP pipeline.

This change delivers the Go API, generated deepcopy and CRD schemas, an
[example](../../config/samples/agents_v1alpha1_securityprofile_tls.yaml), and
schema tests. Agentio must consume the API and implement connection selection
and enforcement before these policies can reject connections. Installing these
CRDs alone does not enforce TLS Deny or Passthrough.

The contract below adopts proposal A (declaration order). Proposal B (most
specific match) remains an alternative, not an implemented mode. No mode field
is added.

## API and validation

```yaml
rules:
  - name: deny-sensitive-tls
    match:
      - domains: [blocked.example.com]
        ports: [443, 8443]
    actions:
      tls:
        action: Deny
  - name: passthrough-private-tls
    match:
      - domains: [private.example.com]
    actions:
      tls:
        action: Passthrough
```

`action` is required, has no default, and accepts `Deny` or `Passthrough`. `tls: {}`, an
empty action, and unknown actions fail CRD schema validation. `Terminate` is an
internal connection-selection result, not a public action.

| Field | Explicit TLS rule | Ordinary HTTP rule |
| --- | --- | --- |
| domains | Required; matches visible SNI | SNI during connection selection, Host / authority during HTTP processing |
| ports | Original destination port | Original destination port during connection selection; existing HTTP port semantics during request processing |
| schemes | Unsupported | Only entries with no schemes or including https participate in TLS termination selection |
| paths, methods, headers, queryParams | Unsupported | Evaluated only during HTTP processing |

Domains and ports in the same match entry are ANDed; values within either list
are ORed. Match entries are ORed independently. For example, separate entries
for `a.example.com:443` and `b.example.com:8443` do not match
`a.example.com:8443`. Ports range from 1 through 65535. Omitted or empty ports
impose no restriction and do not default to 443.

Domain normalization, case handling, and wildcard scope retain Agentio's
existing SNI semantics. `*` matches any non-empty visible SNI, never absent SNI.
Invisible service names cannot be inferred from this policy.

TLS rules cannot combine TLS with block, bypass true, tokenTransformation,
mcpToolPolicy, headerManipulation, httpCallout, other HTTP execution actions,
or rule-level audit. `bypass: false` is equivalent to omission. Profile-level
audit applies only to HTTP rules. Unsupported TLS match fields are invalid,
even when explicitly supplied as empty lists.

**Admission limitation in this delivery:** cross-field CEL validation is
intentionally deferred. The CRD enforces the required action, action enum,
required non-empty domains list, and existing port constraints, but does not
reject mixed TLS/HTTP actions or HTTP-only match fields on TLS rules. Existing
rule and match lists remain unbounded. A consumer must reject an invalid
profile as a whole rather than silently ignore unsupported fields. Admission
enforcement of these cross-field constraints is follow-up work.

E2B Sandbox inline rules retain their existing action allowlist (block and
headerManipulation); this change does not make TLS actions available through
that entry point.

## Connection selection: proposal A

Preserve existing policy-source grouping, including administrator profiles
before Sandbox-owned rules. Within each group, merge global and namespaced
profiles in existing order: ascending priority, creation time, name, then
namespace. Within each profile, preserve rule and match declaration order.

Evaluate visible SNI and original destination port. The first matching entry
decides connection behavior:

- An explicit TLS Deny rule rejects the whole connection.
- An explicit TLS Passthrough rule forwards the connection without decryption
  or HTTP rule processing. No later TLS rules are evaluated.
- An ordinary HTTP rule selects Terminate if that match entry has no schemes
  or includes https. Paths, methods, headers, and query parameters do not limit
  this handshake-stage selection.
- Skip HTTP match entries whose schemes exclude https.
- No match preserves the existing default behavior.

An HTTP block or bypass action only selects termination at this stage; it is
not executed before decryption. After termination, each HTTP request starts
normal HTTP rule evaluation, including all applicable HTTP rules. The source
rule selected at the handshake neither restricts nor sets the starting point
of HTTP evaluation. Existing SNI/Host consistency checks remain. Explicit TLS
rules never execute in the HTTP phase.

Thus Deny for `blocked.example.com` followed by HTTP `*.example.com` denies
that host. Reversing them selects Terminate instead, even if the HTTP rule has
a `/health` path condition. There is no fixed Deny priority. Similarly, an
explicit Passthrough for `private.example.com` before HTTP `*.example.com`
keeps that connection encrypted; reversing them selects Terminate.

### Gateway exclusions

`tlsTermination.excludeHosts` forbids termination, not Deny. A selected Deny
still rejects an excluded host. A selected Passthrough always forwards without
decryption, regardless of exclusions. A selected Terminate for an excluded host
becomes passthrough without evaluating later TLS rules. No match preserves
existing default and exclusion behavior.

### Updates and compatibility

This contract covers TCP connections that begin directly with a TLS handshake.
Deny fails the connection without promising HTTP 403 or a specific TLS alert.
Updates affect subsequent connection evaluations; existing connections need
not be interrupted.

Existing HTTP request matching, accumulated actions, credential replacement,
and block/bypass behavior remain. Connection selection now respects HTTP
match ports: a rule restricted to port 443 no longer triggers termination for
the same SNI on port 8443. Other rules or default handling may still apply.

Consumers must retain the previous valid version when an accepted update
cannot compile, using their existing last-valid-version behavior. Without a
previous valid version, the profile must not be reported as effective. Never
apply only the valid subset of an invalid profile.

## Alternative: proposal B

The same API could select the most specific matching candidate within each
existing policy-source group: exact domain before suffix wildcard before `*`,
longer matching wildcard suffix first, then explicit ports before unrestricted
ports. Only actually matched domains contribute specificity. Port lists
`[443]` and `[443, 8443]` tie for a connection on 443. Domain specificity precedes
port specificity, and both precede profile ordering.

Ties across profiles use existing profile order. Equivalent candidates within
one profile with overlapping ports and different actions reject that profile
version; equal actions keep source order for attribution. Specificity never
crosses administrator/Sandbox source-group boundaries. Deny has no fixed
priority: exact Terminate or Passthrough could override wildcard Deny. This alternative and
its conflict checks are not part of this delivery.

## Non-goals

No TLS audit webhook, ALPN matching, certificate policy, upstream mTLS, QUIC,
STARTTLS detection, default-deny mode, or default decryption is introduced.
