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

## 2026-09 原地更新重构修订

本节覆盖下文历史方案中“Claim 不变”“Ready 恒为 False”“旧轮次未完成不能原地纠错”的约束。详细场景见 `docs/analysis/inplace-update-refactor-todo.md` 的 C/U/H 状态表。当前代码实施中，尚未完成验证。

### 共享引擎与状态消费

- Claim 与 Upgrade 共享三阶段引擎，返回阶段与保留原始 Cause 的错误；配置生效不等于 Pod Ready，更不等于整个 Upgrade 成功。
- Claim 保持 Running，最终成功要求配置生效且 Pod Ready。Upgrade 按 PreUpgrade → 配置生效 → PostUpgrade → Pod Ready 完成；正常等待时 Ready 跟随 Pod，写入失败和 hook 失败关闭 Ready。
- 写入与生效失败统一 UpdateFailed；QoS 拒绝独立分类，静态校验先于 PreUpgrade。Conflict/限流/临时网络错误重试，拉取退避和 resize Deferred 等待；可确定属于当前目标的不可行 resize 等错误终止。
- resize 前持久化资源意图，避免 resize 成功、metadata patch 失败后丢失生效检查；资源下调必须等待实际配额收敛。目标 hash 不能替代实际状态。未声明资源键继续保留系统侧值，本轮不增加资源键删除协议。
- 镜像记录新增 `targetImage`，以实际运行目标确认生效，允许回到原 ImageID；历史记录仍使用原基线判断。
- Claim 等待端仅在 `InplaceUpdate=False/InplaceUpdating` 时等待。QoS 或 immutable-hash 拒绝等未写入镜像或资源的终态 `Failed`，若 Sandbox 保持 `Running` 且旧 Pod 健康，则沿用既有行为并允许交付；显式 Upgrade 不继承旧 Claim Condition。

#### 待解决：部分写入重试误入 metadata 快速路径（2026-09-17）

状态：用户确认暂缓修复，保留现有快速路径；具体方案尚未确定，本次仅记录问题，不调整引擎、底层更新模式或追踪协议。上文“部分写入后仍须观察资源实际生效”是目标约束，不能据此认为当前路径已经实现或验证该保证。

触发过程：

1. 首次更新的 resize 成功，Pod.spec 中的资源已变为目标值，但容器实际资源尚未生效。
2. 随后的 metadata/hash Patch 失败，例如返回 Conflict；本次调用报告 `inplaceUpdateStepPatchDelivered` 和错误。
3. 重试读取到已更新的 Pod.spec，目标 hash 尚未补齐；`isMetadataOnlyChange` 仅比较 spec 与模板，将本次补写识别为 metadata-only。
4. metadata Patch 成功后，引擎直接返回 `inplaceUpdateStepSucceeded`，没有等待实际资源生效。若采用“引擎 Succeeded 且 Pod Ready 即最终成功”的 Claim adapter，此时旧 Pod 仍 Ready 就会提前写出 `Ready=True`、`InplaceUpdate=True/Succeeded`。

根因与修复边界：

- spec 已匹配目标不等于运行状态已生效；旧镜像或资源更新仍未完成时叠加 metadata 变更，也有相同的快速路径风险。
- 单独移除快速成功返回、改为下一轮观察，不一定解决全部问题：若部分写入后的追踪记录缺失，或旧记录没有覆盖本次资源更新，完成检查仍可能漏掉待观察资源。因此不能把这一调整单独视为完整修复。
- 后续方案需要区分真正的纯 metadata 更新与未完成更新的收尾，并保证跨重试观察当前目标；目前未选择具体实现，不直接切换整个 UpdateMode，以免同时改变资源降配等兼容语义。
- 此问题独立于 `handleInPlaceUpdateCommon` 的 `newStatus` 参数移除及历史终态短路清理，后两项的变更不代表本问题已经关闭。

后续验收场景（暂未实施）：

- resize 成功、metadata 失败后重试成功，但实际资源仍旧、Pod Ready：不得提前报告更新成功，继续观察配置生效。
- 上一轮镜像或资源尚未生效时仅修改 metadata：不得因 spec 匹配而跳过已有更新的完成检查。
- 没有待完成更新的纯 metadata 变更可正常完成；资源或镜像实际生效后，Claim 仍须结合 Pod Ready 判定最终成功。

### 新 SUO 与有限生命周期

