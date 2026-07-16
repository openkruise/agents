---
title: Precise Sandbox Lifecycle States
authors:
  - "@AiRanthem"
reviewers: []
creation-date: 2026-07-15
last-updated: 2026-07-16
status: provisional
see-also:
  - "/docs/proposals/20260711-short-sandbox-id.md"
  - "/docs/specs/2026-07-10-short-sandbox-id-design.md"
---

# Precise Sandbox Lifecycle States

## Summary

Replace the derived five-state model (`creating`, `available`, `running`, `paused`, `dead`) with an
eleven-state internal lifecycle, without adding a persisted CRD field. The change is split into three
ordered steps: define and prove the model, activate it through the short-ID routing foundation, then
atomically migrate all remaining consumers and add reasoned E2B 404 responses.

## Motivation

The current `dead` value conflates normal completion, failure, controller progress, and temporary
unready observations. It causes false 404s and premature route deletion, while manager code must
reconstruct pause, resume, and upgrade progress from raw CRD conditions.

The new model must distinguish real absence from temporary unavailability, preserve healthy traffic
after a proven pre-mutation update failure, and remain compatible with the approved short-ID design.

## Proposal

### Internal model

`pkg/utils/lifecycle` owns a pure function `(Sandbox, now) -> (state, non-empty reason)`. It reads no
clock or external system, returns no `unknown`, and covers every declared phase. First match wins:

| Priority | State | Decisive facts |
|---:|---|---|
| 1 | `terminating` | deletion timestamp, shutdown time at or before `now`, or `Terminating` phase |
| 2 | `succeeded` | `Succeeded` phase |
| 3 | `failed` | `Failed` phase |
| 4 | `recycling` | `Recycling` phase, or cleanup and cleanup-enabled annotations both true |
| 5 | `pausing` | pause requested from Running, or Paused phase not yet confirmed |
| 6 | `paused` | Paused phase, pause requested, and Paused condition true |
| 7 | `resuming` | Resuming phase, completed pause released, or post-resume initialization incomplete |
| 8 | `upgrading` | Upgrading phase, or active/unsafe in-place update while Running |
| 9 | `available` | unclaimed SandboxSet member is Running, Ready, and has Pod IP |
| 10 | `running` | claimed or standalone Sandbox is Running, Ready, and has Pod IP |
| 11 | `creating` | empty, pending, unsupported, unready, or otherwise unusable observation |

`SandboxResumed=True` implies `resuming` only when `RuntimeInitialized` exists and is not true.
Persistent `InplaceUpdate=True/Succeeded` and `Upgrading=True/Succeeded` do not imply active upgrade.
Only `status.phase=Failed` produces global `failed`. Local derivation never returns `dead`.

In-place failures expose mutation stage rather than error text:

| Condition reason | Evidence | Serving behavior |
|---|---|---|
| `PreUpdateFailed` | no Pod write was accepted | fall through to Ready and Pod-IP checks |
| `UpdatePodFailed` | mutation may be partial or acceptance is uncertain | `upgrading`, retry |
| `PostUpdateFailed` | submitted mutation failed to converge | `upgrading`, terminal attempt |
| legacy `Failed` | historical stage unknown | conservatively `upgrading` |
| `Succeeded` | completed | does not trigger `upgrading` |

If Ready was cleared before a proven `PreUpdateFailed`, the Controller re-reads the Pod and
re-synchronizes Ready; it never forces Ready true. New Controller code stops writing legacy `Failed`.

### Ordered implementation

```text
1-define-sandbox-lifecycle-state-model -----------+
                                                   +--> 2-prepare-sandbox-lifecycle-infrastructure
short-ID routing/cache/infra foundation ----------+                          |
                                                                              v
                                            3-adopt-sandbox-lifecycle-state-model
```

Step 1 and short-ID may proceed independently. Step 2 starts only after both are verified. Step 2 is
the first production data-plane activation; Step 3 is the atomic consumer and public-API cutover.

#### Step 1: define and prove

- Add the eleven-state vocabulary, explicit-time derivation, exact precedence, and exhaustive tests.
- Add `PreUpdateFailed`, `UpdatePodFailed`, and `PostUpdateFailed` Controller reasons and explicit
  mutation outcomes, including Ready re-synchronization tests.
- Keep `pkg/utils.GetSandboxState` and all production consumers behaviorally unchanged.

#### Step 2: prepare lookup and routing

Short-ID remains authoritative for ID resolution, `Route`, `Projector`, ObjectKey/UID/RV-fenced
`Store`, conditional peer mutations, collision quarantine, targeted `Repairer`, and feeder ownership.
Lifecycle supplies only normalized state/reason and component disposition.

Claimed-Sandbox lookup becomes route-independent and treats IDs as opaque:

- zero, one, and multiple indexed matches remain distinct;
- clean repeated zero results become absence only after a bounded propagation window;
- a Route is cache-lag evidence, never existence, ownership, or a Sandbox result;
- `APIReader.Get` is allowed only after a cache hit supplies ObjectKey and proves staleness;
- pure miss never parses `--`, reconstructs ObjectKey, or Lists all Sandboxes;
- ambiguity, timeout, cache, APIReader, and transport failures remain non-404.

The manager path is one-way:

```text
E2B -> SandboxManager -> infra.Sandbox normalized contract
    -> sandboxcr -> api/v1alpha1 + pkg/utils/lifecycle
```

`sandboxcr` alone interprets CR phase, conditions, lifecycle metadata, owner, and Controller reasons.
Infra exposes normalized lifecycle, owner, recycle eligibility, reserved-failure visibility, and
neutral Pod IP/object metadata. It exposes neither Sandbox ID nor Route and does not own projection,
Store mutation, feeder, or repair.

