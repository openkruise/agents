---
title: Shared Sandbox Provider
authors:
  - "@AiRanthem"
reviewers: []
creation-date: 2026-07-17
last-updated: 2026-07-17
status: provisional
see-also:
  - "/docs/proposals/20260715-sandbox-manager-state-model.md"
---

# Shared Sandbox Provider

## Summary

Introduce a responsibility-specific Sandbox provider shared by controller, sandbox-manager, and
Gateway. The provider centralizes reusable Sandbox CR mechanics without making controller depend on
manager or exposing Kubernetes CR details through manager business interfaces.

The change is primarily an architectural extraction. It preserves claim, pool, state, retry, and
error behavior while adding the conservative route baseline required for a safe state-model
rollout. That baseline prevents stale deletion resurrection and stops legacy producers from
describing a known non-serving Sandbox as running.

## Motivation

Reusable Sandbox CR behavior currently lives under sandbox-manager even though controller already
calls it. TryClaimSandbox and its options are the clearest example: controller imports manager
configuration, constants, infra contracts, and the manager CR implementation to reuse one claim
operation. Gateway separately converts the same Sandbox CR into route data.

These dependencies violate the repository boundary, make controller impossible to understand in
isolation, and encourage state and route decisions to drift. Moving the code without separating
mechanics from policy would merely relocate the coupling.

## Proposal

### Package responsibilities

The new sandboxprovider package owns implementation-neutral Sandbox capabilities, request and
result types, claim options and defaults, admission callbacks, resource descriptions, and claim
metrics. It does not own API protocol behavior, authorization, quota policy, or Manager defaults.

Its sandboxcr child owns reusable Sandbox CR algorithms. It wraps CR observations, derives state
and route through methods, provides pool predicates, materializes SandboxSet members, and executes
TryClaimSandbox through a narrow CR backend contract. It does not own concrete Kubernetes clients,
caches, queries, or writes.

The existing sandbox-manager infra package remains the Manager port. Its sandboxcr child retains
concrete Kubernetes clients, cache access, CR reads and writes, Manager-specific construction,
lifecycle, clone, checkpoint, volume, error mapping, and assembly. It implements the shared backend
contract while delegating reusable observation and claim orchestration to the new provider.

### State and route access

Raw Sandbox CR conversion to manager-oriented state stays private to the shared sandboxcr
implementation. Manager obtains state only through GetState. Gateway obtains a complete route only
through GetRoute. In this foundation change, GetRoute calls GetState and owns the current legacy
route projection plus a conservative serviceability filter. The later state-model change replaces
that projection with the single state-to-action mapping.

Gateway uses a read-only CR view that does not require cache or mutation dependencies. Manager's
mutable Sandbox adapter delegates its observation methods to the same view. No free CR-to-state or
CR-to-route function is exposed.

Controller does not consume the manager-oriented state. Pool creating and claimable checks are
separate pure Sandbox CR predicates owned by sandboxcr and reused by Controller, cache, claim, and
GetState. This keeps controller self-contained without making the shared package import controller.

Route data and actions remain in the existing neutral route model. No sandboxstate or sandboxroute
package is introduced.

### Claim options and policy

Common claim input, result, admission, update, runtime, CSI, resource, and metrics types move out of
manager infra into sandboxprovider. CR pointers, SandboxClaim metadata, CR metadata tracking, and
the narrow backend capability remain in sandboxcr claim options. Concrete cache/client types remain
inside the Manager or Controller adapter that implements that capability.

Defaults are explicit caller input. Manager and controller may select the same values, but the
shared implementation does not import Manager constants or silently apply API policy. Quota is
provided through admission callbacks, and Manager or controller maps provider errors at its own
boundary.

TryClaimSandbox retains candidate selection, speculative creation, create-on-no-stock, revision
preference, optimistic locking, admission acquire and release, readiness waits, runtime and CSI
initialization, identity processing, failed-Sandbox retention, cancellation, and retry behavior.