- 补救沿用删除旧 SUO、提交正确 images 的新 SUO。新 UID 开始完整生命周期，不继承旧 hook；用户仍可选择 InplaceUpdate 或 Recreate。
- SUO 新增可选 `spec.updateStrategy.timeoutSeconds`，下发到 Sandbox `spec.upgradePolicy.timeoutSeconds`，最小 1 秒，未指定时 Controller 使用 300 秒。预算按 Sandbox 从开始执行到最终 Ready 计算，不含队列等待，同轮重试/重启不延长，新 SUO 重新计时；Claim/E2B 超时不变。
- Sandbox 新增可选 `status.upgradeProgress`：`operationID`（SUO UID）、`revision`、`sourcePodUID`、`startedAt`、`deadline`、`preUpgrade`、`postUpgrade`。进度为执行事实，不进入 SandboxSet/SandboxTemplate 的池模板；已核对其 Sandbox 构造路径，无需复制操作级字段。
- hook 前落盘 Running，成功后单独落盘 Succeeded；成功记录防同轮重跑，未知结果停止自动重放，不承诺外部副作用 exactly-once。同 UID 意外更改活动目标不清空预算或执行记录，补救要求新 SUO。
- Recreate 使用升级前 Pod UID 确认替换，即使旧原地更新已写入相同 hash，也不会把旧 Pod 当成替换成功。InplaceUpdate 修正现存 Pod 时不删除重建。
- deadline 到期采用当前步骤既有 Failed Reason，保持 Phase=Upgrading，不回滚、不自动 Recreate；无副作用 QoS 拒绝且旧 Pod 健康可保持 Running。
- SUO 与 Sandbox 通过内部 `upgrade-operation` 注解下发 UID，删除旧 SUO 保留 UID，避免删除本身重启生命周期。SUO 只汇总已接纳当前 UID 的结果，失败继续占 maxUnavailable。状态及清理写入带 resourceVersion，避免旧结果覆盖新操作。

### 兼容与验证边界

新增 API 字段可选；部署时需更新生成的 CRD。已有正在执行、没有进度记录的升级在首次接纳时建立预算；历史无操作 UID 的直接更新保留原恢复 feature gate。缺少 kubelet 观察 generation 的旧集群不能可靠归属 resize 失败时，保守等调用方预算，不将旧失败直接用于新目标。跨运行时的镜像引用/Pod status 行为需专门集群验证，本轮普通验证不运行 E2E。

#### Claim QoS rejection preserves probe synchronization

This is a compatibility boundary of the existing claim-time path, not a behavior of the SUO `InplaceUpdate` strategy. When a claim-time QoS pre-check rejects a target, the controller reports `InplaceUpdate=False/Failed` and does not issue an in-place image or resource update. It nevertheless continues the pre-existing outer `EnsureProbe` and Pod Ready synchronization flow.

Consequently, if that same template change also updates `spec.probes` or `autoPausePolicy`, the old Pod's probe annotation and probe-related conditions can be synchronized even though its image and resources stay unchanged. This deliberately preserves Claim probe maintenance and AutoPause behavior; it is not an unhandled QoS rejection. A future requirement for a rejected target to make no Pod writes of any kind would need separate design and compatibility evaluation, rather than adding a QoS-specific Probe bypass to this SUO change.

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

Both paths share the same low-level engine, `handleInPlaceUpdateCommon`, so the
actual pod-patching semantics (QoS pre-check, resize handling, multi-round
state annotation rebuild) stay in one place. The engine only establishes facts
and reports terminal failures as classified errors; each path keeps a thin
adapter (`handleClaimInplaceUpdate`, `performInplaceUpgrade`) that dispatches
those errors onto its own reporting channel with `errors.Is`/`errors.As`.

The engine is fail-stop: when the target revision changes while a previous
round is still in progress, it waits instead of re-patching for the new
target, and passes the wait reason from pod status (e.g. `ImagePullBackOff`)
through to each path's condition message. It never attempts to self-rescue a
stuck round — both paths have their liveness exits outside the engine: the
claim path times out and replaces the sandbox from the pool, and the upgrade
path is recovered by deleting the SUO and creating a
`Recreate`/`CheckpointRestore` SUO (see Risk 5).

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
condition. This matters because `handleClaimInplaceUpdate` leaves the condition
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
- **Unsupported change** (missing template-hash label, or a template change
  beyond images/resources/metadata) → terminal failure. The engine reports
  these as classified errors; the claim-path adapter maps them onto the
  `InplaceUpdate` condition, while this adapter turns them into a `failMsg` so
  the state machine does not read the untouched pod as success.
- **In progress** → `done=false`; the sandbox stays in `Upgrading`.
- **Terminal engine failure** (QoS change, kubelet-side terminal failure,
  corrupted state annotation, resize not supported) → terminal failure,
  surfaced as `UpgradePodFailed` with the classified error's message.
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

### Risk 1: Leftover Upgrade Policy and Lifecycle Hooks Persist After an Ops Completes

`spec.upgradePolicy` persists after an ops completes. If a sandbox previously
upgraded by an `InplaceUpdate` ops is later reused through a `SandboxClaim` with
`spec.inplaceUpdate`, the claim-time change will now run through the `Upgrading`
phase, and sandbox-manager's `GetSandboxState` will not see it as `Running`
until the upgrade finishes.

`spec.lifecycle` has the same residual. `applySandboxPatch` overwrites the
sandbox Lifecycle with the ops hooks (or clears it to `nil` when the ops sets
none), but `handleDeletion` only removes the ops tracking label and the
resume-trigger annotation — it never restores or clears `spec.lifecycle`. The
deleted SUO's hooks therefore linger on the sandbox and can fire as "ghost
hooks" during a later upgrade that does not set Lifecycle (a subsequent SUO, or
a claim-driven in-place update), running pre/post hooks the operator never
intended for that upgrade.