Manager and Gateway adapters pass canonical state through short-ID `ProjectionInput.State`:

| State | Local full Route | Forwarding |
|---|---:|---:|
| `creating`, `available`, `pausing`, `paused`, `resuming`, `upgrading` | retained | no |
| `running` | retained | yes |
| `recycling`, `terminating`, `succeeded`, `failed` | authoritatively absent | no |

Each adapter reuses the same visibility/deletion/lifecycle predicate in normal events and targeted
repair. All mutations use short-ID authority-specific Store APIs. Production does not restore
`GetRouteFromSandbox`, an independent registry, sandboxcr-owned periodic reconciliation,
unconditional deletion, or full-List repair.

`dead` survives only on a copied outgoing peer deletion payload. Full payloads preserve
namespace/name/ID/UID/RV and use conditional full deletion; old ID-only payloads use conditional
compatibility deletion. Precise local state is never rewritten. Gateway changes only CR extraction
and inclusion policy; its data-plane and peer policy remain running-only.

#### Step 3: adopt all consumers and public behavior

Switch `pkg/utils.GetSandboxState` to the canonical function and migrate every remaining caller in
one change. Remove tuple state, raw phase, route-owner helpers, and expected-state whitelists after
their last use. Controller/cache code may consume lifecycle directly; manager/E2B consume only the
normalized infra contract. No Controller-to-E2B or Controller-to-manager dependency is introduced.

Consumer policies remain distinct:

| Consumer | Policy |
|---|---|
| SandboxSet capacity | unclaimed controlled `creating` and `available` |
| SandboxSet used | claimed `creating`, `running`, `pausing`, `paused`, `resuming`, `upgrading` |
| SandboxSet ignored | recycling and future unexpected states; log without aborting reconcile |
| SandboxSet terminal GC | `terminating`, `succeeded`, `failed` |
| Active claimed count | claimed, non-reserved-failed six used states above |
| Quota liveness | unchanged and independent from active count |
| Waiters | canonical state plus operation-specific diagnostic fast failures |

Owner/lock metadata and `sandbox-claimed=true` remain one Kubernetes write. Claim recovery keeps
`max(status count, actual count)` with the new claimed-only actual count.

Public E2B state remains unchanged: internal `running` projects to `running`; `pausing`, `paused`, and
`resuming` project to `paused`; all other internal states are unrepresentable and List omits them.
Create, Clone, Resume, and Connect return a Sandbox body only after refreshed state is `running`.

Remove `expectedStates` lookup gates. Lookup first establishes existence, claim identity, and owner;
then lifecycle projection returns a Sandbox or a reasoned error. `web.ApiError` gains optional
`reason,omitempty` while retaining `code`, `headers`, `message`, and `request_id`:

| Found result | HTTP 404 reason |
|---|---|
| confirmed absence or unclaimed object | `SandboxNotFound` |
| `creating`, `available`, `upgrading` | `SandboxTemporarilyUnavailable` |
| `succeeded` | `SandboxSucceeded` |
| `failed` or reserved-failed | `SandboxFailed` |
| `terminating` | `SandboxTerminating` |
| `recycling` | `SandboxRecycling` |

Only `SandboxNotFound` means the claimed CR truly does not exist. Backend/ambiguity failures are not
404. Namespace/name diagnostic context is allowed only after lookup and authorization; absence,
ambiguity, and ownership failure disclose none.

Operation gates are explicit:

| Operation | Behavior |
|---|---|
| Pause | accept `running`/`pausing`/`paused`; `resuming` returns 409 |
| Resume | accept `paused`/`resuming`/`running`; `pausing` returns 409 |
| Connect | return running, start/join resume for paused/resuming, reject pausing with 400 |
| Delete | accept every owned state; confirmed/concurrent NotFound returns 204 |

Same-direction Pause/Resume joins preserve first-writer parameters and timeout ownership.

## Compatibility and Rollout

1. Implement Step 1 and the approved short-ID foundation independently.
2. Roll out short-ID-capable manager/Gateway binaries under its disabled-assignment and old-peer
   drain gates; do not apply Step 2 to mixed legacy route machinery.
3. Deploy Step 2 after lifecycle and route golden-path tests pass.
4. Deploy the atomic Step 3 consumer/API cutover.

There is no lifecycle CRD data migration. Step 2 can roll back state extraction/disposition while
leaving short-ID routing installed. Step 3 can roll back consumers and the additive error field.
Neither rollback crosses short-ID's separate no-rollback boundary after a short label is persisted.

## Risks and Verification

- Incorrect mutation-stage evidence could serve a partially changed Pod: only proven no-write paths
  use `PreUpdateFailed`; every uncertain path is non-serving.
- Step 2 directly affects routing: exhaustive phase/reason/time tests and route golden paths gate it.
- Route repair could resurrect terminal state: event and targeted repair share one predicate.
- Consumer drift could recreate five-state behavior: inventory every caller and test all eleven states.
- Internal state could leak publicly: centralize projection and assert E2B response bodies.
- Gateway peers may omit transitional non-forwarding Routes; local Running observation restores them.

Focused table-driven Go tests cover derivation, Controller stages, lookup zero/one/multiple outcomes,
route disposition/fencing, consumer grouping/accounting, operation gates, all six 404 reasons, and
dependency boundaries. No test under `test/` is required for implementation acceptance.

## Alternatives

Persisting derived state in the CRD duplicates source facts; keeping Route as existence/ownership
misclassifies intentionally absent Routes; evolving the legacy registry duplicates short-ID safety;
and a single big-bang cutover places an unproven model directly on the data plane. All are rejected.

## Implementation History

- [x] 07/16/2026: Design exploration completed and proposal opened for technical review.
