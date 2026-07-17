---
title: Sandbox-manager Internal State Model
authors:
  - "@AiRanthem"
reviewers: []
creation-date: 2026-07-15
last-updated: 2026-07-17
status: provisional
see-also:
  - "/docs/proposals/20260717-sandbox-provider.md"
---

# Sandbox-manager Internal State Model

## Summary

Replace the shared five-state Sandbox classification with a narrower eight-state observation used
by sandbox-manager. The model describes the distinctions required by manager operations and E2B
APIs without making Controller phases a repository-wide lifecycle contract.

The shared Sandbox CR provider is a prerequisite. It owns CR interpretation and exposes state and
route capabilities without exposing raw CR details to manager business logic. Gateway uses the same
route capability as manager, so new versions cannot implement different routing rules for the same
Sandbox observation.

## Motivation

The current shared model is both too coarse and too broad. Its dead state combines completion,
failure, removal, and temporary unavailability, while its creating and available states describe
pool behavior that sandbox-manager does not own.

This ambiguity causes duplicated pause and resume checks, temporary conditions to look like missing
Sandboxes, and routing decisions to depend on lifecycle strings. Duplicating the CR interpretation
in Gateway would replace one inconsistency with another, so state-to-route mapping must have one
implementation shared by both components.

## Proposal

### Manager-oriented state

Sandbox-manager observes one of eight states from the current Sandbox object. The result is
read-only and is never persisted in the Sandbox resource.

| State | Meaning |
|---|---|
| claimable | A SandboxSet member is available for an attempted claim. |
| running | The Sandbox is ready, addressed, and able to serve. |
| pausing | A pause has been requested but is not complete. |
| paused | The pause is confirmed. |
| resuming | Resume or post-resume initialization is still in progress. |
| unready | The Sandbox exists but cannot currently serve. |
| terminating | Removal has started or a finite removal deadline has been reached. |
| completed | The workload has succeeded or failed and cannot become active again. |

Removal takes precedence over completion because an object being deleted or recycled is still
waiting to disappear. A reserved failed Sandbox with a finite shutdown deadline is terminating when
that deadline expires; one retained forever is completed because it can never become active and is
not progressing toward removal.

Pause and resume remain explicit so repeated requests can join work moving in the same direction
and opposite-direction requests can be rejected consistently. Upgrade, readiness, missing address,
and unknown live observations are grouped as unready because manager policy treats all of them as
temporarily unable to serve.

Every observation includes a diagnostic explanation for protected logs. That explanation is not a
stable public value and is never exposed as an E2B Sandbox state.

### Component boundaries

The state vocabulary is defined by the neutral Sandbox provider contract. The Sandbox CR provider
is the only implementation allowed to interpret raw phase, conditions, cleanup metadata, reserved
failure, and shutdown deadlines. Sandbox-manager consumes the result through GetState and does not
inspect those CR facts itself.

E2B never calls the provider or Manager infra directly. It calls the Manager interface, receives a
protocol-independent lifecycle outcome, and owns public state, HTTP status, and error-reason mapping.

Gateway does not consume state for business behavior. It wraps the watched CR with the same
read-only provider and calls GetRoute. GetRoute obtains GetState and applies the canonical route
mapping internally. Controller, SandboxSet, and cache do not consume the eight-state observation;
they use provider-owned pool predicates and operation-specific facts instead.

SandboxSet retains its existing creating and available behavior. Pool indexing, claim selection,
active counting, quota accounting, and waiters preserve their current purpose-specific rules.
Claimable remains a classification; claim still performs address, freshness, lock, candidate, and
revision checks.

Recycle eligibility becomes a provider capability so manager no longer combines a cleanup flag
with raw Controller phase. It includes known Controller preconditions such as the absence of
persistent-volume claims. Cleanup metadata alone is a request and temporarily denies traffic; the
state becomes terminating only after recycling or deletion begins. If Controller cannot honor an
accepted recycle request, it falls back to direct deletion so a successful Delete cannot leave a
serving Sandbox behind.

### Routing decisions

Routes carry an explicit action instead of lifecycle state:

| Observation | Route action |
|---|---|
| running | Allow traffic. |
| claimable, pausing, paused, resuming, or unready | Keep the route but deny traffic. |
| terminating or completed | Delete the route. |

Manager and Gateway both call the provider's GetRoute method. There is no Gateway-local phase,
readiness, resume, or update policy. For the same CR snapshot and observation time, new Manager and
Gateway versions therefore produce identical metadata and action.

