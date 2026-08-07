---
title: SandboxUpdateOps InplaceUpdate Update Strategy
authors:
  - "@mahe"
reviewers:
  - "@zhaomingshan"
  - "@AiRanthem"
  - "@furykerry"
creation-date: 2026-08-04
status: provisional
see-also:
  - "/docs/proposals/20251218-sandbox-inplace-update.md"
  - "/docs/proposals/20260616-create-suo-command.md"
---

# SandboxUpdateOps InplaceUpdate Update Strategy

## Table of Contents

- [Summary](#summary)
- [Motivation](#motivation)
  - [Goals](#goals)
  - [Non-Goals/Future Work](#non-goalsfuture-work)
- [Proposal](#proposal)
  - [Risks and Mitigations](#risks-and-mitigations)
- [Alternatives](#alternatives)
- [Upgrade Strategy](#upgrade-strategy)
- [Test Plan](#test-plan)
- [Implementation History](#implementation-history)

## Summary

This proposal adds an `InplaceUpdate` strategy to `SandboxUpdateOps` (SUO) so
that a sandbox can be upgraded by patching its pod in place (container images
and resources only) instead of deleting and recreating it. The existing
`Recreate` and `CheckpointRestore` strategies both destroy the pod, which is
unnecessarily disruptive when only the image or resource limits change.

The in-place update still runs through the sandbox controller's **upgrade
lifecycle** (`Upgrading` phase, `PreUpgrade → UpgradePod → PostUpgrade`). Only
the `UpgradePod` step differs: the pod is patched instead of replaced. This
keeps an operator-triggered upgrade observable and hookable, and keeps it
distinct from the pre-existing claim-time in-place update, which must stay in
`Running`.

## Motivation

The sandbox controller has supported in-place updates since the very first
implementation: when a sandbox template's image or resources change and the
sandbox has **no** upgrade policy, the controller patches the pod using Kruise's
in-place update mechanism while the sandbox stays in `Running`.

That path exists to serve `SandboxClaim`: `spec.inplaceUpdate` lets a claim
adjust the image or resources of a pooled sandbox and then take delivery of it.
Delivery requires the sandbox to remain `Running`, because sandbox-manager's
`GetSandboxState` only recognizes sandboxes whose `Phase == Running`.

SUO has a different goal. It performs operator-driven batch upgrades, where the
desired behavior is the opposite of claim delivery:

- The upgrade must be observable (`Phase=Upgrading`, `Ready=False`).
- A sandbox being upgraded must **not** be handed to a new user, which the
  `Upgrading` phase achieves for free.
- `PreUpgrade`/`PostUpgrade` hooks must run, as they do for `Recreate`.
- Progress must aggregate the same way as the other SUO strategies.

Therefore SUO's in-place strategy runs through the upgrade lifecycle, while the
claim path is left exactly as it is. Making the claim path use the `Upgrading`
phase would require changing sandbox-manager first and is deliberately deferred.

## Goals

- Add `InplaceUpdate` to the `SandboxUpdateOpsStrategyType` enum.
- Add `InplaceUpdate` to the `SandboxUpgradePolicyType` enum so the sandbox
  controller can distinguish an operator-triggered in-place upgrade from the
  policy-less claim-time in-place update.
- Drive `InplaceUpdate` through the `Upgrading` phase and the upgrade lifecycle,
  replacing only the `UpgradePod` step with an in-place pod patch.
- Support `Lifecycle` hooks (`PreUpgrade`/`PostUpgrade`) for `InplaceUpdate`,
  identically to the pod-replacement strategies.
- Report completion and failure through the `Upgrading` condition, so SUO's
  `classifySandbox` uses one code path for every strategy.
- Support multiple sequential in-place update rounds on the same pod.
- Reject unsupported field changes (anything other than image, resources, and
  template metadata) at admission and at reconciliation time. Admission-time
  patch field validation is guarded by the
  `SandboxUpdateOpsInplacePatchValidation` feature gate (default off).

## Non-Goals/Future Work

- Changing the existing `Recreate` or `CheckpointRestore` code paths.
- Changing the claim-time in-place update path. A sandbox with no upgrade policy
  keeps applying template changes from the `Running` phase.
- Changing sandbox-manager. Teaching `GetSandboxState` to tolerate the
  `Upgrading` phase would be a prerequisite for unifying the two in-place paths;
  that work is deferred.
- Adding pod-level RBAC to the SUO controller. SUO reads only Sandbox Status.
- Exposing per-container failure details (e.g., which container is unhealthy).
  This would require direct pod access, which is out of scope.

## Proposal

### Architecture Decision: Two Distinct In-Place Paths

The sandbox controller keeps two in-place update paths, selected by the presence
of `spec.upgradePolicy`:

| | Claim-time in-place | SUO `InplaceUpdate` |
|---|---|---|
| `spec.upgradePolicy` | absent | `{type: InplaceUpdate}` |
| Trigger | `SandboxClaim.spec.inplaceUpdate` | `SandboxUpdateOps` |
| Phase during update | stays `Running` | `Upgrading` |
| Entry point | `EnsureSandboxUpdated` | `EnsureSandboxUpgraded` |
| Lifecycle hooks | no | yes |
| Outcome reported via | `InplaceUpdate` condition | `Upgrading` condition |
| Consumer | sandbox-manager delivery | SUO progress aggregation |

Both paths share the same low-level executor, `handleInPlaceUpdateCommon`, so
the actual pod-patching semantics (QoS pre-check, resize handling, multi-round
state annotation rebuild) stay in one place.

### Architecture Decision: Three Policy Predicates

Routing is expressed by three predicates with disjoint responsibilities. Keeping
them separate is what allows `InplaceUpdate` to run the lifecycle without ever
touching the pod-replacement code:

- `RequiresUpgradePhase(box)` — does a template change move the sandbox into the
  `Upgrading` phase? True for `Recreate`, `CheckpointRestore`, `InplaceUpdate`.
- `RequiresPodReplacementUpgrade(box)` — does the `UpgradePod` step delete and
  recreate the pod? True only for `Recreate` and `CheckpointRestore`.
- `RequiresInplaceUpgrade(box)` — does the `UpgradePod` step patch the existing
  pod? True only for `InplaceUpdate`.

### Architecture Decision: SUO Reads Sandbox Status Only

The SUO controller does **not** directly access Pods. It reads sandbox status
(phase, conditions) to classify update state. This preserves the existing
separation of concerns:

- SUO owns orchestration logic (selector, patch, quota, completion tracking).
- Sandbox controller owns pod-level operations (in-place update, recreate).

Because `InplaceUpdate` now reports through the `Upgrading` condition, SUO needs
no strategy-specific classification code at all.

### Flow Diagrams

#### SUO Phase Transitions

```plaintext
┌─────────┐     ┌──────────┐     ┌───────────┐
│ Pending │────►│ Updating │────►│ Completed │
└─────────┘     └─────┬────┘     └───────────┘
                       │
                       │ all failed (validation or upgrade)
                       ▼
                  ┌────────┐
                  │ Failed │
                  └────────┘
```

#### Routing a Template Change in the Sandbox Controller

```plaintext
              ┌──────────────────────────┐
              │ template hash changed    │
              └────────────┬─────────────┘
                           ▼
              ┌──────────────────────────┐
              │ RequiresUpgradePhase?    │
              └───────┬──────────┬───────┘
                   no │          │ yes
                      ▼          ▼
     ┌────────────────────┐   ┌──────────────────────┐
     │ stay Running       │   │ Phase = Upgrading    │
     │ EnsureSandboxUpdated│  │ EnsureSandboxUpgraded│
     │ (claim path)       │   └──────────┬───────────┘
     └────────────────────┘              ▼
                              ┌──────────────────────┐
                              │ PreUpgrade           │
                              └──────────┬───────────┘
                                         ▼
                              ┌──────────────────────┐
                              │ Checkpointing        │
                              │ (skipped unless      │
                              │  CheckpointRestore)  │
                              └──────────┬───────────┘
                                         ▼
                              ┌──────────────────────┐
                              │ UpgradePod           │
                              └───────┬──────────┬───┘
                        inplace policy│          │pod-replacement policy
                                      ▼          ▼
                    ┌──────────────────────┐  ┌──────────────────────┐
                    │ performInplaceUpgrade│  │performRecreateUpgrade│
                    │ patch pod in place   │  │ delete + create pod  │
                    └──────────┬───────────┘  └──────────┬───────────┘
                               └───────┬──────────────────┘
                                       ▼
                              ┌──────────────────────┐
                              │ PostUpgrade          │
                              └──────────┬───────────┘
                                         ▼
                              ┌──────────────────────┐
                              │ Succeeded → Running  │
                              └──────────────────────┘
```

#### Single Sandbox InplaceUpdate Lifecycle

```plaintext
┌───────────┐
│  Running  │
└─────┬─────┘
      │ SUO patches template + sets upgradePolicy=InplaceUpdate
      ▼
┌──────────────────────────────────────────┐
│ calculateStatus: revision changed and    │
│ RequiresUpgradePhase → Phase=Upgrading   │
│ clears stale Upgrading/InplaceUpdate     │
│ conditions                               │
└─────┬────────────────────────────────────┘
      ▼
┌──────────────────────────────────────────┐
│ UpgradePod: performInplaceUpgrade        │
│  ├─ no pod → create one (paused case)    │
│  ├─ immutable part changed → UpgradePod  │
│  │    Failed                             │
│  ├─ in progress → stay Upgrading         │
│  └─ InplaceUpdate=Failed → UpgradePod    │
│       Failed                             │
└─────┬────────────────────────────────────┘
      ▼
┌──────────────────────────────────────────┐
│ PostUpgrade hook → Upgrading=Succeeded   │
│ Phase=Running, Ready=True                │
└─────┬────────────────────────────────────┘
      ▼
┌──────────────────────────────────────────┐
│ SUO classifySandbox reads Upgrading      │
│ condition → sandboxUpdated               │
└──────────────────────────────────────────┘
```

### Change 1: API Types — Two New Enum Values

`SandboxUpdateOpsStrategyType` gains `InplaceUpdate` so users can select the
strategy. `SandboxUpgradePolicyType` gains `InplaceUpdate` so the sandbox
controller can tell an operator-driven in-place upgrade apart from the
policy-less claim path. Both enums are extended with kubebuilder validation and
regenerated CRDs.

### Change 2: Policy Predicates

`RequiresPodReplacementUpgrade` keeps its exact meaning (Recreate,
CheckpointRestore). `RequiresUpgradePhase` and `RequiresInplaceUpgrade` are
added. Existing call sites that meant "does this sandbox run the upgrade
lifecycle" switch to `RequiresUpgradePhase`; the one call site that means "does
the UpgradePod step replace the pod" keeps `RequiresPodReplacementUpgrade`.

### Change 3: patch.go — Map the SUO Strategy to an Upgrade Policy

`applySandboxPatch` sets `spec.upgradePolicy` from the SUO strategy:
`InplaceUpdate → {type: InplaceUpdate}`, `CheckpointRestore →
{type: CheckpointRestore}`, everything else `→ {type: Recreate}`. Setting the
policy explicitly (rather than leaving it unset) is what keeps the SUO path off
the claim path.

### Change 4: calculateStatus — Route to the Upgrading Phase

Both upgrade-trigger checks (from `Running` and from `Paused`) use
`RequiresUpgradePhase`. When entering `Upgrading`, the stale `InplaceUpdate`
condition of a previous round is removed alongside the stale `Upgrading`
condition. This matters because `handleInPlaceUpdateCommon` leaves the condition
untouched on some paths (notably a metadata-only change), so without the cleanup
a stale `Failed` from an earlier round would be read as the current round's
outcome.

### Change 5: EnsureSandboxUpdated — Restrict the Running-Phase Path

The `Running`-phase in-place branch is guarded by `!RequiresUpgradePhase(box)`
instead of `!RequiresPodReplacementUpgrade(box)`. Without this, a sandbox with
`upgradePolicy: InplaceUpdate` would be processed by the `Running` branch and
the `Upgrading` branch in alternating reconciles, racing on the same pod.

### Change 6: UpgradePod — Branch on the Policy

`EnsureSandboxUpgraded`'s `UpgradePod` step selects `performInplaceUpgrade` for
`InplaceUpdate` and keeps `performRecreateUpgrade` otherwise. The in-place branch
does not re-fetch the pod or re-initialize the runtime, because the pod was
patched rather than replaced.

`performInplaceUpgrade` returns `(done, failMsg, err)`:

- **No pod** → delegate to `performRecreateUpgrade` to create one. A sandbox
  upgraded while paused has no pod (`EnsureSandboxPaused` deletes it), and a
  freshly created pod already runs the target revision, so this reaches the
  desired state instead of failing.
- **hash-immutable-part changed** → terminal failure. The shared handler treats
  this as a no-op that only emits an event, which the state machine would
  otherwise read as success.
- **In progress** → `done=false`; the sandbox stays in `Upgrading`.
- **`InplaceUpdate=False/Failed`** → terminal failure, surfaced as
  `UpgradePodFailed` with the condition's message.
- Otherwise → success, proceed to `PostUpgrade`.

Checkpointing needs no change: `EnsureCheckpointForUpgrade` already
short-circuits for any policy other than `CheckpointRestore`.

### Change 7: classifySandbox — One Path for Every Strategy

The `InplaceUpdate`-specific branch and `classifyInplaceUpdatedSandbox` are
removed. Every strategy is classified from the `Upgrading` condition:
`Succeeded → Updated`; `PreUpgradeFailed`/`CheckpointFailed`/`UpgradePodFailed`/
`PostUpgradeFailed → Failed`; otherwise `Updating`.

Pre-validation of in-place feasibility (`validateInplaceUpdateFeasible`) is
retained: catching an infeasible patch before the sandbox is patched keeps the
sandbox untouched and fails the ops within a single reconcile.

### Change 8: Webhook — Admission Validation

- **Patch field whitelist** (unchanged): when the
  `SandboxUpdateOpsInplacePatchValidation` gate is on, an `InplaceUpdate` patch
  may only touch container images, container resources, and template metadata.
- **`updateStrategy.type` immutability** (unchanged): changing the type
  mid-flight would leave already-patched sandboxes on the old strategy.
- **Lifecycle hooks are now allowed** with `InplaceUpdate`. The earlier
  mutual-exclusion rule existed because the in-place path bypassed the lifecycle
  entirely; it no longer does.

### Change 9: Observability

`ValidationFailed` events are emitted on the ops when a sandbox fails
pre-validation. In-place `UpgradePod` outcomes emit
`UpgradePodUpdatedInPlace` (success) and `UpgradePodFailed` (terminal failure)
on the sandbox, mirroring `UpgradePodReplaced` on the recreate path.

## Risks and Mitigations

### Risk 1: A Leftover InplaceUpdate Policy Affects Claim Reuse

`spec.upgradePolicy` persists after an ops completes. If a sandbox previously
upgraded by an `InplaceUpdate` ops is later reused through a `SandboxClaim` with
`spec.inplaceUpdate`, the claim-time change will now run through the `Upgrading`
phase, and sandbox-manager's `GetSandboxState` will not see it as `Running`
until the upgrade finishes.

This is inherent to expressing the routing decision through `spec.upgradePolicy`,
and it applies equally to `Recreate` today. Mitigation: claim pools are managed
by `SandboxSet` and are not the intended target of operator-driven SUO upgrades.
If the combination becomes real, the claim path should clear or override the
policy — which is part of the deferred sandbox-manager work.

### Risk 2: In-Place Upgrade of a Paused Sandbox Degrades to Pod Creation

A paused sandbox has no pod, so there is nothing to patch. `performInplaceUpgrade`
creates a pod from the current template instead. The resulting pod already runs
the target revision, so the outcome is correct, but it is a pod creation rather
than a true in-place update. This is strictly better than failing the upgrade and
matches what `Recreate` would do.

### Risk 3: Stale InplaceUpdate Condition Across Rounds

`handleInPlaceUpdateCommon` does not always overwrite the `InplaceUpdate`
condition — a metadata-only change never sets it. A stale `Failed` from an
earlier round would therefore be misread as the current round's outcome.
Mitigated by removing the condition when entering the `Upgrading` phase
(Change 4). The handler's own terminal-state short-circuit is unaffected,
because the cleanup happens once per round, at phase entry.

### Risk 4: updateStrategy.type Immutability

Users who previously changed the strategy type on an existing SUO now receive a
webhook rejection. They should delete and recreate the SUO. This is intentional:
a mid-flight type change would leave already-patched sandboxes following the old
strategy.

## Upgrade Strategy

Both API changes are additive enum values, not breaking changes.

- **Existing SUOs**: `Recreate` and `CheckpointRestore` behave exactly as before.
- **Existing claim-time in-place updates**: unaffected. Sandboxes with no
  upgrade policy still apply template changes from the `Running` phase.
- **Multi-round in-place updates**: a template change on an already-updated pod
  now performs a real second update. Previously the sandbox controller silently
  skipped it.
- **Lifecycle + InplaceUpdate**: previously rejected at admission, now accepted
  and honored. This only widens what is allowed.
- No migration tool is needed.

## Alternatives

### Alternative 1: Reuse the Policy-less Running→Running Path

Rejected. An earlier revision of this proposal cleared `spec.upgradePolicy` for
`InplaceUpdate`, so SUO reused the claim-time path. It was less code, but it
conflated two different intents: the sandbox stayed `Running` throughout, so the
upgrade was not observable, sandbox-manager could hand out a sandbox that was
mid-upgrade, lifecycle hooks could not run, and SUO needed a second, in-place
specific classification path. It also made the two in-place triggers
indistinguishable to the sandbox controller, leaving no room to give them
different behavior.

### Alternative 2: Make the Claim Path Use the Upgrading Phase Too

Deferred, not rejected. Unifying both paths on the upgrade lifecycle is the
cleaner end state, but sandbox-manager's `GetSandboxState` requires
`Phase == Running` and would have to be taught that `Upgrading` is a transient,
recoverable state first. Doing that in the same change would couple an SUO
feature to a sandbox-manager refactor.

### Alternative 3: Distinguish the Two Paths by Label Instead of Policy

Rejected. The routing decision belongs to the sandbox's own spec, which is what
`spec.upgradePolicy` is for, and the upgrade state machine already keys off it.
Using the SUO tracking label would make the sandbox controller depend on an
SUO-owned label and would break for any other future in-place trigger.

### Alternative 4: SUO Directly Gets Pod

Rejected. SUO would need pod RBAC, would be coupled to pod lifecycle, and pod
transient states (restart, migration, temporary not-ready) would trigger
spurious reconciles. Sandbox Status already exposes phase and conditions —
sufficient for SUO's needs.

### Alternative 5: Pre-validation in applySandboxPatch

Rejected. If validation fails in `applySandboxPatch`, the error triggers a
requeue, but the sandbox was never patched (no ops label). On the next
reconcile, `classifySandbox` re-evaluates the sandbox as `sandboxCandidate`,
calls `applySandboxPatch` again, fails again — infinite loop. Pre-validation in
`classifySandbox` avoids this by classifying the sandbox as `sandboxFailed`
before patching.

### Alternative 6: Keep the Single-Round Limitation

Rejected. An earlier revision surfaced the limitation as
`InplaceUpdate=False/Failed`. That fixes the stale-condition false positive but
makes every second update on the same pod fail by design, forcing pod recreation
for routine repeated image bumps — the exact disruption `InplaceUpdate` exists to
avoid. Since `control.Update()` rebuilds the state annotation with a fresh
completion baseline on each call, sequential rounds are safe; the limitation was
historical, not architectural.

## Test Plan

- **Unit tests**:
  - Policy predicates: verify `RequiresUpgradePhase`,
    `RequiresPodReplacementUpgrade`, and `RequiresInplaceUpgrade` for nil, empty,
    `Recreate`, `CheckpointRestore`, `InplaceUpdate`, and unknown policy types.
  - `calculateStatus`: verify an `InplaceUpdate` policy with a changed hash
    enters `Upgrading` and clears the stale `InplaceUpdate` condition; verify a
    sandbox with no policy stays `Running`.
  - `EnsureSandboxUpgraded` with `InplaceUpdate`: verify completion transitions
    to `Running`, an in-progress update stays `Upgrading`, and a missing pod
    results in pod creation.
  - `classifySandbox`: verify `InplaceUpdate` is classified from the `Upgrading`
    condition (Succeeded/failed reasons/in-progress), and that an `InplaceUpdate`
    condition alone no longer decides the outcome.
  - `classifySandbox`: verify hash-immutable-part pre-validation rejects
    unsupported patches and passes supported ones.
  - `applySandboxPatch`: verify `upgradePolicy` is set to `InplaceUpdate`,
    replacing a leftover `Recreate` policy.
  - Webhook `handleCreate`: verify `InplaceUpdate` + `Lifecycle` is accepted.
  - Webhook `handleCreate`: verify the patch field whitelist rejects
    env/volumes/command/unknown fields and `$patch` directives, allows
    image/resources/metadata-only patches, and is inert when the
    `SandboxUpdateOpsInplacePatchValidation` gate is off.
  - Webhook `handleUpdate`: verify `updateStrategy.type` immutability.
  - `handleInPlaceUpdateCommon`: verify a completed or terminally failed previous
    round starts a new round (state annotation rebuilt with the new revision);
    verify an in-progress previous round waits.
- **E2E tests**:
  - An `InplaceUpdate` SUO patching only the image → sandbox passes through
    `Upgrading` and the ops reaches `Completed`; the pod UID is unchanged.
  - An `InplaceUpdate` SUO patching env → ops reaches `Failed` with a
    `ValidationFailed` event.
  - An `InplaceUpdate` SUO with `PreUpgrade`/`PostUpgrade` hooks → hooks execute.
  - Two sequential `InplaceUpdate` ops on the same sandbox → both complete.
  - A `SandboxClaim` with `spec.inplaceUpdate` → sandbox never leaves `Running`
    (claim path regression).
- **Regression**: existing `Recreate`/`CheckpointRestore` E2E tests pass
  unchanged.

## Implementation History

- [x] 08/04/2026: Grilling session to stress-test design; all decisions aligned.
- [x] 08/04/2026: Initial implementation using the policy-less in-place path.
- [x] 08/07/2026: Revised to run `InplaceUpdate` through the upgrade lifecycle
      with a dedicated `SandboxUpgradePolicy` value, keeping the claim-time
      in-place path untouched; lifecycle hooks are now supported.
- [ ] 08/07/2026: Open proposal PR.