Both residuals are inherent to expressing per-ops configuration through sandbox
`spec` fields that the deletion path does not unwind, and the policy residual
applies equally to `Recreate` today. Mitigation: claim pools are managed by
`SandboxSet` and are not the intended target of operator-driven SUO upgrades.
If the combination becomes real, the claim path should clear or override the
policy and lifecycle — which is part of the deferred sandbox-manager work. A
targeted controller-side fix is for `handleDeletion` to also clear
`spec.upgradePolicy` and `spec.lifecycle` so no per-ops configuration outlives
the ops that set it.

### Risk 2: In-Place Upgrade of a Paused Sandbox Degrades to Pod Creation

A paused sandbox has no pod, so there is nothing to patch. `performInplaceUpgrade`
creates a pod from the current template instead. The resulting pod already runs
the target revision, so the outcome is correct, but it is a pod creation rather
than a true in-place update. This is strictly better than failing the upgrade and
matches what `Recreate` would do.

### Risk 3: Stale InplaceUpdate Condition Across Rounds

`handleClaimInplaceUpdate` does not always overwrite the `InplaceUpdate`
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

### Risk 5: Any In-Place Correction of a Stuck Unpullable-Image Update Deadlocks

**Do NOT recover a stuck unpullable-image `InplaceUpdate` by deleting the SUO
and creating a new `InplaceUpdate` SUO that rolls the image back or fixes it
to a different image. Every variant deadlocks in `Upgrading`.**

When an in-place image update targets an unpullable image, the kubelet keeps
the old container running while retrying the pull, so the container never
leaves the pre-update image. Completion of an in-place update is judged by an
ImageID change against the pre-patch baseline (`isPodImageUpdateCompleted`),
and the container never restarts to produce that change:

- **Rolling back to the running image**: the target equals the image the
  container is already running, so the kubelet sees no image change at all.
  The current ImageID is recorded as the new baseline and can never differ
  from it.
- **Fixing to a different pullable image**: the kubelet does not restart a
  container stuck in `ImagePullBackOff` just because `spec.image` changed
  again (verified on K8s 1.32 by E2E — the spec was patched to a pullable
  image but the container kept running the old one until the SUO timed out).

The engine therefore does not even deliver the corrective patch while the
stuck round is in flight (fail-stop): the sandbox stays in `Upgrading`, and
the wait reason from pod status (e.g. `ImagePullBackOff` with the kubelet's
message) is passed through on the `Upgrading` condition message so the user
can see why and pick the recovery path. The only supported recovery path is a
pod replacement strategy:

1. Delete the stuck SUO and create a `Recreate` (or `CheckpointRestore`) SUO.
   The pod is replaced, so even the original image works as a rollback target.
   Covered by E2E in `test/e2e/inplaceupdate_upgrade_test.go`.

### Risk 6: Stale InplaceUpdate Condition on the Claim Path

Risk 3 covers the SUO path, where Change 4 mitigates stale `InplaceUpdate`
conditions by clearing them on `Upgrading` phase entry. The claim-time path
(`!RequiresUpgradePhase`, sandbox stays `Running`) never enters that phase, so
the cleanup never fires. A terminal failure on the claim path — e.g. a QoS-class
change rejected by `CheckResizeQoSChange` — writes
`InplaceUpdate=False/Failed` without mutating the pod
(`common_inplace_update_handler.go:204-219`). If the template is then rolled
back to a revision the pod already matches, the hash-match branch hits the
terminal short-circuit (`isInplaceUpdateTerminal`, `:318-329`) and returns
without rewriting the condition; a subsequent metadata-only change takes the
metadata-patch branch (`:179-202`) which also never touches the condition. In
both cases a stale `Failed` persists on a sandbox whose pod and template are
fully consistent.

Functional delivery is **not** blocked: every ready-gate waits only on the
transient `InplaceUpdating` reason, never on `Failed`
(`infra/sandboxcr/claim.go:960-964`, `pkg/cache/tasks.go:123-126`). The impact
is observability noise: `sandboxReadyFailureMessage` surfaces the stale
`Failed` in its diagnostic string (`claim.go:908-910`), and
`sandbox_status_inplace_updating` reports `1` for the sandbox indefinitely
because it does not distinguish `Failed` from `InplaceUpdating`
(`metrics.go:527-534`), also leaking the `inplaceUpdateStartTimes` entry until
the sandbox is deleted.

This is listed under Risks rather than fixed because the terminal short-circuit
guards a real case: a resize-subresource failure leaves `spec == status`, which
would make `isPodResourceResizeCompleted` falsely report completion if the
short-circuit were removed. A correct fix needs a pod-vs-template consistency
precheck before `isInplaceUpdateTerminal` so that a consistent pod clears the
stale condition while a resize-failure residual keeps it. That is more than a
one-line change and is out of scope for this proposal (changing the claim path
is a Non-Goal).

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
  - `handleClaimInplaceUpdate`: verify a completed or terminally failed previous
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
