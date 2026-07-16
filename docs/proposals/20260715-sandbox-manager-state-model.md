---
title: Sandbox-manager Internal State Model
authors:
  - "@AiRanthem"
reviewers: []
creation-date: 2026-07-15
last-updated: 2026-07-16
status: provisional
---

# Sandbox-manager Internal State Model

## Summary

Replace the shared five-state Sandbox classification with a smaller internal model owned only by
sandbox-manager. The model describes the distinctions needed by manager operations and E2B APIs
without turning Controller phases into a repository-wide lifecycle contract.

SandboxSet continues to classify pool members with its own rules. Cache and waiters continue to use
the facts needed by their individual behavior. Routing carries an explicit allow, deny, or delete
decision instead of exposing lifecycle state.

## Motivation

The current shared model is both too coarse and too broad. Its dead state combines completion,
failure, removal, and temporary unavailability, while its creating and available states describe
pool behavior that sandbox-manager does not own.

This ambiguity causes duplicated pause and resume checks, temporary conditions to look like missing
Sandboxes, and routing decisions to depend on lifecycle strings. It also encourages unrelated
components to share one policy even though they need different answers from the same Sandbox facts.

## Proposal

### Manager-owned state

Sandbox-manager derives one of eight internal states from the current Sandbox object. The result is
read-only and is never persisted in the Sandbox resource.

| State | Meaning |
|---|---|
| claimable | A SandboxSet member is available for an attempted claim. |
| running | The Sandbox is ready, addressed, and able to serve. |
| pausing | A pause has been requested but is not complete. |
| paused | The pause is confirmed. |
| resuming | Resume or post-resume initialization is still in progress. |
| unready | The Sandbox exists but cannot currently serve. |
| terminating | Removal has started or is expected under an existing cleanup policy. |
| completed | The workload has succeeded or failed and no operation can make it active again. |

Removal takes precedence over completion because a Sandbox being deleted or recycled is still
waiting to disappear. Reserved failed Sandboxes are also terminating because they are retained only
temporarily for diagnosis before removal.

Pause and resume remain explicit so repeated requests can join work already moving in the same
direction and opposite-direction requests can be rejected consistently. Upgrade, readiness, missing
address, and unknown live observations are grouped as unready because manager policy treats all of
them as temporarily unable to serve.

Every observation includes a diagnostic explanation for protected logs. That explanation is not a
stable public value and is not exposed as an E2B Sandbox state.

### Component boundaries

Only sandbox-manager and its E2B server consume the eight-state model. Interpretation of raw phases,
conditions, cleanup metadata, and reserved-failure metadata stays behind the sandbox-manager
infrastructure boundary.

SandboxSet retains its existing creating and available behavior through local predicates. Pool
indexing, claim selection, active counting, quota accounting, and waiters keep their current
purpose-specific rules. In particular, claimable remains only a classification; the claim operation
still performs all address, freshness, lock, and candidate checks.

Recycle eligibility becomes a single infrastructure capability so upper manager layers no longer
combine a cleanup flag with raw Controller phase. Existing recycle metrics and fallback deletion
behavior remain unchanged.

### Routing decisions

Routes carry an explicit action rather than a manager lifecycle state:

| Observation | Route action |
|---|---|
| running | Allow traffic. |
| claimable, pausing, paused, resuming, or unready | Keep the route but deny traffic. |
| terminating or completed | Delete the route. |

Gateway derives the same three actions from its own Sandbox facts and does not import manager state.
Only allowed routes forward traffic. Denied routes remain available for later recovery, while a
delete action removes the route and is never stored as an active entry. Unknown actions fail closed.

During rolling upgrades, route messages carry both the new action and the legacy state understood by
older peers. New peers prefer the action and apply component-specific legacy behavior only when it
is absent. This compatibility detail remains outside route storage and data-plane decisions.

### E2B behavior

The public E2B Sandbox state remains unchanged. Running maps to public running, while pausing,
paused, and resuming map to public paused. Other internal states cannot be represented as a public
Sandbox and are omitted from lists or returned as reasoned unavailable responses.

The stable unavailable reasons distinguish confirmed absence, temporary unavailability, completion,
and termination. Internal state names and diagnostic explanations do not become public Sandbox
states.

Lookup first establishes existence and ownership, then the requested operation applies state policy.
Pause and Resume accept stable state or progress in the same direction, while opposite progress is a
conflict. Connect can join resume but rejects an in-progress pause. Delete accepts every owned state
and remains idempotent for confirmed absence. Backend and authorization failures remain distinct
from absence.

Some observations intentionally change behavior: upgrading, recycling, empty-phase, and unsupported-
phase Sandboxes may currently appear as public paused. They will instead be omitted or returned with
the appropriate unavailable reason because they are not safely representable as running or paused.

## Compatibility and Rollout

The change does not add a persisted state or require CRD data migration. Route messages remain
compatible during mixed-version rollout, and the optional E2B error reason is additive. Stable
running and paused observations retain their public representation.

The manager interface, consumer migration, route action, and legacy-state cleanup land as one atomic
implementation change. Rollback therefore reverts that complete change rather than leaving a binary
with mixed old and new state semantics.

## Risks and Verification

- A claimable Sandbox could be mistaken for guaranteed claim success. Existing atomic claim checks
  remain independent and receive equivalence coverage.
- A future Controller phase could be served accidentally. Unknown observations remain unready, and
  Gateway allows traffic only for an explicitly serviceable Running Sandbox.
- A delete decision could be stored as a route. Route tests verify that it only removes state.
- Mixed-version peers could interpret non-running routes differently. Compatibility tests cover both
  manager and Gateway fallback behavior.
- Internal state could leak through E2B. Projection tests assert that public Sandbox bodies contain
  only running or paused.
- Non-manager behavior could drift during migration. Focused tests preserve SandboxSet, cache, claim,
  quota, waiter, and recycling behavior.

Detailed derivation precedence, API contracts, migration tasks, and acceptance scenarios are defined
in the accompanying OpenSpec change.

## Alternatives

A shared repository-wide lifecycle was rejected because it would continue coupling unrelated
component policies. A larger model mirroring every Controller phase was rejected because manager
operations do not need those distinctions. Keeping lifecycle strings in routes was rejected because
routing needs a forwarding decision, not lifecycle meaning. A hard route protocol cutover was
rejected because rolling upgrades must remain safe.

## Implementation History

- 2026-07-15: Initial lifecycle-state exploration created a broader shared model.
- 2026-07-16: The design was narrowed to a sandbox-manager-only state model with separate SandboxSet
  and routing policies.
