---
title: SandboxUpdateOps Concurrency — Per-Sandbox Latest-Template-Wins
authors:
  - "@mahe"
reviewers:
  - "@zhaomingshan"
  - "@furykerry"
creation-date: 2026-08-12
last-updated: 2026-08-19
status: provisional
see-also:
  - "/docs/proposals/20260804-suo-inplace-strategy.md"
  - "/docs/proposals/20251218-sandbox-inplace-update.md"
---

# SandboxUpdateOps Concurrency — Per-Sandbox Latest-Template-Wins

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals/Future Work](#non-goalsfuture-work)
- [Proposal](#proposal)
  - [Model Shift: Sandbox-Centric View](#model-shift-sandbox-centric-view)
  - [Decision 1: Per-Sandbox Granularity](#decision-1-per-sandbox-granularity)
  - [Decision 2: Latest-Template-Wins Requires a Full Template](#decision-2-latest-template-wins-requires-a-full-template)
  - [Decision 3: Safe Switch Points](#decision-3-safe-switch-points)
  - [Decision 4: Old-SUO Terminal Accounting (No Superseded Phase)](#decision-4-old-suo-terminal-accounting-no-superseded-phase)
  - [Decision 5: Polling Requeue for Wake-Up](#decision-5-polling-requeue-for-wake-up)
  - [Decision 6: No FIFO — Staleness Is By Design](#decision-6-no-fifo--staleness-is-by-design)
  - [Decision 7: Status Shape — Aggregates on SUO, Details on Sandbox](#decision-7-status-shape--aggregates-on-suo-details-on-sandbox)
  - [Three-SUO Interleaving Scenario](#three-suo-interleaving-scenario)
- [Known Limitations](#known-limitations)
- [Risks and Mitigations](#risks-and-mitigations)
- [Alternatives](#alternatives)
- [Test Plan](#test-plan)
- [Implementation History](#implementation-history)

## Summary

When multiple SandboxUpdateOps (SUO) objects target overlapping sandbox sets,
the current controller **skips** any sandbox already labeled by another active
SUO (`sandboxNoNeedUpdate`). The later SUO silently completes as a no-op — the
user's expected change never lands.

This proposal replaces the earlier "whole-SUO queueing" draft with a
**sandbox-centric latest-template-wins** model:

1. **Per-sandbox granularity.** Each sandbox independently determines its
   target. Idle sandboxes in a SUO's selection update immediately; a sandbox
   occupied by an in-flight round waits only until the next safe switch point.
2. **Latest-template-wins.** Among all active SUOs selecting a sandbox, the
   newest one (by `creationTimestamp`, tie-broken by name) defines the
   sandbox's desired final template. Older intents are superseded, not queued.
3. **Full-template contract.** A SUO participating in this model must carry
   the complete desired template, not an incremental field patch — otherwise
   skipping intermediate SUOs would lose user intent.
4. **Safe switch points.** A sandbox mid-upgrade switches to a newer target
   only at states where the pod is stable; critical sections (checkpointing,
   post-upgrade hooks, in-flight in-place patches) are never interrupted.

The sandbox keeps its existing two externally visible states — `Running` and
`Upgrading`. No `Waiting`/`Superseded` phase is introduced anywhere.

## Motivation

Users may issue multiple SUOs concurrently — this is **not under our control**:

- **CI/CD pipelines** triggering multiple upgrades in parallel.
- **Automation systems** emitting SUOs without waiting for prior ones.
- **Multiple operators** working on overlapping sandbox sets.

Two failure modes exist today:

- **Silent drop:** the later SUO completes as a no-op; its change never lands.
- **Pointless intermediate rollouts (if naively queued):** executing SUO-1
  (image), SUO-2 (cpu), SUO-3 (memory) strictly in sequence forces every
  sandbox through two obsolete intermediate states before reaching the final
  one the user actually wants.

The user's real expectation when they submit several updates in a row is that
the **last submission describes the final state**. The system should converge
each sandbox to that final state with the minimum number of rollouts.

### Goals

- Every sandbox converges to the template of the **newest** SUO selecting it.
- **No silent drops:** superseded or waiting work is visible in SUO status.
- **Per-sandbox progress:** one busy sandbox never stalls the rest of a batch.
- **Minimal rollouts:** obsolete intermediate templates are skipped whenever a
  newer target exists before a sandbox starts (or safely re-targets) a round.
- Deterministic final state regardless of reconcile interleaving.

### Non-Goals/Future Work

- **Strict FIFO execution of every SUO.** Older SUOs are intentionally
  superseded; this is the core semantics, not a limitation.
- **Waiting timeout.** A takeover pending on a safe switch point does not time
  out; liveness comes from fast-fail (image pull) and round completion.
- **Event-driven wake-up.** Polling requeue is the MVP mechanism.
- **Multi-worker hardening.** The default single worker serializes SUO
  reconciles; `--sandboxupdateops-workers > 1` remains best-effort.
- **Mid-critical-section preemption.** Never planned; see Decision 3.

## Proposal

### Model Shift: Sandbox-Centric View

The earlier draft of this proposal reasoned at the SUO level ("is this SUO
fully or partially blocked?") and required whole-SUO all-or-nothing waiting.
That reasoning is replaced.

The unit of conflict is the **sandbox**, because that is where the occupancy
label (`LabelSandboxUpdateOps`) and the template live. For each sandbox:

```
candidates(sbx) = all active SUOs whose selector matches sbx
target(sbx)     = the newest SUO in candidates(sbx)
                  (creationTimestamp, tie-break: name)
```

- The sandbox follows `target(sbx)`; its ops label records which SUO it is
  currently following.
- "Complete overlap" vs "partial overlap" between SUOs stops being a case
  split: both decompose into independent per-sandbox `target()` decisions.
- A SUO is merely (a) a source of one immutable full-template revision and
  (b) an observation window aggregating the progress of sandboxes that
  currently follow it.

### Decision 1: Per-Sandbox Granularity

| Aspect | Detail |
|--------|--------|
| **Behavior** | 10 selected, 3 occupied by an older in-flight round → 7 update immediately, 3 switch at their next safe point |
| **Rationale** | One stuck sandbox must not stall nine ready ones; with the full-template contract the user intent is *convergence to a final state*, not batch atomicity |
| **Visibility** | The SUO aggregates per-sandbox progress counters (updated / updating / pendingTakeover); no sandbox is silently dropped |
| **Rejected** | Whole-SUO all-or-nothing waiting (previous draft) — amplifies a single stuck sandbox into a batch-wide stall and forces a queue model that latest-wins makes unnecessary |

### Decision 2: Latest-Template-Wins Requires a Full Template

Latest-wins is only safe if the newest SUO fully describes the desired state.
With incremental patches (`SUO-1: image`, `SUO-2: cpu`, `SUO-3: memory`),
executing only SUO-3 silently loses the image and cpu intents.

| Aspect | Detail |
|--------|--------|
| **Contract** | Every SUO carries the complete desired template snapshot |
| **API shape** | Replace `spec.patch` with `EmbeddedSandboxTemplate` (`template` \| `templateRef`), consistent with `SandboxSet`/`Sandbox`; template-only, no patch mode |
| **`spec.patch`** | **Removed.** Incremental patch semantics are incompatible with latest-wins skipping (a skipped intermediate patch is silent intent loss) |
| **Spec mutability** | Only `updateStrategy.maxUnavailable` (rollout speed) and `paused` (emergency brake) stay mutable. `template`/`templateRef`, `selector`, `lifecycle`, `updateStrategy.type`, and `states` (mutable today, tightened here) are immutable after creation. Binding the intent to `creationTimestamp` is what makes the newest-wins rule deterministic: an editable template on an old timestamp would carry new intent that the winner computation cannot see. New intent = new SUO — which latest-wins turns into the natural workflow anyway |
| **Rejected** | Reusing `spec.patch` with a documented "must be full" convention — unverifiable, silent foot-gun |
| **Rejected** | Dual-mode (`patch` kept alongside `template`) — two concurrency semantics on the same sandbox fork the winner rules and double the test matrix, without a real user need |
| **Rejected** | Patch-stacking (merge all pending patches in timestamp order, one rollout) — final state is opaque to the user, requires cross-SUO merge machinery, and conflicts between stacked patches are undiagnosable |

### Decision 3: Safe Switch Points

Switching a sandbox's target mid-upgrade is only allowed when the pod is in a
**stable state**. From the upgrade state machine
(`SandboxUpgradingReason*`): `Resuming → ResumeSucceed → PreUpgrade →
Checkpointing → UpgradePod → PostUpgrade → Succeeded`, plus per-stage Failed
reasons.

Stable states:

- **S1** — the round has not touched the pod yet (`Resuming`, `ResumeSucceed`,
  `PreUpgrade` done): old pod intact.
- **S2** — the round finished (`Succeeded`): new pod ready.
- **S3** — the round terminally failed with a determinable pod state.

Per-stage takeover rules:

| Stage | Pod state | Switch action |
|-------|-----------|---------------|
| Resuming / ResumeSucceed | old pod untouched (S1) | switch immediately, zero cost |
| PreUpgrade | old pod untouched (S1) | switch immediately; the pre-upgrade backup captures the *current* state and is target-independent, so it is reused |
| Checkpointing | commit job in flight | **critical section** — wait for the checkpoint to finish, then switch; the checkpoint snapshots the old pod and remains valid for the new target |
| UpgradePod (Recreate / CheckpointRestore) | old pod gone, replacement not ready | phase 1: wait for round end; phase 2 (optimization): delete the unserved half-built pod and rebuild directly with the new template |
| UpgradePod (InplaceUpdate, in flight) | pod mid-transition | **never switch** — fail-stop; the completion baseline is judged by ImageID change and re-patching mid-flight corrupts it |
| UpgradePod (InplaceUpdate, image pull failed) | pod stuck mid-pull (S3) | in-place takeover impossible (a container in `ImagePullBackOff` never picks up a changed `spec.image`; E2E-verified). Only a **Recreate/CheckpointRestore** newest SUO may take over. If the newest SUO is itself `InplaceUpdate`, it does **not** fall back to an older Recreate SUO (that would land a stale intent) and does not silently escalate the policy; it reports the sandbox as **failed** with guidance: *"cannot take over an image-pull-failed sandbox with InplaceUpdate; submit a Recreate/CheckpointRestore SUO"* |
| UpgradePod (InplaceUpdate, resize infeasible) | pod stable (S3) | switch immediately — the engine's existing terminal gate already supports starting a corrected round |
| PostUpgrade | new pod ready, hook running inside it | **critical section** — interrupting a half-done workspace restore leaves dirty state; wait for `Succeeded`, then start a fresh round |
| Any Failed reason | per table above | take over according to the actual pod state |

**MVP scope:** phase 1 implements switching only at **S1** and at **round
end** (S2/S3). The Checkpointing-completion switch and the half-built-pod
rebuild are phase-2 optimizations; correctness is identical, only takeover
latency differs (bounded by one round).

**Atomic switch:** re-targeting is one sandbox update carrying the new
template, `LabelSandboxUpdateOps`, and the upgrade policy/lifecycle fields
together, so no observer sees a label/template mismatch.

**Inherited failures and the rolling window (tentative):** the rolling
formula charges `failed` against the `maxUnavailable` budget as a circuit
breaker — correct when the failure was caused by this SUO's own rounds (a bad
template must slow itself down). But a **policy-mismatch failure inherits a
pod that was already broken before this SUO touched it**; charging it against
the window lets a few leftover wrecks freeze the rollout of hundreds of
healthy sandboxes (e.g. 3 inherited failures with `maxUnavailable=3` halts
everything), violating the per-sandbox granularity goal. Therefore failures
are classified by origin: **self-inflicted failures consume the window;
inherited failures are counted and reported but do not consume it.** Marked
tentative: revisit if operators find the two failure classes confusing in
practice.

The fail-fast work (image pull failures become terminal within seconds,
`UpgradePodFailed`) is what makes the bad-image path livelock-free **without
manual SUO deletion**: the stuck round reaches S3 quickly and the newest
Recreate-type SUO takes over at the next poll.

### Decision 4: Old-SUO Terminal Accounting (No Superseded Phase)

From the sandbox's perspective only `Running` and `Upgrading` exist; no
sandbox ever needs a "superseded by X" state. The supersession is expressed
purely by *which SUO the sandbox follows next*.

For the SUO object:

| Aspect | Detail |
|--------|--------|
| **Accounting** | A sandbox that re-targets to a newer SUO leaves the old SUO's pending set (it is no longer "mine to update") |
| **Terminal rule** | An old SUO reaches `Completed` when no selected sandbox still follows it and none remains pending; its status message records how many sandboxes were converged by newer operations (e.g. `5 updated, 3 superseded by ops-c`) |
| **No new phase** | `Pending / Updating / Completed / Failed` unchanged; no `Superseded` enum value, no CRD validation change |
| **Rejected** | A `Superseded` phase — adds state-machine surface for information that a message conveys; the sandbox-centric model makes the SUO object a report, not a contract |

### Decision 5: Polling Requeue for Wake-Up

Unchanged from the previous draft, and still necessary: the
`SandboxEventHandler` enqueues only the SUO named by the sandbox's ops label.
A newer SUO that has not yet taken over holds no labels, so sandbox events
never wake it; deletion of the older SUO clears the label and enqueues nobody.

| Aspect | Detail |
|--------|--------|
| **Mechanism** | A SUO with pending takeovers requeues with a configurable delay (MVP default 30s); each poll recomputes `target(sbx)` for its selection and takes over whatever reached a safe point |
| **Covers all release paths** | round completion, terminal failure (incl. fast-fail), and deletion of the older SUO |
| **Future** | Event-driven wake-up (on round end, enqueue SUOs selecting the sandbox) — deferred |

### Decision 6: No FIFO — Staleness Is By Design

The earlier draft needed admission ordering (FIFO by `creationTimestamp`) to
prevent starvation of waiting SUOs. Latest-wins dissolves this problem:

- When a sandbox frees, the **newest** candidate wins directly. There is no
  queue to be fair about.
- An older SUO that never gets to run is not starved — it is **stale**: its
  intent has been explicitly replaced by a newer full-template submission.
  Its terminal accounting (Decision 4) reports that fact.
- `creationTimestamp` (tie-break: name) is used only to *select the winner*,
  not to order execution. Reconcile time is never used: it reflects controller
  scheduling, not user intent.

**Paused winner freezes its selection.** A paused SUO stays the winner: it
starts no new rounds (in-flight rounds finish naturally — the brake is not an
abort), and no older SUO may touch its sandboxes (that would land stale
intent, forcing a double rollout after unpause). This is the literal meaning
of an emergency brake: everything the user targeted holds still while they
investigate. Recovery: unpause to resume, or submit a newer SUO which becomes
the winner and obsoletes the paused one. Accepted cost: a forgotten paused
SUO freezes its selection indefinitely — visible via its stalled counters and
a perpetual `Updating` phase in `kubectl get suo`.

### Decision 7: Status Shape — Aggregates on SUO, Details on Sandbox

The SUO status carries **counters only**; per-sandbox detail lives on the
sandbox itself, which is the single source of truth.

```yaml
status:
  phase: Updating
  updated: 750          # already at my template
  updating: 50          # rounds in flight toward my template
  pendingTakeover: 200  # waiting for an older round to reach a safe point
  failed: 0
```

| Aspect | Detail |
|--------|--------|
| **Detail lookup** | Which sandboxes are pending? `kubectl get sandbox -l <selector>` — the ops label names the round each sandbox currently follows. Why pending? The sandbox's `Upgrading` condition message carries the wait reason (e.g. `ImagePullBackOff`) |
| **Events** | Key transitions are recorded as SUO events: takeover performed, takeover blocked at a critical section, policy-mismatch failure (Decision 3) |
| **Rationale** | Sandbox state is never duplicated into SUO status, so the two can never disagree; status size is O(1) regardless of batch size (1000-sandbox batches would otherwise bloat etcd objects and watch traffic) |
| **Rejected** | Per-sandbox detail list in status (`pendingTakeoverSandboxes: [{name, blockedBy}]`) — O(N) status churn, etcd object growth, and a second copy of sandbox truth |
| **Rejected** | Capped sample list (first 10 blocked names) — sample selection is arbitrary and unstable across reconciles; the user still needs the label-selector query for the full picture |

### Three-SUO Interleaving Scenario

SUO-A (sandboxes 1,2), SUO-B (2,3), SUO-C (1,3); created in that order, all
carrying full templates. Per-sandbox targets:

```
sbx-1: {A, C} → C wins
sbx-2: {A, B} → B wins
sbx-3: {B, C} → C wins
```

One possible execution (single worker):

```
A reconcile: patches 1,2 (label=A) → rounds start
B reconcile: sbx-2 occupied by A's round → pending takeover; sbx-3 free →
             patch 3 (label=B)
C reconcile: sbx-1 occupied → pending; sbx-3 occupied by B → pending
sbx-1 round (A) reaches safe point → C takes over → sbx-1 → C.template
sbx-2 round (A) reaches safe point → B takes over → sbx-2 → B.template
sbx-3 round (B) completes → C takes over → sbx-3 → C.template
A: Completed ("0 remaining, 2 superseded"); B: Completed; C: Completed
```

The final state — `sbx-1: C, sbx-2: B, sbx-3: C` — is **deterministic by
construction** for any reconcile interleaving, because `target(sbx)` depends
only on the candidate set, never on timing. The previous draft could only
claim order-independence via field-merge commutativity; this model makes the
final state a pure function.

## Known Limitations

### Multi-Worker Behavior

The per-sandbox winner is deterministic, so two workers reconciling different
SUOs compute the same `target(sbx)` and the loser does not patch. The residual
risk is the pre-existing lock-free status/label write pattern under
`--sandboxupdateops-workers > 1` (silent MergeFrom overwrite); the default
remains 1 worker and a startup warning is emitted for higher values.

### Takeover Latency Bounded by Round Duration (MVP)

Phase 1 switches only at S1/round-end, so a takeover can wait for a full
in-flight round (bounded; unbounded stalls are excluded by image-pull
fast-fail and the resize terminal gate). Phase 2 shortens the Recreate and
Checkpointing paths.

Note the asymmetry for bad images: the **InplaceUpdate** path reaches S3
within seconds (fast-fail, implemented), but a **Recreate** round whose new
pod sticks in `ImagePullBackOff` only reaches S3 when the upgrade's own
failure detection fires. Until the phase-2 half-built-pod rebuild lands,
bad-image rescue latency on the Recreate path equals that detection delay,
not seconds. Accepted for phase 1.

## Risks and Mitigations

### Risk 1: User Submits a Partial Template Believing It Is a Patch

The most important UX risk of the full-template contract: omitted fields are
*removals*, not "keep as is".

**Mitigation:** distinct API field (`template`/`templateRef`) so the semantics
are explicit at the type level; webhook validation requires the template to be
self-contained (same rules as `SandboxSet.spec.template`); documentation
states the override semantics prominently.

### Risk 2: Rapid Successive SUOs (Template Thrash)

Many SUOs in quick succession → intermediate ones never execute.

**Mitigation:** none needed — this is the intended semantics (the newest
snapshot is the only goal); each superseded SUO's status says so explicitly.

### Risk 3: InplaceUpdate Stuck Round Delays Takeover

A newer SUO cannot take over a mid-flight in-place round.

**Mitigation:** image-pull failures reach S3 within seconds (fast-fail,
implemented); resize rejections are terminal via the existing gate; the only
remaining wait is a *healthy* in-flight round, which completes on its own.

### Risk 4: handleDeletion Cache Race (Pre-existing)

Informer lag can cause label-cleanup omission on SUO deletion. Unchanged by
this proposal; `ResourceVersionExpectation` and requeue already handle
eventual consistency.

## Alternatives

### Alternative 1: Skip (Current Behavior)

Later SUO silently no-ops. **Rejected:** silent intent loss.

### Alternative 2: Whole-SUO All-or-Nothing Queueing (Previous Draft)

Queue entire SUOs; any occupied sandbox parks the whole operation
(`Pending + WaitingFor`), FIFO admission, strict sequential execution.
**Superseded:** (1) one stuck sandbox stalls the whole batch; (2) sequential
execution forces obsolete intermediate rollouts; (3) it required new queueing
machinery (WaitingFor, FIFO admission, blocked classification) that
latest-wins makes unnecessary.

### Alternative 3: Patch-Stacking

Keep incremental patches; at each safe point, merge **all** pending patches in
timestamp order and roll out once. Preserves patch ergonomics and avoids
intermediate rollouts. **Rejected:** the final state is not stated anywhere
(only derivable by mentally replaying the stack); cross-SUO merge conflicts
are undiagnosable; accounting for "which SUO is done" becomes ambiguous.

### Alternative 4: Waiting / Superseded Phases

**Rejected:** the sandbox needs only `Running`/`Upgrading`; SUO-level
supersession is a status message, not a state machine extension.

### Alternative 5: Event-Driven Wake-Up

Precise, zero polling delay. **Deferred:** requires wake-up logic on round
completion and deletion paths; polling covers both with one mechanism.

## Test Plan

### Unit Tests

1. **Winner computation:** `target(sbx)` picks the newest active SUO by
   `creationTimestamp`, tie-broken by name; terminal and deleting SUOs are
   excluded from candidates.
2. **Idle immediate update:** selected idle sandboxes are patched by the
   newest SUO even while other selected sandboxes are occupied.
3. **No mid-critical-section switch:** a sandbox in Checkpointing /
   PostUpgrade / in-flight inplace round is not re-targeted; the pending
   takeover is reflected in status counters.
4. **S1 switch:** a sandbox whose round has not touched the pod re-targets
   atomically (template + ops label in one update).
5. **S3 rules:** resize-infeasible → immediate takeover; image-pull-failed →
   takeover only by a Recreate/CheckpointRestore newest SUO; an InplaceUpdate
   newest SUO reports the sandbox failed with the guidance message instead of
   falling back to an older SUO or escalating the policy.
6. **Old-SUO accounting:** superseded sandboxes leave the old SUO's pending
   set; the old SUO completes with a supersession message when nothing
   follows it.
7. **Full-template validation:** webhook requires `template` or `templateRef`
   (self-contained, same rules as `SandboxSet.spec.template`); `spec.patch`
   is no longer accepted.
8. **Spec immutability:** webhook rejects post-creation changes to
   `template`/`templateRef`, `selector`, `lifecycle`, `updateStrategy.type`,
   and `states`; allows `maxUnavailable` and `paused`.
9. **Window accounting:** an inherited (policy-mismatch) failure increments
   `failed` but does not reduce the number of new rounds the SUO may start;
   a failure produced by the SUO's own round does.

### E2E Tests

1. **Same-target pair:** SUO-1 then SUO-2 (both full-template) on the same
   sandboxes; sandboxes not yet started by SUO-1 go straight to SUO-2's
   template (no intermediate rollout); SUO-1 completes with supersession
   accounting.
2. **Three-SUO interleaving:** A(1,2), B(2,3), C(1,3) → final state
   `1:C, 2:B, 3:C` regardless of ordering; all three reach terminal phase.
3. **Bad-image rescue without manual deletion:** SUO-1 (InplaceUpdate, bad
   image) sticks a sandbox; SUO-2 (Recreate, good image) is created; verify
   the round fast-fails to S3 and SUO-2 takes over automatically — no
   deletion of SUO-1 required.
4. **Partial overlap progress:** B's free sandbox updates immediately while
   its contested sandbox waits for A's round to settle.

## Implementation History

- 2026-08-12: Original draft — whole-SUO all-or-nothing queueing
  (`Pending + WaitingFor`, FIFO admission).
- 2026-08-19: Redesigned to sandbox-centric latest-template-wins after design
  review: per-sandbox granularity, full-template contract, safe switch
  points, no new phases. The queueing draft is preserved as Alternative 2.
