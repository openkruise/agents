---
title: SandboxUpdateOps Concurrent Queueing
authors:
  - "@mahe"
reviewers:
  - "@zhaomingshan"
  - "@AiRanthem"
  - "@furykerry"
creation-date: 2026-08-12
status: provisional
see-also:
  - "/docs/proposals/20260804-suo-inplace-strategy.md"
  - "/docs/proposals/20251218-sandbox-inplace-update.md"
---

# SandboxUpdateOps Concurrent Queueing

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals/Future Work](#non-goalsfuture-work)
- [Proposal](#proposal)
  - [Design Decisions](#design-decisions)
  - [Decision 1: All-or-Nothing Granularity](#decision-1-all-or-nothing-granularity)
  - [Decision 2: Pending + WaitingFor Field (No New Phase)](#decision-2-pending--waitingfor-field-no-new-phase)
  - [Decision 3: Polling Requeue for Wake-Up](#decision-3-polling-requeue-for-wake-up)
  - [Decision 4: Field-Level Merge via StrategicMergePatch](#decision-4-field-level-merge-via-strategicmergepatch)
  - [Decision 5: InplaceUpdate Bad-Image Interaction](#decision-5-inplaceupdate-bad-image-interaction)
  - [Three-SUO Interleaving Scenario](#three-suo-interleaving-scenario)
- [Known Limitations](#known-limitations)
  - [Multi-Worker Atomicity Crack](#multi-worker-atomicity-crack)
- [Risks and Mitigations](#risks-and-mitigations)
- [Alternatives](#alternatives)
- [Test Plan](#test-plan)
- [Implementation History](#implementation-history)

## Summary

When multiple SandboxUpdateOps (SUO) objects target overlapping sandbox sets
and arrive concurrently, the current controller **skips** any sandbox that is
already labeled by another active SUO (`sandboxNoNeedUpdate`). This means the
later SUO silently completes as a no-op without applying its intended update —
the user's expected change never lands.

This proposal adds **queueing semantics**: instead of skipping, a SUO whose
candidate sandboxes are occupied by another active SUO enters a **Waiting**
state (expressed as `Phase=Pending` + a `Status.WaitingFor` field). It polls
for release and, once the preceding SUO completes or is deleted, takes over
the now-free sandboxes and applies its own patch.

The design relies on the existing **single-worker default**
(`concurrentReconciles = 1`) to guarantee "all-or-nothing" atomicity at the
classification stage. Multi-worker concurrency is documented as a known
limitation with a deferred solution.

## Motivation

Users may issue multiple SUOs concurrently — this is **not under our control**.
Common scenarios include:

- **CI/CD pipelines** triggering multiple upgrades in parallel.
- **Automation systems** emitting SUOs without waiting for prior ones to
  complete.
- **Multiple operators** (human or automated) working on overlapping sandbox
  sets.

The current skip model is *safe* (no corruption), but the user experience is
poor: the later SUO silently completes as a no-op, and the user's expected
update is never applied. The user has no way to tell their change was dropped
unless they inspect the sandbox template afterwards.

### Goals

- Allow multiple concurrently-issued SUOs to **safely wait** and apply their
  updates in sequence.
- Preserve the existing "all-or-nothing" classification guarantee under the
  default single-worker configuration.
- Reuse the existing `StrategicMergePatch` mechanism for field-level merge —
  no new patch logic required.
- Provide user-visible feedback (`Status.WaitingFor`) so users can tell which
  SUO is blocking theirs and take manual action (delete the stuck SUO).

### Non-Goals/Future Work

- **Multi-worker concurrency** (N ≥ 2 workers). Documented as a known
  limitation; deferred to future work.
- **Waiting timeout.** A SUO in Waiting does not time out; release depends on
  the preceding SUO completing, failing, or being manually deleted. See
  [InplaceUpdate Bad-Image Interaction](#decision-5-inplaceupdate-bad-image-interaction).
- **Field-level conflict resolution.** The existing `StrategicMergePatch`
  already handles field-level merge semantics; this proposal does not change
  merge behavior.
- **Event-driven wake-up.** A polling requeue is used as the MVP mechanism;
  upgrading to event-driven wake-up is future work.

## Proposal

### Design Decisions

The following decisions were aligned through a structured grilling session.
Each decision is presented with its rationale and rejected alternatives.

### Decision 1: All-or-Nothing Granularity

When a SUO matches N candidate sandboxes and **any one** of them is occupied by
another active SUO, the **entire** SUO enters Waiting — not just the occupied
sandboxes.

| Aspect | Detail |
|--------|--------|
| **Behavior** | 10 candidates, 3 occupied → all 10 wait, 0 patched |
| **Rationale** | Atomic semantics: the user issued one SUO to upgrade a batch, and partial execution leaves an inconsistent intermediate state |
| **Throughput cost** | 1 stuck sandbox can stall 9 ready ones. Acceptable because: (1) the recovery path is identical to partial execution (delete the stuck SUO), (2) bad-image stall is an exception, not the norm |
| **Rejected** | Partial execution (patch the 7 free, wait on the 3 occupied) — introduces a SUO state machine that simultaneously holds Updating and Waiting sandboxes, and partial rollback on conflict is complex |

**Why current code already supports this at classification time:**
`classifySandboxes` collects all candidates before patching. If any candidate
returns `sandboxNoNeedUpdate` due to occupation, the SUO does not patch any —
it stays Pending. The "all-or-nothing" check is a classification-stage
determination, not a patch-stage one.

### Decision 2: Pending + WaitingFor Field (No New Phase)

Waiting is **not** a new phase. It is expressed as `Phase=Pending` + a new
`Status.WaitingFor` string field naming the blocking SUO.

| Aspect | Detail |
|--------|--------|
| **Phase** | Remains `Pending` (no new enum value, no CRD validation change) |
| **Field** | `Status.WaitingFor: "ops-A"` — users see who is blocking them |
| **Active check** | The existing `classifySandbox` active check `Phase == Pending \|\| Updating` already covers Waiting — a Waiting SUO has `Phase=Pending`, so it is already treated as active by subsequent SUOs. **No code change needed here.** |
| **Completion** | A Waiting SUO patches 0 sandboxes → `updated=0, failed=0, updating=0` → does not satisfy the completion count → stays Pending. **No code change needed here.** |
| **Rejected** | New `Waiting` phase — requires changing the phase enum, CRD validation, completion-terminal checks, and the active-occupation check. Higher cost for marginal clarity gain |

**Important:** An earlier draft of this proposal claimed the active check and
completion logic needed modification. That was **incorrect** — under the
Pending+WaitingFor scheme, both are already correct. The existing
`Pending || Updating` check naturally covers Waiting SUOs because their phase
is still `Pending`.

### Decision 3: Polling Requeue for Wake-Up

A SUO in Waiting is **not** woken by events. It polls via requeue.

**Why event-driven wake-up does not work today:**

The `SandboxEventHandler.Update` reads the sandbox's
`LabelSandboxUpdateOps` label and enqueues the SUO named by that label. When
SUO-B is Waiting, it has not patched any sandbox, so no sandbox's label points
to SUO-B. Sandbox state changes enqueue **SUO-A** (the label holder), not
SUO-B. When SUO-A is deleted, `handleDeletion` clears the sandbox label →
`SandboxEventHandler` sees `opsName == ""` → enqueues nobody. **In both cases,
SUO-B is never woken by events.**

| Aspect | Detail |
|--------|--------|
| **Mechanism** | SUO-B enters Waiting → requeue with a delay (e.g., 30s) → on requeue, `classifySandboxes` re-checks all candidates → if free, patch; if still occupied, requeue again |
| **Delay** | Configurable; MVP default 30s. Trade-off: shorter = faster wake-up but more reconcile load; longer = less load but slower wake-up |
| **Covers both release paths** | (1) Preceding SUO completes → sandbox free on next poll. (2) Preceding SUO deleted → `handleDeletion` clears label → sandbox free on next poll |
| **Rejected (future)** | Event-driven: preceding SUO's reconcile, upon reaching Completed/Failed, lists all SUOs with `WaitingFor=preceding-ops-name` and enqueues them. `handleDeletion` does the same on deletion. No polling delay, but requires adding wake-up logic to two code paths. Deferred to future work |

### Decision 4: Field-Level Merge via StrategicMergePatch

"Later-writer-wins" is already the current behavior — no code change required.

`mergeTemplate` ([`patch.go` L73-91](../../pkg/controller/sandboxupdateops/patch.go#L73-L91))
uses `strategicpatch.StrategicMergePatch` with `ops.Spec.Patch.Raw` against the
sandbox's **current** template. This means:

- **Same field, later wins:** SUO-A patches `image=centos:8`, SUO-B patches
  `image=centos:9`. SUO-B takes over → merge overwrites to `centos:9`. ✓
- **Same field, same value:** SUO-B patches `image=centos:8` against a template
  already at `centos:8` → `isSandboxTemplateMatchPatch` returns true → SUO-B
  completes as no-op. ✓
- **Different fields, additive:** SUO-A patches `image=centos:8`, SUO-B
  patches `resources={cpu:2}`. SUO-B takes over → merge keeps `image=centos:8`
  and adds `resources=cpu:2`. ✓

The base for `mergeTemplate` is `modified.Spec.Template` = the sandbox's
**current** template at takeover time, which already reflects SUO-A's result.
This naturally implements "later-writer-wins at field level."

### Decision 5: InplaceUpdate Bad-Image Interaction

When SUO-A is an `InplaceUpdate` stuck on a bad image (kubelet
`ImagePullBackOff`, container never restarts), the sandbox is stuck in
`Upgrading` phase and never completes. A Waiting SUO-B will never be released
by completion — only by **manual deletion of SUO-A**.

| Aspect | Detail |
|--------|--------|
| **Release mechanism** | User deletes stuck SUO-A → `handleDeletion` clears sandbox label → sandbox no longer "occupied" → SUO-B's next poll takes over |
| **SUO-B must be Recreate** | If SUO-B is also `InplaceUpdate`, it deadlocks (see [0804 proposal Risk 5](./20260804-suo-inplace-strategy.md#risks-and-mitigations)). Only `Recreate` or `CheckpointRestore` can recover a bad-image-stuck sandbox |
| **No timeout** | Waiting does not time out. Adding a timeout risks killing legitimate long-running upgrades (e.g., batch Recreate of 50 pods). The user's perception burden ("is my SUO stuck?") is the same as today — check `WaitingFor` to see who is blocking, then inspect that SUO |
| **Residual state coverage** | `handleDeletion` leaves `UpgradePolicy` and `Lifecycle` on the sandbox (documented in prior analysis). When SUO-B takes over, `applySandboxPatch` overwrites both fields ([`patch.go` L152-170](../../pkg/controller/sandboxupdateops/patch.go#L152-L170)). Residual state is naturally covered |

### Three-SUO Interleaving Scenario

Consider three SUOs with overlapping sandbox sets arriving concurrently:

- **SUO-A**: sandboxes 1, 2
- **SUO-B**: sandboxes 2, 3
- **SUO-C**: sandboxes 1, 3

Under single-worker serialization (default `concurrentReconciles = 1`):

```
A reconcile: 1,2 free → patch 1,2 (label=A) → Updating
B reconcile: 2 occupied by A (active) → all-or-nothing → Pending+WaitingFor=A
C reconcile: 1 occupied by A (active) → all-or-nothing → Pending+WaitingFor=A

A completes → 1,2 released
B poll: 2 free, 3 free → patch 2,3 (label=B) → Updating
C poll: 1 free, 3 occupied by B (active) → Pending+WaitingFor=B

B completes → 3 released
C poll: 1 free, 3 free → patch 1,3 (label=C) → Updating → completes
```

**No deadlock.** The execution order is A → B → C (or A → C → B depending on
which poll fires first after A completes). The key invariant: at any point,
**at most one active SUO** holds labels on the overlapping sandboxes. The
"all-or-nothing" check ensures a Waiting SUO does not partially patch, and the
single-worker serialization ensures B and C do not simultaneously patch and
race on the shared sandbox (e.g., sandbox 3).

If the poll order is B-then-C after A completes:
- B grabs 2,3 → C waits on B (3 occupied)
- B completes → C grabs 1,3

If C-then-B:
- C grabs 1,3 → B waits on C (3 occupied)
- C completes → B grabs 2,3

Both orderings are correct. The final sandbox state is the same regardless of
order, because each SUO's patch is applied against the current template
(field-level merge), and the last writer for each field wins.

## Known Limitations

### Multi-Worker Atomicity Crack

The "all-or-nothing" guarantee holds **only** under single-worker serialization
(`concurrentReconciles = 1`, the default). If the `--sandboxupdateops-workers`
flag is set to N ≥ 2, different SUOs can be reconciled concurrently by
different worker goroutines.

In the three-SUO scenario above, if B and C are reconciled simultaneously
after A completes:

```
B reconcile: 2 free, 3 free → enters patch stage
C reconcile: 1 free, 3 free → enters patch stage

B patches sandbox 2 → success (label=B)
B patches sandbox 3 → optimistic lock conflict with C
C patches sandbox 1 → success (label=C)
C patches sandbox 3 → conflict (or success depending on ordering)

Loser requeues, reclassifies → sandbox 3 already labeled by winner
→ all-or-nothing requires waiting, but sandbox 1 (or 2) already patched
→ forced partial execution: cannot roll back the already-patched sandbox
```

`applySandboxPatch` patches sandboxes **one by one** in the candidates loop.
If a conflict occurs mid-way, the already-patched sandboxes cannot be rolled
back (the template is already changed, the pod may already be upgrading). The
SUO is forced into partial execution — violating the "all-or-nothing" contract.

**Current mitigation:** The default worker count is 1, which serializes all SUO
reconciles and prevents this race. Users who increase `--sandboxupdateops-workers`
must accept that "all-or-nothing" degrades to "best-effort all-or-nothing"
under contention.

**Future work options (not implemented in this proposal):**

1. **Reservation lock:** Before patching, atomically claim labels on all
   candidate sandboxes (optimistic lock). Only proceed to template patch if
   all label claims succeed; otherwise roll back labels and wait.
2. **Per-sandbox mutex:** A distributed lock per sandbox to serialize
   cross-SUO access.
3. **Document and accept:** Leave multi-worker as an unsupported configuration
   with a documented warning.

## Risks and Mitigations

### Risk 1: Polling Delay

A Waiting SUO wakes up to `requeue-delay` (e.g., 30s) after the preceding SUO
releases. This adds latency to the upgrade pipeline.

**Mitigation:** The delay is configurable. For latency-sensitive deployments,
reduce the requeue interval. Event-driven wake-up (future work) eliminates this
delay entirely.

### Risk 2: Stuck SUO Stalls All Waiters

If SUO-A is stuck (e.g., InplaceUpdate bad image), all SUOs waiting on it are
stalled until the user manually deletes SUO-A.

**Mitigation:** The `Status.WaitingFor` field tells the user exactly which SUO
is blocking. The user inspects that SUO, determines it is stuck, and deletes
it. This is the same perception burden as today — the user must notice a
long-stuck upgrade. Queueing does not add new burden; it only changes "silently
skipped" to "explicitly waiting."

### Risk 3: handleDeletion Cache Race (Pre-existing)

`handleDeletion` uses the informer cache to list labeled sandboxes. If the
label was applied just before deletion, cache lag may cause cleanup omission
(documented in prior analysis as P2).

**Mitigation:** This is a pre-existing issue, not introduced by this proposal.
The `ResourceVersionExpectation` mechanism and reconcile requeue already handle
eventual consistency. Queueing does not worsen this race.

## Alternatives

### Alternative 1: Skip (Current Behavior)

The current model skips occupied sandboxes. The later SUO silently completes as
a no-op.

**Rejected:** The user's expected update never lands, and there is no
indication it was dropped. Queueing is strictly better for user experience.

### Alternative 2: New Waiting Phase

Add a `Waiting` value to the phase enum, distinct from `Pending`.

**Rejected:** Requires changes to the phase enum, CRD validation, completion
checks, and the active-occupation check. The `Pending + WaitingFor` scheme
achieves the same semantics with zero phase-enum changes — the existing
`Pending || Updating` active check already covers Waiting SUOs because their
phase is still `Pending`.

### Alternative 3: Event-Driven Wake-Up

On reaching Completed/Failed, the preceding SUO lists all SUOs with
`WaitingFor=preceding-name` and enqueues them. `handleDeletion` does the same
on deletion.

**Deferred:** More precise than polling (zero wake-up delay, no polling
overhead), but requires adding wake-up logic to two code paths (reconcile
completion and handleDeletion). Polling is simpler for MVP and covers both
release paths. Event-driven can be layered on later without changing the
queueing semantics.

### Alternative 4: Partial Execution

Patch the free sandboxes immediately; wait only on the occupied ones.

**Rejected:** Introduces a SUO state machine that simultaneously holds Updating
and Waiting sandboxes. On optimistic-lock conflict mid-patch, partial rollback
is complex. The "all-or-nothing" semantics are simpler and match user intent
("I issued one SUO to upgrade this batch").

## Test Plan

### Unit Tests

1. **classifySandboxes all-or-nothing:** Given N candidates where 1 is occupied
   by an active SUO, verify 0 are classified as `sandboxCandidate` and the SUO
   remains Pending with `WaitingFor` set.
2. **WaitingFor field population:** Verify `WaitingFor` is set to the blocking
   SUO's name when entering Waiting, and cleared when transitioning to
   Updating.
3. **Active check covers Pending:** Given SUO-B in Pending+WaitingFor, verify
   SUO-C's `classifySandbox` treats SUO-B as active (does not skip/take over).
4. **Field-level merge on takeover:** Given SUO-A patched `image=centos:8` and
   SUO-B patches `resources={cpu:2}`, verify SUO-B's takeover produces a
   template with both `image=centos:8` and `resources=cpu:2`.
5. **Same-field later-wins:** Given SUO-A patched `image=centos:8` and SUO-B
   patches `image=centos:9`, verify the final template has `image=centos:9`.
6. **Polling requeue:** Verify a Waiting SUO requeues itself with the
   configured delay and does not transition to Completed.

### E2E Tests

1. **Sequential two-SUO:** Issue SUO-A (sbox 1,2) and SUO-B (sbox 2,3)
   concurrently. Verify A completes first, then B takes over and completes.
   Verify final template reflects B's patch (field-level merge).
2. **Three-SUO interleaving:** Issue A(1,2), B(2,3), C(1,3) concurrently.
   Verify all three complete in some order with no deadlock.
3. **InplaceUpdate bad-image recovery:** Issue SUO-A (InplaceUpdate, bad image)
   on sandbox 1. Issue SUO-B (Recreate) on sandbox 1 concurrently. Verify SUO-B
   enters Waiting. Delete SUO-A. Verify SUO-B takes over, recreates the pod,
   and completes.
4. **WaitingFor visibility:** Issue two concurrent SUOs. Verify the Waiting SUO
   shows `Status.WaitingFor` pointing to the active SUO.

## Implementation History

- 2026-08-12: Proposal drafted after a structured grilling session that aligned
  on all design decisions and exposed two hallucination risks (event-driven
  wake-up assumption, multi-worker atomicity crack).