SandboxSet materialization and annotation keys needed by both paths move to the CR provider so it
does not import controller or E2B packages. Concrete reads, writes, waits, runtime calls, identity
processing, and cleanup persistence are invoked through the backend contract. Shared cache and
logging packages must not pull Manager configuration into the controller dependency closure.

### Dependency rules

Controller may depend on sandboxprovider and sandboxprovider/sandboxcr, but neither shared package
may depend on controller, sandbox-manager, or servers. Gateway may depend on the read-only CR
provider and neutral route model, never on Manager state or Manager infra. E2B continues to call
sandbox-manager rather than bypassing it for direct provider access.

Manager protocol and business policy remain above its infra port. Concrete CR I/O remains inside
the existing Manager infra/sandboxcr boundary, while the shared provider remains a pure CR
algorithm library and never exposes CR types to Manager core.

### Route compatibility baseline

The provider rollout also prepares the later state-model compatibility window. Route deletion keeps
a non-forwarding tombstone containing Sandbox identity and resource version. A route with the same
UID and an equal or older resource version cannot recreate the deleted entry; a newer version or a
new UID can proceed normally. Tombstones are not visible as active routes and never forward traffic.

Tombstones are supported by the receiver's authoritative local Sandbox observation. A receiver
does not accept peer routes or forward traffic until its Sandbox cache has synchronized. After a
restart it rebuilds current route decisions from that cache, rejects peer records that do not match
the current Sandbox identity and resource version or that weaken its current decision, and therefore
does not depend on lost in-memory history. A tombstone may be removed after the synchronized cache
proves that its UID is absent, replaced by a new UID, or represented by a newer active route; the
same validation continues to reject late records for the old decision.

The shared legacy GetRoute path also emits running only when the Sandbox satisfies every safety fact
required by the later Allow action. Resume initialization, in-place update, cleanup request,
reserved failure, missing address, and other known non-serving observations use a non-running legacy
value. This is a private wire-safety filter inside GetRoute, not a second state API. It deliberately
tightens legacy routing without changing its wire fields.

Every Manager, proxy, and Gateway producer and receiver must run this baseline before the
state-model starts producing Route actions. Versions older than this baseline are not supported in
the mixed-version window.

## Compatibility and Rollout

This extraction introduces no public API, CRD schema, wire-format, or lifecycle-classification
change. It intentionally rejects stale same-version resurrection after route deletion and changes
known unsafe legacy running routes to non-running. Existing callers migrate atomically, then
duplicate manager-owned option and helper definitions are removed so there is only one source of
truth.

The provider change lands before the state-model change. It can be rolled back independently until
the dependent change begins using the new state and route contracts.

## Risks and Verification

- A mechanical move could import Manager policy into the shared package. Import checks prohibit
  manager, server, and controller dependencies from the provider production closure.
- Manager and controller could resolve different defaults. Adapter tests compare fully resolved
  options and preserve existing behavior intentionally.
- A backend contract could leak concrete clients or Manager policy. Contract tests and import checks
  keep it CR-specific but implementation-neutral, with concrete I/O in component adapters.
- A read-only Gateway wrapper could accidentally expose mutating methods. The read-only view has no
  backend, cache, client, or mutation capability.
- Claim behavior could drift during extraction. Table-driven equivalence tests cover candidate,
  locking, admission, retry, cleanup, runtime, CSI, identity, and error paths.
- A deletion tombstone could block a legitimate replacement or grow without bound. It is scoped by
  UID and resource version, and authoritative-cache validation permits replacement and collection
  without admitting stale peer records.
- Two CR implementations could survive the migration. Manager's adapter must delegate shared
  behavior, and old duplicate options and helpers are removed after consumer migration.

## Alternatives

Keeping the code in sandbox-manager was rejected because controller would retain a forbidden
reverse dependency. Placing everything in one concrete sandboxcr package was rejected because
Manager interfaces and future providers would depend on Sandbox CR types. A generic utils or infra
package was rejected because its responsibility would be unclear and easily confused with the
Manager infra layer.

## Implementation History

- 2026-07-17: Proposed sandboxprovider as the neutral contract with sandboxcr as its Kubernetes
  implementation.
