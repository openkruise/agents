---
title: Sandbox-manager Internal State Model
authors:
  - "@AiRanthem"
reviewers: []
creation-date: 2026-07-15
last-updated: 2026-07-28
status: provisional
---

# Sandbox-manager Internal State Model

## Summary

Replace the shared five-state Sandbox classification with eight internal states used only by
sandbox-manager. The new model distinguishes pause and resume progress, temporary unavailability,
removal, and completion without exposing Controller phases as a public lifecycle API.

## Motivation

The current `dead` state combines completed workloads, removal, and temporary failures. As a
result, an existing Sandbox can look missing, pause and resume checks are duplicated, and route
behavior depends on ambiguous lifecycle strings.

SandboxSet also uses `creating` and `available` for pool management. Those classifications answer a
different question and should not define sandbox-manager lifecycle behavior.

## Proposal

### Internal states

The Sandbox provider derives one read-only state from the current Sandbox object. The state is not
persisted in the CR.

| State | Meaning | Route | E2B representation |
|---|---|---|---|
| `claimable` | A pool member is available for an attempted claim. | Deny | HTTP 404 `SandboxTemporarilyUnavailable` |
| `running` | Ready, addressed, and able to serve. | Allow | `running` |
| `pausing` | Pause was requested but is not complete. | Deny | `paused` |
| `paused` | Pause is confirmed. | Deny | `paused` |
| `resuming` | Resume or post-resume initialization is in progress. | Deny | `paused` |
| `unready` | Exists but cannot currently serve. | Deny | HTTP 404 `SandboxTemporarilyUnavailable` |
| `terminating` | Deletion or recycling is in progress. | Delete | HTTP 404 `SandboxTerminating` |
| `completed` | Succeeded or failed and cannot become active again. | Delete | HTTP 404 `SandboxCompleted` |

Removal takes precedence over completion. A retained failed Sandbox is `completed` until deletion
starts or its finite shutdown deadline expires, at which point it becomes `terminating`.
A deadline expires only when the observation time is strictly after it.

Pause and resume remain explicit so repeated requests can join work moving in the same direction,
while opposite-direction requests can be rejected. Upgrading, missing readiness or address, cleanup
requests not yet acted on, and unknown live observations are `unready`.

Each observation includes a diagnostic reason for protected logs. It is not a stable public value
and is never returned as an E2B Sandbox state or route field.

### Component boundaries

- The Sandbox CR provider is the only code that interprets raw phase, conditions, cleanup metadata,
  reserved failures, and shutdown deadlines.
- Sandbox-manager consumes `GetState`; E2B consumes Manager results and owns HTTP and public-model
  mapping.
- Manager and Gateway both consume the provider's `GetRoute`. Gateway does not implement a second
  state or route policy.
- Controller, SandboxSet, and cache keep their purpose-specific pool and waiter predicates. They do
  not consume the eight-state model.
- `claimable` is only a classification. Claim still performs its existing address, freshness, lock,
  candidate, and revision checks.
- Manager uses the provider's `IsRecyclable` capability. If recycling is ineligible or cannot
  start, deletion falls back to direct removal.

### Routing decisions

Routes carry `Allow`, `Deny`, or `Delete` instead of lifecycle state. Only `Allow` forwards traffic.
`Deny` keeps a recoverable route but blocks traffic. `Delete` removes the active route and keeps the
existing non-forwarding deletion decision.

For the same Sandbox UID and resource version, the safe order is `Delete > Deny > Allow`. Unknown
actions fail closed. This prevents a slower observer from restoring traffic after another observer
has reached a shutdown deadline.

### E2B behavior

Public Sandbox bodies continue to expose only `running` or `paused`. Lookup first establishes
existence and ownership; each API then applies its own state policy.

| E2B API | Accepted internal states | Successful result | Other states |
|---|---|---|---|
| Create / Clone | No existing state | Wait for refreshed `running`; return Sandbox state `running`. | Preserve existing operation errors. |
| List | All observations | Include `running`; include `pausing`, `paused`, and `resuming` as `paused`. | Omit `claimable`, `unready`, `terminating`, and `completed`. |
| Describe | `running`, `pausing`, `paused`, `resuming` | Return `running` or `paused` using the projection above. | Return the lifecycle HTTP 404 reason from the state table. |
| Pause | `running`, `pausing`, `paused` | Start, join, or skip pause and return the existing empty success response. | `resuming` returns HTTP 409; other states use lifecycle unavailability. |
| Resume | `paused`, `resuming`, `running` | Start, join, or skip resume; return empty HTTP 204 after refreshed `running`. | `pausing` returns HTTP 409; other states use lifecycle unavailability. |
| Connect | `running`, `paused`, `resuming` | Return Sandbox state `running`; start or join resume when needed. | `pausing` returns HTTP 400; other states use lifecycle unavailability. |
| Snapshot / Set timeout | `running` | Preserve the existing success response. | Preserve the existing running-only rejection or lifecycle error. |
| Delete | Every state, confirmed absence, and concurrent NotFound | Accept deletion or recycling and return empty HTTP 204. | Authorization and non-NotFound backend failures remain errors. |

Confirmed absence returns HTTP 404 `SandboxNotFound` except for idempotent Delete. Backend,
authorization, timeout, and cancellation failures are not converted to absence.

## Compatibility and Rollout

This proposal builds on the existing shared Sandbox provider and route-safety baseline. The state
model and route action are then adopted atomically by Manager, Gateway, proxy, and E2B consumers.

No state is persisted and no CRD migration is required. The E2B error `reason` field is additive,
and the public `running` and `paused` values remain unchanged.

The behavior is not fully backward compatible: claimed Upgrading, Recycling, empty-phase, and
unsupported-phase Sandboxes may currently return HTTP 200 `paused`. They will instead be omitted
from List or return a reasoned HTTP 404.

During a mixed-version rollout, route messages temporarily carry both action and legacy state. New
peers prefer action; existing provider compatibility rules keep old peers fail-closed. The legacy
field is removed in a later change.

## Risks and Verification

- Test every state-derivation precedence rule, including unknown future phases.
- Compare Manager and Gateway `GetRoute` results for the same observation.
- Verify same-version `Delete > Deny > Allow` ordering and stale-route rejection.
- Preserve existing pool, claim, waiter, quota, and SandboxSet behavior with equivalence tests.
- Cover every row of the E2B table and assert that response bodies expose only `running` or `paused`.

Detailed precedence, API contracts, compatibility cases, and acceptance scenarios are defined in
the accompanying OpenSpec changes.

## Alternatives

- A repository-wide lifecycle was rejected because Controller and Manager answer different
  questions.
- A Gateway-local route policy was rejected because it can drift from Manager.
- Lifecycle strings in Route were rejected because routing needs an action.
- A hard route protocol cutover was rejected because rolling upgrades must remain safe.

## Implementation History

- 2026-07-15: Initial lifecycle-state exploration created a broader shared model.
- 2026-07-16: The design was narrowed to a sandbox-manager-oriented eight-state model.
- 2026-07-17: The design adopted the shared Sandbox provider and one canonical `GetRoute` path.
- 2026-07-28: Simplified the proposal and documented per-API E2B behavior.