Only allowed routes forward traffic. Denied routes remain stored for later recovery, while a delete
action removes the active route and records a non-forwarding decision tombstone. For the same
Sandbox UID and resource version, Delete overrides Deny and Allow, and Deny overrides Allow. A slow
observer therefore cannot recreate an allowed route after another observer has crossed a shutdown
deadline. Unknown actions fail closed.

During rolling upgrades, route messages carry both the new action and the legacy state understood
by older peers. New peers prefer action. Supported mixed-version rollout starts only after every
route producer and receiver has the provider prerequisite's deletion tombstone, authoritative
restart validation, and conservative legacy-running safety. An older baseline component may delete
a denied record rather than retain it, but every fallback remains fail-closed and cannot enable
traffic that the new provider denies.

### E2B behavior

The public E2B Sandbox state remains limited to running and paused. Running maps to public running,
while pausing, paused, and resuming map to public paused. Other internal states cannot be represented
as a public Sandbox and are omitted from lists or returned as reasoned unavailable responses.

The stable unavailable reasons distinguish confirmed absence, temporary unavailability,
completion, and termination. Internal state names and diagnostic explanations do not become public
Sandbox states.

Lookup first establishes existence and ownership, then the requested operation applies state
policy. Pause and Resume accept stable state or progress in the same direction, while opposite
progress is a conflict. Connect can join resume but rejects an in-progress pause. Delete accepts
every owned state and remains idempotent for confirmed absence. Backend and authorization failures
remain distinct from absence.

A direct request for a claimed Upgrading, Recycling, empty-phase, or unsupported-phase Sandbox may
currently return HTTP 200 with public state paused. Upgrading, empty-phase, and unsupported-phase
observations instead return HTTP 404 with SandboxTemporarilyUnavailable, while Recycling returns
HTTP 404 with SandboxTerminating. List omits all four.

## Compatibility and Rollout

The shared Sandbox provider refactor lands first. It preserves public and lifecycle behavior while
installing the conservative legacy-route and deletion-tombstone safety baseline. The state-model
implementation then changes the provider observation and Route action atomically across manager,
Gateway, proxy, and E2B consumers.

No state is persisted and no CRD migration is required. The optional E2B error reason is additive,
but the documented HTTP 200 to HTTP 404 changes are intentionally not backward compatible. Stable
running and paused observations retain their public representation.

The compatibility-only wire state remains during the mixed-version window and is removed by a
later change. Rollback reverts the complete state-model implementation without reverting the
provider extraction and its route-safety baseline.

## Risks and Verification

- Manager and Gateway could diverge if either reintroduces raw CR routing logic. Import and behavior
  tests require both to use GetRoute and compare their results for the same observations.
- A claimable Sandbox could be mistaken for guaranteed claim success. Existing atomic claim checks
  remain independent and receive equivalence coverage.
- A future Controller phase could be served accidentally. Unknown observations remain unready and
  therefore denied.
- A delete decision could be stored as a route. Route tests verify that it only removes state.
- Independent clocks can cross shutdown time on the same CR resource version. Same-version action
  ordering and deletion tombstones prevent a late Allow from reviving a removed route.
- Receiver restart can erase in-memory decisions. The prerequisite keeps routing closed until the
  local Sandbox cache is synchronized, rebuilds current decisions, and rejects absent or stale peer
  records before allowing forwarding.
- Mixed-version peers can retain different records. Compatibility tests require every fallback to
  remain fail-closed and preserve resource-version ordering.
- Internal state could leak through E2B. Projection tests assert that public Sandbox bodies contain
  only running or paused.

Detailed precedence, API contracts, compatibility cases, and acceptance scenarios are defined in
the accompanying OpenSpec changes.

## Alternatives

A repository-wide lifecycle shared by Controller and manager was rejected because those components
answer different questions. A Gateway-local route policy was rejected because it cannot guarantee
the same result as manager. Lifecycle strings in Route were rejected because forwarding needs an
action rather than lifecycle meaning. A hard route protocol cutover was rejected because rolling
upgrades must remain safe.

## Implementation History

- 2026-07-15: Initial lifecycle-state exploration created a broader shared model.
- 2026-07-16: The design was narrowed to a sandbox-manager-oriented eight-state model.
- 2026-07-17: Review clarified E2B compatibility and mixed-version routing safety.
- 2026-07-17: The design adopted the shared Sandbox provider and one canonical GetRoute path for
  Manager and Gateway.
