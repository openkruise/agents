---
title: Sandbox Pre-Pause and Post-Resume Lifecycle Hooks
authors:
  - "@ywExcellent"
reviewers: []
creation-date: 2026-10-21
last-updated: 2026-10-21
status: provisional
see-also:
  - "/docs/proposals/20260626-sandbox-auto-pause.md"
  - "/docs/proposals/20260627-wake-on-traffic-via-spec-patch.md"
  - "/docs/proposals/20251218-sandbox-inplace-update.md"
---

# Sandbox Pre-Pause and Post-Resume Lifecycle Hooks

## Table of Contents

- [Glossary](#glossary)
- [Summary](#summary)
- [Motivation](#motivation)
  - [Current Pause and Resume Flow](#current-pause-and-resume-flow)
  - [Existing Hooks](#existing-hooks)
  - [Problems](#problems)
- [Goals](#goals)
- [Non-Goals and Follow-up Goals](#non-goals-and-follow-up-goals)
  - [Non-Goals](#non-goals)
  - [Follow-up Goals](#follow-up-goals)
- [Proposal](#proposal)
  - [User Stories](#user-stories)
  - [API Design](#api-design)
  - [Configuration Propagation](#configuration-propagation)
  - [PrePause Execution Semantics](#prepause-execution-semantics)
  - [PostResume Execution Semantics](#postresume-execution-semantics)
  - [Status and Conditions](#status-and-conditions)
  - [Failure and Retry Semantics](#failure-and-retry-semantics)
  - [Concurrency and Cancellation](#concurrency-and-cancellation)
  - [Interactions with Existing Features](#interactions-with-existing-features)
  - [Security and Output Handling](#security-and-output-handling)
- [Implementation Design](#implementation-design)
  - [Layering and Responsibilities](#layering-and-responsibilities)
  - [Shared Hook Executor](#shared-hook-executor)
  - [Controller State Machine Changes](#controller-state-machine-changes)
  - [SandboxSet and SandboxTemplate](#sandboxset-and-sandboxtemplate)
  - [SandboxUpdateOps](#sandboxupdateops-1)
  - [Pause Wait Deadline](#pause-wait-deadline)
- [Upgrade and Compatibility](#upgrade-and-compatibility)
- [Risks and Mitigations](#risks-and-mitigations)
- [Alternatives](#alternatives)
- [Test Plan](#test-plan)
- [Implementation Plan](#implementation-plan)
- [Proposal History](#proposal-history)

## Glossary

- **Pause**: Set `Sandbox.spec.paused` to `true` and let the Sandbox controller converge to the Paused state.
- **Resume**: Set `Sandbox.spec.paused` to `false` and let the Sandbox controller recreate or restore the Pod and complete runtime initialization.
- **PrePause Hook**: A user command executed in the still-live Sandbox environment before the Sandbox actually performs a checkpoint or deletes the Pod.
- **PostResume Hook**: A user command executed after the resumed Pod is Ready and runtime and dynamic storage initialization have completed, but before the Sandbox becomes externally Ready.
- **Hook cycle**: One complete Running → Paused → Running lifecycle.
- **Runtime initialization**: The existing post-resume agent-runtime `/init`, security Token re-issuance, and CSI dynamic mount restoration flow.
- **Normal Resume**: A resume operation that restores the workload for use, such as a user Resume, Connect, scheduled Resume, or traffic-driven wake-up.
- **Upgrade Resume**: An internal resume step performed to upgrade a Paused Sandbox.

## Summary

This proposal adds two optional user lifecycle hooks to Sandbox: `prePause` and `postResume`.

`prePause` runs while the Pod is still running, before a Hibernate checkpoint and Pod deletion, allowing applications to flush files, close external connections, commit transactions, or save application state. `postResume` runs after the resumed Pod is Ready, agent-runtime has been reinitialized, security Tokens have been re-issued, and CSI mounts have been restored. It completes before the Sandbox becomes externally Ready and traffic resumes, allowing applications to rebuild connections, restart schedulers, or validate application state.

Both hooks are executed by the Sandbox controller. Manual Pause/Resume, timeout-driven auto-pause, probe-driven pause and resume, Connect, and wake-on-traffic share the same semantics. This proposal runs hooks through the existing agent-runtime/envd command execution capability, by default as the container's own user (the envd default user) with an optional explicit `user` override. It permits retries and repeated execution when results are uncertain, and requires user commands to be idempotent.

This proposal adopts asymmetric failure policies: after bounded retries are exhausted, `prePause` records a Warning and allows pause to continue, so a user script cannot retain the Pod indefinitely. A failed `postResume` retains the Pod, retries with backoff, and keeps Ready=False so application traffic cannot resume prematurely. Once a pause has entered the controller state machine, it must complete first; no transition is added to cancel pause midway.

## Motivation

### Current Pause and Resume Flow

The current public Pause/Resume flow for the Kubernetes backend is:

```text
API / Manager
  → atomically update Sandbox.spec.paused
    → Sandbox controller
      → Pause: delete Pod
      → Resume: create Pod, wait for Ready, reinitialize runtime/Token/CSI
```

Stop Pause currently proceeds as follows:

```text
spec.paused=true
  → SandboxPaused=False
  → delete Pod
  → Pod gone
  → SandboxPaused=True
```

Resume currently proceeds as follows:

```text
spec.paused=false
  → Resuming
  → create Pod
  → Pod Running / Ready
  → runtime init
  → security token re-issue
  → CSI remount
  → Running / Ready
```

The API already defines `PauseStrategy=Hibernate` and checkpoint helper capabilities, but the current public upstream controller still performs Stop Pause through common control; Hibernate checkpoint pause has not been wired into the actual dispatch path. This proposal defines a consistent ordering of hooks relative to Stop and Hibernate, without assuming that Hibernate is already implemented.

### Existing Hooks

Currently, `SandboxLifecycle` contains only upgrade hooks:

```go
type SandboxLifecycle struct {
    PreUpgrade  *UpgradeAction `json:"preUpgrade,omitempty"`
    PostUpgrade *UpgradeAction `json:"postUpgrade,omitempty"`
}
```

Upgrade hooks execute inside the Sandbox through agent-runtime/envd, support exec commands and timeouts, and currently always run as root.

Users can also configure native Kubernetes `postStart` / `preStop` hooks in the PodTemplate, but these belong to the Pod lifecycle:

- `preStop` runs only when the Pod terminates and is constrained by the termination grace period. Stop Pause currently explicitly sets the deletion grace period to 5 seconds, which is unsuitable for a longer flush operation.
- `postStart` runs both on initial creation and whenever a Pod is created during resume, so it cannot distinguish initial startup from Resume.
- Under memory-persistent pause/resume (checkpoint/restore), containers are neither terminated nor restarted: the container processes are frozen and dumped on pause and restored from the checkpoint on resume, so neither preStop nor postStart executes at all.
- They are not explicit steps in the Sandbox state machine, and their failures cannot be directly represented in Pause/Resume Conditions.

### Problems

1. Applications cannot proactively flush files and application state before a Hibernate checkpoint.
2. Stop Pause can only rely on Kubernetes preStop and lacks a separate, bounded application-state flush step before Pod deletion.
3. Users cannot run initialization after runtime, Token, and CSI restoration during Resume.
4. Wake-on-traffic may admit requests before the application recovery script runs.
5. Pod postStart cannot distinguish initial startup from Resume.
6. Higher-level platforms must assemble Pause/Resume and remote command calls themselves, without unified retry, status, or concurrency semantics.

## Goals

- Add optional `prePause` and `postResume` fields to `SandboxLifecycle`.
- Share PrePause semantics across all normal Pause sources.
- Share PostResume semantics across all normal Resume sources.
- Allow checkpoint or Pod deletion only after the terminal PrePause result—success, skip, or failure after bounded retries—has been persisted; hook failure must not block pause indefinitely.
- Allow the Sandbox to become externally Ready and serve routes only after PostResume succeeds.
- Make hook failures observable through Conditions and Events.
- Reuse the existing agent-runtime/envd transport, including Runtime TLS.
- Allow SandboxSet to declare pause/resume hooks for pool-created instances, and SandboxTemplate to retain those hooks for checkpoint/clone creation; distinguish these sources explicitly.
- Restore pool-level hooks after recycle to prevent configuration leakage from the previous tenant.
- Preserve existing behavior for Sandboxes without hooks.

## Non-Goals and Follow-up Goals

### Non-Goals

This proposal does not provide an exactly-once guarantee, add Prometheus metrics, implement a Hibernate backend or checkpoint provider, or replace Kubernetes preStop/postStart with Sandbox hooks.

### Follow-up Goals

The following capabilities are outside this proposal and are listed as possible follow-up goals, without prescribing interfaces or promising implementation:

- Support HTTP/Webhook hooks.
- Support hook environment variables, Secret injection, and output persistence.
- Select hooks by Pause/Resume source or trigger conditions.
- Support multiple hooks, DAGs, and parallel execution.
- Support dynamic hook configuration through E2B create requests.
- Support user-configurable retry/failure policies. Allowing PrePause failure to block pause would also require designing pause cancellation, automatic pause threshold resets, and a resource reclamation safety net.
- Support persistent attempt counts, execution identity, and task records if strict cross-restart retry limits or stale-result validation are required.
- Overlap runtime initialization (and therefore PostResume) with the tail of Pod startup instead of waiting for the whole Pod to be Ready, reducing resume and traffic-wake latency; the current sequencing is retained because PostResume ordering is defined on top of it.
- Introduce independent hook scheduling to reduce the time long-running hooks occupy controller workers.
- Improve coordination between hooks and external checkpoints to provide strictly serialized execution boundaries.
- Remove the old `UpgradeAction` name after defining a deprecation period and compatibility arrangements. This proposal only marks it as deprecated and retains the alias.

## Proposal

### User Stories

#### Story 1: Flush Application State Before Pause

A user runs an Agent with a local cache and wants to synchronize data to a PVC before pausing:

```yaml
spec:
  lifecycle:
    prePause:
      exec:
        command: ["/bin/bash", "-c", "/workspace/flush.sh"]
      timeoutSeconds: 10
```

The controller runs the flush before deleting the Pod. Transient failures are retried; if retries still fail, it records a Warning and continues pausing. Users should save state with techniques such as writing a temporary file followed by an atomic rename, to avoid partially written files when the hook times out. This proposal does not guarantee that the flush succeeds. In Hibernate mode, the checkpoint for this cycle also follows hook processing, and hook failure does not bypass the checkpoint's own success checks.

#### Story 2: Rebuild Connections After Resume

A user's Agent needs to rebuild database connections after CSI remounting and injection of a new Token:

```yaml
spec:
  lifecycle:
    postResume:
      exec:
        command: ["/bin/bash", "-c", "/workspace/reconnect.sh"]
      timeoutSeconds: 10
```

The Sandbox is not externally Ready during resume; route serving resumes only after the hook succeeds.

#### Story 3: Auto-Pause and Traffic-Driven Wake-Up

A probe determines that the Agent is idle and sets `spec.paused=true`; the controller automatically runs PrePause. When sandbox-gateway later receives traffic and triggers Resume, the controller allows the original request to be forwarded only after PostResume completes.

### API Design

This proposal makes `LifecycleAction` the canonical lifecycle action type, retaining the existing action structure. New hooks, existing upgrade hook fields, and internal execution code all use the new name. `UpgradeAction` remains as a deprecated alias for existing Go consumers; there are not two independent action structures:

```go
type SandboxLifecycle struct {
    // PrePause is best-effort; exhausted retries allow pause even on failure.
    // Applications must not rely on hook success for correctness.
    // +optional
    PrePause *LifecycleAction `json:"prePause,omitempty"`

    // PostResume may repeat and gates Ready after runtime/credential/storage init.
    // +optional
    PostResume *LifecycleAction `json:"postResume,omitempty"`

    // PreUpgrade runs before a Pod replacement upgrade.
    // +optional
    PreUpgrade *LifecycleAction `json:"preUpgrade,omitempty"`

    // PostUpgrade runs after a Pod replacement upgrade.
    // +optional
    PostUpgrade *LifecycleAction `json:"postUpgrade,omitempty"`
}

type LifecycleAction struct {
    // +optional
    Exec *corev1.ExecAction `json:"exec,omitempty"`

    // User is the execution identity. Empty follows the envd default user
    // (the user delivered to the runtime via /init, typically the image USER),
    // so a non-root container runs its hooks as that non-root user by default.
    // +kubebuilder:validation:MaxLength=32
    // +kubebuilder:validation:Pattern=`^[a-zA-Z_][a-zA-Z0-9_-]*\$?$`
    // +optional
    User string `json:"user,omitempty"`

    // +kubebuilder:default=60
    // +optional
    TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// Deprecated: use LifecycleAction.
type UpgradeAction = LifecycleAction
```

`LifecycleAction` retains the JSON structure of the original `UpgradeAction` and adds `user`: `exec.command` is executed directly, with the first element as the executable and the remaining elements as arguments. A shell must be requested explicitly, for example with `["/bin/bash", "-c", "..."]`. `timeoutSeconds` retains the existing default of 60 seconds and specifies the timeout of a single execution, not the wait deadline for the entire Pause/Resume operation. The type migration does not change YAML/JSON fields or require consumers to immediately change the old Go type name.

Execution identity:

- Pause/resume hooks default to an empty `user`: the executor sends no per-call user credential and envd resolves the default user delivered via `/init` (typically the image USER). A non-root container therefore runs its hooks as that non-root user; only when no user was ever delivered does envd fall back to its built-in `root`.
- An explicitly configured `user` is passed through per call, enabling deliberate cross-identity execution for future scenarios.
- Upgrade hooks keep their existing behavior: the executor continues to run them explicitly as root, because backup/restore of the whole workspace regularly needs cross-owner file access. The shared `user` field is therefore available on upgrade hooks structurally, but this proposal does not change the upgrade executor's root default.
- The runtime rejects unknown usernames at execution time (hook failure), matching the behavior of the existing E2B command-execution user parameter.

This proposal supports exec only. Configured prePause/postResume hooks must have a non-empty `exec.command`, and pause/resume hook timeouts are limited to 1–60 seconds. Validation must be applied in the context of the new hook fields; adding a global upper bound to the shared `LifecycleAction` would unintentionally change existing upgrade hook behavior. A non-empty `user` must match the username pattern above.

The repository currently has no Sandbox validating webhook, so validation of direct Sandbox creation and updates must use CRD schema/CEL. SandboxSet/SandboxTemplate likewise use schema validation. Resource-level validation limits the shared structure to the following fields:

| Resource | Allowed lifecycle hooks |
| --- | --- |
| Sandbox | prePause, postResume, preUpgrade, postUpgrade |
| SandboxSet | prePause, postResume only |
| SandboxTemplate | prePause, postResume only |
| SandboxUpdateOps | preUpgrade, postUpgrade only |

CEL rejects preUpgrade/postUpgrade on SandboxSet/SandboxTemplate; sharing SandboxLifecycle does not make upgrade hooks pool-level policy. The existing SandboxUpdateOps webhook rejects prePause/postResume. Sandbox upgrade hooks remain instance policy, which UpdateOps may overwrite according to the field-level merge rules below.

### Configuration Propagation

PrePause/PostResume are instance runtime policies and should not exist only on directly created Sandboxes.

The following fields should be added or completed:

```go
type SandboxSetSpec struct {
    // ...
    Lifecycle *SandboxLifecycle `json:"lifecycle,omitempty"`
}

type SandboxTemplateSpec struct {
    // ...
    Lifecycle *SandboxLifecycle `json:"lifecycle,omitempty"`
}
```

Requirements:

- `NewSandboxFromSandboxSet` deep-copies the SandboxSet Lifecycle.
- Both inline and templateRef effective-spec construction paths include the SandboxSet Lifecycle.
- Lifecycle participates in the SandboxSet revision hash. Changing the pool-level Lifecycle triggers a rolling update of unclaimed pool members and does not silently modify claimed instances.
- Recycle restores Lifecycle from the current SandboxSet and clears execution state from the previous cycle, including PostResumeHook.
- Both checkpoint-generated SandboxTemplates and clone-created Sandboxes explicitly deep-copy prePause/postResume, without inheriting preUpgrade/postUpgrade that UpdateOps may have written to the source instance.
- Instance modifications must not mutate maps, slices, or pointers in informer cache objects.

SandboxTemplate follows the repository rule that every pool-level field participating in the SandboxSet revision must have a corresponding representation in SandboxTemplateSpec. However, configuration sources must be distinguished:

- SandboxSet with templateRef retains the existing convention: PodTemplate comes from the template, while pool-level policies such as Lifecycle come from SandboxSet. The template's Lifecycle is not an automatic fallback when SandboxSet leaves it unset.
- The checkpoint/clone paths directly use the prePause/postResume saved in SandboxTemplate as an independent effective configuration source.
- A direct Sandbox reference to a template must not be assumed to inherit every pool-level field. For direct Sandboxes, this proposal continues to take hooks from Sandbox.spec.lifecycle.

The source of truth for each creation path is:

| Creation path | Lifecycle source |
| --- | --- |
| Direct Sandbox with an inline PodTemplate | Sandbox.spec.lifecycle |
| Direct Sandbox with templateRef | Sandbox.spec.lifecycle; no template fallback |
| Inline SandboxSet | SandboxSet.spec.lifecycle |
| SandboxSet with templateRef | SandboxSet.spec.lifecycle; no template fallback |
| User-managed SandboxTemplate referenced by a SandboxSet | Its Lifecycle does not propagate to the pool; use SandboxSet.spec.lifecycle |
| Checkpoint clone | prePause/postResume stored in the checkpoint's SandboxTemplate |

Thus, "SandboxTemplate changes trigger rolling updates" applies only to fields that actually enter the effective spec, such as PodTemplate. Changing its Lifecycle alone must not affect the pool revision driven by SandboxSet's own Lifecycle.

### PrePause Execution Semantics

PrePause applies consistently to the following Pause sources:

- E2B Pause.
- Directly setting `spec.paused=true`.
- `PauseTime` expiration.
- Probe-driven auto-pause.
- Other normal Pause operations triggered through desired state.

Recommended order:

```text
Pause accepted / spec.paused=true
  → initialize SandboxPaused=False / Ready=False
  → wait for an existing in-progress Checkpoint
  → execute PrePause once per reconcile, with up to 3 attempts while retry state is retained
  → persist PrePauseHook Succeeded / Failed / Skipped
  → recheck existing Checkpoint before the destructive step
  → Hibernate: create and wait for pause Checkpoint
  → delete Pod
  → Pod gone
  → SandboxPaused=True
```

Waiting for an observed in-progress Checkpoint must precede PrePause. After the hook finishes, check again before checkpointing or deleting the Pod. Both checks use informer-backed List to reduce concurrency risks, but they are not a mutual exclusion lock: a new Checkpoint can still be created after a check. This proposal does not extend the Manager checkpoint lock into the controller or guarantee strict serialization with arbitrary external checkpoint writers.

PrePause must precede any Hibernate checkpoint created by the controller itself, so completed hook changes are included in the snapshot. After retries fail, the normal Hibernate checkpoint flow still runs; it is hook failure that is allowed through, not checkpoint failure.

From entry into the Paused phase, keep Sandbox Ready=False and do not publish a Running route. This gate applies only to traffic through the unified gateway; it neither guarantees draining of established connections nor prevents direct Pod IP/Service access that bypasses the gateway.

Stop mode order:

```text
PrePause → Kubernetes preStop → Pod deletion
```

If the Sandbox has already completed pause (`SandboxPaused=True`), subsequent reconciles do not execute PrePause again.

If PrePause is configured but the Pod is already absent, terminal, or being deleted before an attempt, record PrePauseHook=False/Skipped and a Warning Event, then continue the existing pause convergence flow. Do not pretend the hook succeeded or block pause indefinitely because the execution environment is missing. Missing or failed Hibernate checkpoints remain governed by the checkpoint state machine and cannot be bypassed by skipping the hook.

### PostResume Execution Semantics

PostResume applies consistently to the following normal Resume sources:

- E2B Resume.
- Connect.
- Directly setting `spec.paused=false`.
- Probe-driven scheduled resume.
- Wake-on-traffic.
- Other normal Resume operations triggered through desired state.

Recommended order:

```text
spec.paused=false
  → create/restore Pod
  → Pod Running and Ready
  → runtime /init
  → security token re-issue
  → CSI remount
  → execute PostResume
  → PostResumeHook=True
  → Sandbox Ready=True
  → route state becomes Running
  → Resume/Connect/Wake completes
```

PostResume does not run on initial Sandbox creation; it runs only during a normal Paused → Running resume cycle.

The current controller sets phase to Running after Resuming, then performs runtime initialization in `EnsureSandboxUpdated`. This proposal retains that internal phase structure and requires:

- Run PostResume after RuntimeInitialized succeeds, and recheck that the current Pod is executable and Ready before each attempt.
- Keep Sandbox Ready=False until PostResume succeeds.
- For a live claimed Sandbox undergoing PostResume, `GetSandboxState`, E2B describe/list, and route projection report `paused` until Ready=True; internal phase remains Running. Ready=False with reason PostResumeHook identifies this gate, including the interval between persisted hook success and publishing Ready. Publishing Ready changes its reason back to the ordinary PodReady reason, so a retained result Condition does not reactivate the gate on later unrelated NotReady states. Do not change mappings for actual termination, deletion, or unrelated NotReady causes.
- ResumeTask already waits for Ready and does not need a different completion criterion.
- `IsSandboxPausable` allows Pause for a live instance waiting for or retrying PostResume, so deletion is not the user's only option. This relaxation applies only to normal resume instances with an incomplete PostResumeHook state for the current cycle.
- Persist hook success first, then publish Ready in the next reconcile, to avoid admitting traffic based on stale desired state.

### Status and Conditions

#### PrePause

Add a separate Condition for the hook result:

```go
SandboxConditionPrePauseHook SandboxConditionType = "PrePauseHook"
SandboxConditionPostResumeHook SandboxConditionType = "PostResumeHook"
```

PrePauseHook uses Pending, Running, Succeeded, Failed, and Skipped reasons. Status=True means success; all other reasons use Status=False. A retryable failure returns to Pending with an attempt number and a safe error summary, for example `attempt 2 failed: timeout; retrying`. Failed means the current controller has exhausted its three-attempt budget; counts can restart after controller restart or leadership change. The message is diagnostic only: the controller does not parse it to restore counters, identify an execution, or make retry decisions.

SandboxPaused continues to represent checkpoint/Pod deletion progress only. It does not store the hook result in its reason or message. After a failed hook and a completed Stop Pause, both facts remain machine-readable:

```yaml
status:
  phase: Paused
  conditions:
    - type: PrePauseHook
      status: "False"
      reason: Failed
      message: "attempt 3 failed: timeout; pause continues"
    - type: SandboxPaused
      status: "True"
      reason: StopPauseSucceed
```

Persist the hook terminal result before the next reconcile proceeds to checkpoint/delete. Retain PrePauseHook through pause completion and resume until the next Pause cycle replaces it. Checkpoint processing must not overwrite it; no special preservation of the hook's message in CheckpointControl is required. Events supplement the Condition rather than being the only record of failure.

#### PostResume

PostResumeHook uses Pending, Running, Succeeded, and Failed reasons. Status=True means success; pending, running, and failure use Status=False. Unlike PrePause, Failed does not exhaust a retry budget; PostResume continues retrying while Ready=False. Its message may include the locally observed attempt number and safe failure summary; it is not a durable total across controller restarts.

```yaml
status:
  phase: Running
  conditions:
    - type: RuntimeInitialized
      status: "True"
      reason: Succeeded
    - type: PostResumeHook
      status: "False"
      reason: Failed
      message: "attempt 2 failed: timeout; retrying"
    - type: Ready
      status: "False"
      reason: PostResumeHook
```

On entry to a new Pause or Resume, clear the corresponding previous Hook Condition and in-memory retry state; create Pending only if the hook is configured. Normal resume with PostResume sets Ready=False/reason=PostResumeHook before running initialization and the hook. Initial creation does not create this resume hook marker or execute PostResume. Publish Ready=True/reason=PodReady after hook success and retain its result Condition for observation. Do not gate readiness on the mere presence of a historical Hook Condition. Phase=Paused or Running alone does not indicate operation completion: pause completes at SandboxPaused=True, and normal resume completes at Ready=True.

#### Retry State and Configuration Changes

No execution-record fields or new status structures are added to SandboxStatus. Conditions retain hook progress and outcomes; attempt counts and next-retry times remain in the controller's memory, keyed by Sandbox UID and hook kind, and are reset on an observed new lifecycle operation or deletion. Repeated API calls and ordinary reconciles do not reset that state.

Messages record diagnostic information such as `attempt 2 failed: timeout; retrying` or `attempt 3 failed: timeout; pause continues`. They are not parsed as counters, execution identities, or deadlines. Hook Conditions may set ObservedGeneration to the generation observed for the attempt; this field is observational and must be updated correctly, not used as a strict identity check for unrelated spec changes.

Each attempt uses the hook configuration and current Pod observed for that reconcile. A failed attempt may retry updated configuration without resetting the in-memory PrePause count. Once success or exhausted failure is persisted, that result ends hook processing for this operation; a later configuration change does not reopen it. Hooks added after an operation starts are not guaranteed to run in that operation and take effect reliably from the next corresponding lifecycle transition. There is no persisted command snapshot or digest, and no guarantee that a result certifies the latest configuration after concurrent changes.

A new resume resets previous PostResumeHook state before restoration so an old successful Condition is not reused. Hook calls use current Pod information, and existing Pod failure/recovery handling still applies; hook failure does not recreate the Pod. This proposal does not provide durable Pod/configuration execution identity or strict stale-result rejection across restart. Users needing those guarantees require the follow-up execution-status design.

Removing a hook stops future attempts and releases its gate once any active command has ended or passed its bounded termination boundary. Clear its Condition and local retry state when the operation ends. Recycle clears Hook Conditions and local state; checkpoint/clone propagates configuration only, never source execution results.

### Failure and Retry Semantics

#### PrePause Failure

- Execute at most once per reconcile. During uninterrupted controller operation, attempt PrePause at most 3 times, including the initial attempt. Maintain the count in memory, not in a new status field or by parsing Condition messages.
- After attempt 1 fails, store the next-retry time locally and return RequeueAfter=1s. After attempt 2 fails, return RequeueAfter=2s. Check the local next-retry time if informer events enqueue the Sandbox early; do not sleep inside a worker.
- Each attempt has a 1–60-second timeout. Three attempts and retry delays may take approximately 183 seconds in total, plus queueing and bounded cleanup, but no reconcile runs all three or sleeps between retries. The non-blocking execution limiter bounds concurrent worker occupancy.
- Until success, exhausted failure, or skip has been persisted in PrePauseHook, do not create this cycle's Hibernate checkpoint or delete the Pod; keep SandboxPaused=False and Ready=False. Handle known previous-process termination before the next attempt.
- A successful attempt sets True/Succeeded and may record `attempt 2 succeeded`. A retryable failure with attempts remaining sets False/Pending and records the attempt number and safe error summary. After the third failed attempt, set False/Failed, record `attempt 3 failed: timeout; pause continues`, and emit a Warning. A missing, terminal, or deleting Pod sets False/Skipped instead of pretending success.
- Persisted Failed or Skipped allows normal checkpoint/Pod deletion to continue; it does not bypass Hibernate checkpoint validation. A persisted terminal Condition is not retried simply because the controller restarts.
- Controller restart or leadership change can lose counters and backoff and restart counting for an incomplete hook. A persisted Running Condition has an uncertain outcome and may be retried. No strict three-attempt total, durable dispatch count, or reconstruction from message text is promised across interruptions; scripts must tolerate repetition and possible overlap.
- PrePauseHook=False/Failed is not a terminal error for Pause wait. E2B Pause returns success only after SandboxPaused=True. If the request times out first, only the wait ends; the accepted pause is not withdrawn.
- This proposal does not support cancellation midway through pause. If spec.paused is changed back to false, a pause that has entered the state machine still completes first, followed by a normal Resume that executes PostResume.

#### PostResume Failure

- Retain the Pod that has already been created.
- Keep Ready=False and do not publish a Running route.
- Set reason=`Failed`.
- Emit a Warning Event.
- The controller retries with backoff, with no total retry limit.
- Resume/Connect/Wake do not return success.
- Do not repeatedly delete and recreate the Pod because of hook failure; users may still actively pause or delete the instance.
- Execute PostResume at most once per reconcile. On failure, persist the Failed Condition with the locally observed attempt number and safe error summary, then return a retryable error or RequeueAfter to schedule controller backoff. Keep any next-retry time in memory and honor it while retained; restart may reset that backoff. Do not merely write an unchanged Failed Condition and return nil, since that does not guarantee continued retries.
- PostResume does not adopt PrePause's failure pass-through policy. Each attempt has a timeout, but application readiness continues to wait for success.

#### Execution Guarantees

This proposal permits retries and repeated execution when results are uncertain, and does not provide exactly-once execution. These replay semantics are often described as at-least-once, but PrePause is best-effort: a missing environment can be skipped and exhausted retries can be allowed through, so it does not guarantee at least one successful execution per cycle.

The controller persists Hook Conditions, not attempt reservations or execution identities. A crash after command success but before result persistence may repeat the command. PrePause success, exhausted failure, or skip must be persisted before checkpoint/delete; PostResume success must be persisted before publishing Ready. Attempt numbers in messages describe only observations by the current controller, not an authoritative lifetime execution count.

User hooks must be idempotent, and important persistent writes should use atomic commits. Before retrying after an ordinary timeout, confirm that a known old command has ended. When a controller crash or network interruption leaves the PID/result unknown, automatic avoidance of overlap cannot be guaranteed. Scripts for which overlap is a risk must also provide mutual exclusion inside the Sandbox, for example with a file lock. There is no persisted per-attempt record; incomplete hooks may retry and counters may restart after interruptions.

### Concurrency and Cancellation

- Retain the first-writer-wins spec patch semantics for Pause/Resume.
- Multiple Pause/Resume calls share the existing wait task.
- Only the controller executes hooks; API/Manager/Gateway do not execute them again.
- Do not add semantics that let Resume cancel pause during PrePause. The current API still rejects Resume for a pausing instance. Directly changing spec.paused=false likewise does not cancel a pause that has entered the state machine; normal resume follows pause completion.
- If the user sets spec.paused=true again during PostResume, the current command should end within a bounded time and the next reconcile should transition to Pause. Hook success must not incorrectly set Ready in the same reconcile.
- Separate success/failure pass-through results from subsequent destructive actions or Ready publication into different reconciles. Re-observe desired state using the existing resource-version expectations; do not add APIReader Get for this purpose.
- Do not start new hooks for a deleting Sandbox. The executor handles timeout/cancellation cleanup for commands already running, rather than assuming client disconnection naturally kills the process.
- Recycle does not execute Pause/Resume hooks.

This proposal does not guarantee strict mutual exclusion between hook modifications and external operations. Users requiring strong data consistency should make scripts idempotent and implement atomic commits/mutual exclusion inside the Sandbox.

### Interactions with Existing Features

#### Kubernetes postStart/preStop

Both kinds of hooks can coexist:

```text
Resume:
Pod postStart → runtime/CSI init → Sandbox postResume

Pause:
Sandbox prePause → Pod preStop → Pod deletion
```

#### AutoPausePolicy

Probe-driven auto-pause only sets `spec.paused=true`; PrePause is executed by the unified controller state machine.

Scheduled resume only sets `spec.paused=false`; PostResume is executed by the unified controller state machine.

#### Wake-on-Traffic

The gateway continues to reuse `sandboxcr.Sandbox.Resume()` and must not run hooks directly. ResumeTask already waits for Sandbox Ready, so adding PostResume to the Ready gate reuses this wait chain. Repeated Connect/Resume and traffic wake requests attach to the current resume operation: they do not reset Hook Conditions, retained in-memory attempt counts, or timeout settings owned by a winning request. The gateway calls Continue only after Resume succeeds and its refreshed route is Running; a paused route alone never admits traffic.

The gateway's current default wake timeout is 60 seconds, the same as the shared LifecycleAction's default single-attempt timeout. The actual restoration budget also includes Pod creation, runtime/Token/CSI initialization, and queueing, so there is no guarantee that a hook using its default timeout allows the first wake request to succeed. Deployments should explicitly set a shorter postResume timeout and leave enough wake timeout for the full restoration budget; the controller continues converging after a request times out. Examples in this proposal use 10 seconds without changing the action's existing default or the gateway's default configuration.

#### Hibernate

After Hibernate dispatch is wired in:

```text
PrePause success / exhausted retries / skipped → Checkpoint success → Pod deletion
Pod restore → Runtime init → PostResume success → Ready
```

Continuing to attempt a checkpoint after PrePause failure does not mean incomplete application transactions have been saved, or that checkpointing can repair partially written files or inconsistent external state.

#### Upgrade

An internal Resume used to upgrade a Paused Sandbox skips PostResume and retains the separate UpgradeControl state machine:

```text
Upgrade internal Resume
  → initialize old Pod when PreUpgrade requires it
  → PreUpgrade
  → Upgrade / initialize new Pod
  → PostUpgrade
```

UpgradeControl currently reuses handleResume directly without going through EnsureSandboxUpdated in the normal Running path. Therefore, no ResumePurpose type or extension to the Manager/Infra Resume interface is needed.

Both normal and internal resume clear previous PostResumeHook state on entry and create Pending only when postResume is configured. Initialize this marker once, not on every markResumeSucceeded call. Normal resume executes the hook in EnsureSandboxUpdated; UpgradeControl does not execute it internally. Clear the Pending marker and local retry state on upgrade success to avoid executing PostResume after returning to Running. If internal resume succeeds but the upgrade is abandoned midway and returns directly to Running, retain a configured hook's Pending marker, complete runtime initialization, and execute PostResume before publishing Ready. No execution-record fields are needed.

If a paused instance receives a normal Resume during upgrade, handle it according to the final workload path: when the upgrade continues to completion, PostUpgrade still finishes it; when the upgrade is abandoned in favor of normal workload resume, PostResume must gate readiness.

#### SandboxUpdateOps

`SandboxUpdateOps.spec.lifecycle` currently configures PreUpgrade/PostUpgrade. The existing applySandboxPatch overwrites Sandbox.spec.lifecycle entirely and sets it to nil when ops does not configure it. There is currently no automatic cleanup of upgrade hooks after completion.

Rules in this proposal:

- The SandboxUpdateOps webhook prohibits configuring prePause/postResume.
- The UpdateOps controller overwrites only PreUpgrade/PostUpgrade. When ops.lifecycle is nil, it clears only these two upgrade fields.
- Preserve the Sandbox's existing PrePause/PostResume.
- Do not add automatic cleanup of upgrade hooks after completion, to avoid widening compatibility changes.
- `spec.lifecycle` remains immutable after UpdateOps creation.

Changing pool-level hooks does not automatically modify claimed instances, and UpdateOps is not responsible for distributing new prePause/postResume configuration. Changing existing instances still requires directly updating their lifecycle.

### Security and Output Handling

Hooks run through agent-runtime/envd and inherit the current runtime HTTP/TLS selection and readiness checks.

Security constraints:

- Hooks run as the envd default user by default (empty `user`), or as an explicitly configured `user`; upgrade hooks continue to run as root for workspace-wide backup/restore.
- Do not expose dynamic configuration through E2B create.
- Only callers authorized to write Sandbox/SandboxSet/SandboxTemplate may configure hooks.
- Do not inject control-plane Kubernetes credentials into hooks.
- Do not add Secret parameters.
- Do not persist successful stdout in Conditions.
- Failure messages retain only exit codes, safe error categories such as timeout/transport, and truncated summaries. Do not treat arbitrary raw stdout/stderr as sanitized content suitable for persistence.
- Logs, Conditions, and Events in the Pause/Resume hook call chain must not record complete commands, env, output, Tokens, or mount credentials.
- Incomplete runtime TLS configuration causes hook failure, never a downgrade to plaintext. PrePause follows bounded failure pass-through; PostResume continues to block Ready.

Existing upgrade hooks include successful stdout in result messages and logs; the final success Condition clears its message. On failure, stdout/stderr are written to truncated Conditions/Events. The shared runtime process executor also has debug logging of complete results and accumulates output in memory, so truncating messages only in the hook wrapper is not sufficient to claim output safety.

Pause/Resume hooks need per-call options to disable output logging and limit captured output, without increasing output leakage from the shared executor. Existing upgrade hook output compatibility behavior is unchanged by this proposal.

## Implementation Design

### Layering and Responsibilities

#### API

`api/v1alpha1`:

- Name the canonical action type LifecycleAction, retain UpgradeAction as a Deprecated alias, and use the new name in both new and existing hook fields.
- Add the optional `user` field with pattern validation to LifecycleAction.
- Add separate PrePauseHook/PostResumeHook Conditions; do not add execution-record fields to SandboxStatus. Counts and failure summaries are diagnostic Condition messages, with retry bookkeeping in controller memory.
- Add Lifecycle to SandboxSetSpec/SandboxTemplateSpec.
- Generate DeepCopy and CRDs.

#### Admission

CRD schema/CEL and `pkg/webhook/sandboxupdateops`:

- Validate that exec is required and command is non-empty on the new hook fields of Sandbox/SandboxSet/SandboxTemplate, and that a non-empty `user` matches the username pattern.
- Reject preUpgrade/postUpgrade on SandboxSet/SandboxTemplate through resource-level CEL; Sandbox still permits all four hooks.
- Retain the timeout default of 60 seconds. Restrict only the new hooks to 1–60 seconds; existing upgrade hooks are unaffected by this range.
- Do not introduce a Sandbox validating webhook.
- The existing SandboxUpdateOps webhook prohibits prePause/postResume.
- Preserve existing UpdateOps lifecycle immutability.

#### Controller

`pkg/controller/sandbox`:

- Execute the state machine.
- Manage Conditions/Events.
- Complete a pause that has entered the state machine before responding to a normal Resume.
- Gate Ready on PostResume and allow active Pause during retries.
- Separate hook terminal status and subsequent actions into different reconciles.
- Keep hook lifecycle actions and execution logic in core.
- Do not execute hooks in Manager.

#### Manager/Infra/Gateway

- Keep Pause/Resume interfaces protocol-neutral.
- Manager does not interpret hooks.
- sandboxcr continues to patch spec and wait for the controller only.
- The gateway continues to call the unified Resume operation, without executing hooks.

### Shared Hook Executor

Reuse the existing LifecycleHookFunc/NewLifecycleHookFunc, which already have neutral names, and change their action parameters to `*agentsv1alpha1.LifecycleAction`. UpgradeAction is an alias for the same type, so existing callers using the old name remain compatible. Do not introduce a second execution interface for this proposal.

Preserve runtime transport resolution, TLS enforcement, the default 60-second timeout, and direct exec semantics. Pause/Resume wrappers pass the configured `user` per call (empty means no per-call credential, following the envd default user) and own their respective Conditions/Events, bounded or backoff retries, output policies, and trace spans; the upgrade wrapper keeps its explicit root credential.

Execution boundaries that must be completed:

- RunCommandWithRuntime currently applies a context timeout only to the client RPC and uses gRPC transport. Client disconnection cannot be treated as proof that the process inside the Sandbox has ended.
- The reference envd source separates process context from request context and reads Connect-Timeout-Ms as a server-side process timeout. Before implementation, validate against the envd version actually deployed by OKA that this header takes effect with the existing transport and terminates the entire process tree.
- Pause/Resume hook timeouts must also bound server-side process lifetime. When a PID is available, use a bounded independent cleanup context to terminate the command/process group and confirm it has ended. Retries must not simply close the old stream and immediately start a new command.
- The cleanup context must not directly reuse the already-timed-out command context. It must still respect the controller lifecycle and have an explicit cleanup deadline of its own.
- When the PID or execution result is unknown, termination of all child processes cannot be guaranteed. Rely on verified server-side execution deadlines to bound how long processes can remain, and document the limits around repeated/overlapping execution. If these capabilities cannot be confirmed, a client-side timeout alone is not sufficient grounds to consider the feature ready for deployment.
- Control output limits and suppression of output logging through per-call options, keeping the shared runtime capability neutral and avoiding process-global switches.

Hooks execute synchronously, at most once per reconcile; retries are scheduled through the workqueue, never by sleeping inside a worker. PrePause has up to 3 attempts while the current operation's in-memory retry state is retained; restart can reset that count. PostResume keeps retrying without a total limit. A single command still occupies a worker for up to 60 seconds plus bounded cleanup.

Use one controller-owned, non-blocking execution limiter shared by PrePause and PostResume. Its capacity is derived from sandbox-workers: max(1, floor(workers/4)). With the current default of 500 workers, at most 125 run these commands concurrently. If workers is greater than one, reserve worker capacity for ordinary lifecycle reconciliation; with one worker, even one synchronous command necessarily occupies it. This is an execution resource bound, not a feature switch or new user API.

When no slot is available, return RequeueAfter without waiting, consuming an attempt, or resetting hook state. Release the slot on every completion/error/cancellation path, after bounded process cleanup. On controller shutdown, stop starting commands, cancel active RPCs, perform bounded cleanup independently of an expired RPC context, and rely on the server-side command deadline for uncertain termination. No retry sleep remains to interrupt; local retry counts and schedules are not recovered from Condition messages.

These bounds protect runtime worker occupancy; integration tests still measure queueing and fairness under bulk pause/resume. They do not provide priority scheduling or guaranteed traffic-wake latency. An independent hook scheduler remains a follow-up goal.

### Controller State Machine Changes

#### Pause

Keep hook execution in core. The top-level reconciler handles phase entry, checkpoint observation, Condition persistence, and requeue scheduling:

```text
enter Pause
  → reset local retry state and previous PrePauseHook
  → persist PrePauseHook=False/Pending if configured, SandboxPaused=False, Ready=False

hook incomplete
  → wait for any observed existing checkpoint and local retry delay
  → try an execution slot without blocking
  → persist PrePauseHook=False/Running
  → execute one attempt and update the local count
  → persist Succeeded, retry Pending, or exhausted Failed/Skipped
  → message records the observed attempt number and safe result
  → return RequeueAfter for retries; no checkpoint creation or Pod deletion here

next reconcile after hook terminal status, or no configured hook
  → recheck existing checkpoint without overwriting PrePauseHook
  → control.EnsureSandboxPaused: checkpoint if applicable, then delete Pod
  → SandboxPaused=True only when the Pod is gone
```

Repeated API calls and ordinary informer events reuse retained retry state; an early event honors the in-memory next-retry time. A failed Running Condition write must not start a command, and a failed terminal write must not be followed by Pod deletion. If result persistence fails, a subsequent attempt may repeat the command; no exactly-once guarantee is introduced.

PrePauseHook retains its independent result throughout checkpoint waiting and pause completion. A persisted Succeeded/Failed/Skipped result ends this operation's hook processing, and is not rerun merely because a checkpoint is still pending. Clear local retry bookkeeping when pause finishes or the Sandbox is deleted. Controller restart can lose that bookkeeping for an incomplete hook; do not recover it by parsing message text.

#### Resume

On transition into resume, clear previous PostResumeHook and local retry state. For a configured hook, persist Pending and set Ready=False/reason=PostResumeHook before initialization; otherwise do not create a Hook Condition or execution record. Initial creation has no resume hook marker. Repeated markResumeSucceeded calls must not reset a marker already initialized for this resume.

EnsureSandboxUpdated drives the following order:

```text
RuntimeInitialized pending/failed
  → wait current Pod Ready
  → Initialize
  → persist RuntimeInitialized=True

PostResumeHook for this resume is Pending/Running/Failed
  → honor the local retry delay and recheck current Pod Ready
  → try an execution slot without blocking
  → persist PostResumeHook=False/Running
  → execute one attempt with the latest observed configuration
  → persist Succeeded/Failed with an observed attempt number and safe summary
  → keep Ready=False and return; schedule backoff after failure

next reconcile with persisted success, or no configured hook
  → re-observe desired state and current Pod readiness
  → probe/update handling
  → publish Ready=True/reason=PodReady and clear local retry state
```

Prevent syncStatusFromPod from mapping PodReady=True to Sandbox Ready=True while this resume's Hook Condition is pending/failed. Use current Pod information for execution; do not repeat initialization that remains completed for that Pod. Retry failures use newly observed commands, but a persisted successful result ends the hook step without a durable Pod/configuration identity check.

Retain the successful Hook Condition for observation, not as an active-operation record. Ready's PostResumeHook reason is cleared on normal completion, so later unrelated NotReady or configuration changes do not reopen the completed resume. Clear a configured hook's Pending marker after upgrade success; retain it if an abandoned upgrade returns to normal Running. Removing a hook releases its gate once any active command has ended or passed its bounded termination boundary. No new status fields are required.

### SandboxSet and SandboxTemplate

Add Lifecycle to the revision hash as pool-level behavior, with test coverage for these paths:

- Inline SandboxSet.
- TemplateRef SandboxSet.
- SandboxSet Lifecycle changes trigger rolling updates; templateRef Lifecycle neither overrides SandboxSet nor independently triggers pool rolling updates.
- SandboxTemplate PodTemplate changes still participate in revisions through the existing effective spec.
- Propagate prePause/postResume in both checkpoint template recording and clone creation, without copying upgrade hooks.
- Deep copying in NewSandboxFromSandboxSet.
- Recycle reset.
- Claimed Sandboxes are not silently overwritten by later pool changes.

### SandboxUpdateOps

UpdateOps application merges by field and handles existing inputs where Sandbox/ops.lifecycle may be nil:

```go
lifecycle := sandbox.Spec.Lifecycle.DeepCopy()
if lifecycle == nil {
    lifecycle = &agentsv1alpha1.SandboxLifecycle{}
}
lifecycle.PreUpgrade = nil
lifecycle.PostUpgrade = nil
if ops.Spec.Lifecycle != nil {
    lifecycle.PreUpgrade = ops.Spec.Lifecycle.PreUpgrade.DeepCopy()
    lifecycle.PostUpgrade = ops.Spec.Lifecycle.PostUpgrade.DeepCopy()
}
sandbox.Spec.Lifecycle = lifecycle
if lifecycle.PrePause == nil && lifecycle.PostResume == nil &&
    lifecycle.PreUpgrade == nil && lifecycle.PostUpgrade == nil {
    sandbox.Spec.Lifecycle = nil
}
```

Do not add automatic cleanup of upgrade fields after completion. Existing ops tracking label/finalizer cleanup continues through the existing flow, without modifying the entire Lifecycle.

### Pause Wait Deadline

sandboxcr Pause currently uses a fixed one-minute wait. PrePause retains the shared action's default single-attempt timeout of 60 seconds, with up to three attempts across separate reconciles while its local retry state is retained; interruptions may cause additional attempts. Including retry scheduling, queueing, checkpoint waiting, and Pod deletion can make pause take substantially longer than one minute; failure pass-through does not guarantee that callers receive success within the original fixed deadline.

Implementation must adjust Pause wait semantics rather than rely on a fixed one minute:

- Prefer the request context deadline.
- If there is no deadline, use a defensive maximum, for example 10 minutes.
- LifecycleAction controls the timeout of a single hook attempt; PrePause's local retry budget is independent of the API wait deadline.
- A wait timeout stops only the current API call's wait; it does not cancel the Pause desired state accepted by the controller.
- Retrying Pause attaches to the same state machine rather than repeatedly resetting the hook cycle.

This is consistent with the existing `resumeWaitMaxTimeout` pattern for Resume.

## Upgrade and Compatibility

- All new fields are optional; Sandboxes without hooks retain their behavior.
- Deploy the CRDs before the controller.
- Older controllers ignore the new fields and may therefore not execute hooks during a mixed-version rollout. Complete the controller rollout before making hooks available to users.
- New controllers do not create meaningless Conditions for old Sandboxes without hooks.
- LifecycleAction becomes the canonical Go type; UpgradeAction is marked Deprecated and retained as an alias. Existing Go consumers may keep using the old name. JSON fields and upgrade hook defaults remain unchanged, and new hook validation does not extend to old upgrade fields.
- Validate controller-gen and DeepCopy generation for the type alias. DeepCopy uses LifecycleAction as the receiver, and the old alias can still call the same methods; do not manually maintain duplicate generated methods.
- This proposal does not remove UpgradeAction. Its eventual removal is a follow-up goal requiring a defined deprecation period and migration arrangements for Go consumers.
- Adding Lifecycle to SandboxSet changes the revision hash; only pools with configured hooks roll as expected.
- Existing SandboxUpdateOps lifecycle JSON remains unchanged, but the controller's merge behavior needs correction.
- E2B adds no hook configuration fields. Pause success still means pause has completed, not that PrePause succeeded; failures are observable through Conditions/Events.
- During an active normal resume, a live claimed instance remains externally paused until Ready=True. This corrects observation without making actual termination, deletion, or unrelated NotReady states more permissive.

| Observation during PostResume processing | Value |
| --- | --- |
| Internal Sandbox phase | Running |
| SandboxState / E2B describe and list state | paused |
| Route state | paused, never Running |
| Ready Condition | False |
| ResumeTask / Connect / Resume | Wait for Ready; no premature success |
| Gateway wake | Wait for Resume and a Running route; no premature Continue |

The generic E2B `dead / RunningResourceClaimedButNotReady → running` compatibility override must not apply to this active resume state. `GetSandboxState` returns paused directly, so the existing override does not match; preserve the generic override for unrelated cases rather than duplicating backend Hook inspection in the API layer. Explicit tests cover describe/list, route projection, repeated waits, and gateway continuation.

## Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| Side effects from repeated or overlapping hook execution | Idempotency and atomic commits; unknown results may be replayed, with script-level locking when needed |
| Loss of unflushed state after PrePause retries fail | Explicit best-effort field contract; retain Failed in independent PrePauseHook, without promising application data has been committed |
| PrePause failure retains resources indefinitely | Allow pause to continue after bounded retries; an accepted pause still completes first |
| Hook failure pass-through is mistaken for checkpoint failure pass-through | Hibernate must still pass the checkpoint's own success checks |
| Traffic is admitted before PostResume completes | Ready/route gate; does not cover draining existing connections or direct access that bypasses the gateway |
| An instance with failed PostResume is treated as Dead and cannot be actively paused | Correct state mapping and Pause admission only for an incomplete resume hook in the current cycle |
| Hook client times out while the process keeps running | Validate actual envd server-side timeouts and use bounded cleanup for known PIDs; merely closing the stream is insufficient |
| Long-running hooks in bulk occupy all controller workers | One attempt per reconcile, in-memory backoff with requeue scheduling, and a shared non-blocking execution cap; no retry sleeps or blocked semaphore acquisition |
| The first traffic-driven wake request times out | Reserve the full wake timeout budget for Pod/init/hook/queueing; examples use shorter hook timeouts |
| Hook output leaks Secrets or consumes unbounded memory | Do not log raw output; limit captured output; write only safe error summaries to Conditions/Events |
| Root hooks have excessive privileges | Pause/resume hooks default to the container's own (envd default) user instead of root; explicit `user` is pattern-validated; upgrade hooks keep root only for workspace backup/restore; only authorized CR writers may configure them |
| UpdateOps overwrites Pause/Resume hooks | Field-level merge; the webhook prohibits configuring new hooks through UpdateOps |
| Recycle/clone leaks previous-tenant or temporary upgrade hooks | Recycle restores pool-level Lifecycle; checkpoint/clone inherit only prePause/postResume |
| Existing Checkpoint runs concurrently with a hook | Check before the hook and before the destructive action; do not claim strict mutual exclusion |
| Controller restart loses counts or an attempt's result | Allow counting/backoff to restart for incomplete hooks; messages are diagnostic, not durable totals; require idempotent scripts |
| Concurrent Pod/configuration changes make execution results ambiguous | Use current observed Pod/configuration, reset Conditions on new lifecycle transitions, and do not promise durable execution identity or latest-configuration validation |
| E2B compatibility override misreports active resume as running | Return paused from shared state evaluation and test describe/list, routes, repeated waits, and Continue together |
| Upgrade internal Resume mistakenly runs the hook | Skip through the separate upgrade path; clear the marker on completion and retain the gate when an abandoned upgrade returns to normal Running |
| No executable Pod/agent-runtime | Skip with a Warning when the Pod is missing; for unreachable runtime, PrePause allows pause after bounded failure, while PostResume blocks Ready |

## Alternatives

### 1. Use Only Kubernetes preStop/postStart

Rejected. preStop/postStart cannot:

- Explicitly distinguish initial startup from Resume.
- Guarantee execution before checkpointing.
- Participate in Sandbox Conditions.
- Run after runtime/CSI initialization.
- Unify manual, automatic, and traffic-driven wake semantics.

### 2. Execute Hooks in API/Manager Pause/Resume Requests

Rejected. This would cause:

- Direct spec patches to bypass hooks.
- Auto-pause and wake-on-traffic to use different logic.
- Client disconnection to potentially cancel accepted desired state.
- Manager and controller to maintain two state machines.

### 3. Execute PostResume in the Gateway

Rejected. The gateway should not own Sandbox lifecycle business logic and cannot cover scheduled/manual Resume.

### 4. Separate LifecycleHook CRD

A separate CRD could hold execution history, but increases object, permission, and GC complexity, and cannot guarantee exactly-once merely by being a CRD. This proposal uses inline exec declarations and Hook Conditions, with retry bookkeeping in memory; persisted execution status or external task records remain follow-up goals.

### 5. Block Pause Indefinitely After PrePause Failure

Not adopted. This would require additional design for pause cancellation, automatic pause thresholds after cancellation, and a resource reclamation safety net. It would also change the existing state machine constraint that a started pause must complete first. The chosen policy continues Pause after bounded retries fail; PostResume still blocks Ready, and the two failure policies must not be mixed.

## Test Plan

### API/Schema

- DeepCopy for the new fields and LifecycleAction; the UpgradeAction alias can still call the same DeepCopy methods.
- Compatibility of construction, assignment, and hook parameter passing using old and new Go type names.
- controller-gen handles the alias correctly; the Go type migration does not change the CRD exec/timeoutSeconds structure or defaults.
- Sandbox/SandboxSet/SandboxTemplate schema/CEL rejects missing exec or empty command, and rejects a non-empty `user` that violates the username pattern.
- Timeout defaults to 60 seconds, with 1–60-second boundaries for new hooks; existing upgrade hooks are unaffected by the new upper bound.
- SandboxSet/SandboxTemplate field mirroring and CEL rejection of upgrade hooks; Sandbox allows all four hook fields.
- SandboxUpdateOps rejects prePause/postResume and lifecycle remains immutable.
- No new execution-record fields in SandboxStatus; Hook Conditions preserve ObservedGeneration correctly and messages are not parsed as retry counters.

### PrePause

- Stop: hook success → persisted terminal status → Pod delete.
- Success after transient failure, all 3 attempts failing, and process cleanup after a single-attempt timeout.
- One attempt per reconcile, 1s/2s local retry delays, early events honor retained backoff, and pause continues after the third failed attempt during uninterrupted operation.
- Hibernate helper: hook success/failure pass-through → checkpoint success → Pod delete; checkpoint failure still blocks deletion.
- No hook execution before an existing checkpoint completes; rechecking a new checkpoint after terminal status does not rerun the hook.
- Success, failure pass-through, and skipping must all be persisted first; a failed status write must not delete the Pod.
- Restart may lose in-memory counts and retry an incomplete Running/Pending hook; terminal persisted Conditions remain terminal, and diagnostic messages are not used to recover counts.
- spec.paused=false does not cancel pause midway; pause completes before normal resume and PostResume execution.
- Automatic and manual Pause use the same path; a completed Pause does not execute the hook again.
- PrePauseHook=False/Skipped and a Warning when the Pod is missing/non-executable, followed by the existing pause convergence flow.
- Independent Failed/Skipped remains observable after SandboxPaused=True; checkpoint reason/message changes do not erase it.
- Messages show locally observed attempt numbers and safe failure summaries; changed message text does not affect retry decisions.
- Failed retries use updated configuration without resetting retained counts; persisted terminal results are not reopened by later configuration changes.
- A new Pause resets retry bookkeeping, while repeated API requests and unrelated timeout updates do not reset it.

### PostResume

- No execution before Pod Ready.
- No execution before runtime init / Token / CSI completion.
- Ready=False while the hook is pending/failed.
- Ready=True after success.
- Wake-on-traffic waits for hook completion.
- Consistent scheduled/manual/connect Resume behavior.
- No execution on initial Pod creation.
- Upgrade internal Resume skips the hook and upgrade success clears the marker; abandoning upgrade for normal Running executes the hook.
- Hook failure retains the Pod, with one attempt per reconcile and error-triggered backoff.
- During the active resume until Ready=True, SandboxState, E2B describe/list, and routes report paused; the generic E2B dead-to-running override does not apply.
- Repeated Connect/Resume/Wake reuse Hook Conditions and retained retry state, and neither success nor Continue occurs before Ready.
- Active Pause remains allowed without relaxing actual termination, deletion, or unrelated abnormal states.
- Hook success terminal status and Ready publication occur in different reconciles; another Pause during execution does not publish Ready prematurely.
- Use the current Pod IP/UID, without accessing the pre-pause runtime.
- New Resume entry resets old Hook state and creates Pending only for a configured hook; initial creation does not execute PostResume.
- Completion publishes Ready=True/reason=PodReady while retaining the result Condition; unrelated later NotReady does not reactivate a historical Hook gate.
- Each failed retry uses current observed configuration; later edits do not reopen persisted success, and mid-operation additions have no execution guarantee until the next resume.
- Removing a hook releases the gate after bounded command termination; restart may reset counts/backoff without changing the requirement that PostResume succeed before Ready.
- Failure messages include a diagnostic attempt number and safe summary, never authoritative lifetime counts.

### SandboxSet/Template/Recycle

- Inline/templateRef propagation.
- Revision hash.
- Rolling update.
- Deep copying does not mutate the cache.
- Recycle restores pool-level hooks and clears execution Conditions, without leaking previous-tenant configuration.
- Both checkpoint templates and clones retain only prePause/postResume, without inheriting temporary upgrade hooks.
- SandboxSet is the policy source for templateRef; changing template Lifecycle does not change the pool revision.
- UpdateOps preserves prePause/postResume whether Sandbox/ops.lifecycle is nil or non-nil.
- UpdateOps completion does not introduce automatic cleanup of upgrade hooks.

### Concurrency

- Concurrent Pause calls share one hook state machine cycle.
- Concurrent Resume calls share one hook state machine cycle.
- Pause racing with Resume.
- Hooks racing with deletion.
- Hooks racing with Checkpoint.
- Status patch conflicts.
- The controller continues converging after the API context times out.
- The old command has terminated before an ordinary timeout retry; replay of unknown results validates the limits of idempotency/script-level mutual exclusion.
- Non-blocking execution capacity applies jointly to PrePause/PostResume; no slot means no command, no attempt consumption, and a delayed requeue.
- Success/error/cancellation releases the execution slot; controller shutdown stops new commands and uses bounded cleanup rather than sleeping retries.
- Final integration validates worker capacity for unrelated reconciliation, queueing/fairness with bulk hooks, and eventual instance recovery when a wake request times out first.

### Security

- Raw stdout/stderr are not written to Conditions/Events or leaked through shared process result logging.
- Safe error summaries are truncated and captured output has a size limit.
- A TLS Sandbox with no bundle never downgrades; each hook follows its own failure policy.
- Runtime URL/Pod IP not being ready does not silently succeed.
- Secrets, command contents, users, and complete output are not logged.
- Validate server-side timeout, process-tree termination, unknown PID/results, and cleanup context boundaries in the actually deployed envd version with the existing gRPC transport.

## Implementation Plan

Suggested breakdown:

1. Add API/CRD hook fields and validation, migrate the canonical type to LifecycleAction, and retain and validate generation and Go compatibility of the deprecated UpgradeAction alias.
2. Validate executor server-side timeout/process cleanup and implement output boundaries.
3. Implement independent Hook Conditions with diagnostic attempt messages, in-memory retry bookkeeping, non-blocking execution limits, and per-reconcile PrePause retries with failure pass-through and Pause wait deadlines.
4. Implement the PostResume Ready gate, initializer/Upgrade paths, normal resume state mapping, and active Pause.
5. Implement SandboxSet/SandboxTemplate/checkpoint/clone propagation and recycle reset.
6. Implement SandboxUpdateOps field-level merge and validation rejecting new hooks.
7. Perform final integration acceptance: manual Pause/Resume, auto-pause, traffic-driven wake-up, and bulk queueing.

These steps define code review and validation boundaries; they do not imply that hooks may be exposed to users before the prerequisite propagation and overwrite fixes are complete. Verification of actual envd timeout and process termination capabilities is required before user availability.

Each code PR runs only tests for affected packages, without E2E tests during ordinary development. Real-environment acceptance takes place in the final integration step.

## Proposal History

- 2026-10-21: Created the Chinese draft, defining the PrePause/PostResume API, execution order, Conditions, failure semantics, propagation, and interactions with existing features.
- 2026-10-21: Following review, adopted PrePause failure pass-through after bounded retries and PostResume failure blocking Ready. Corrected pause non-cancellation, schema validation, template policy sources, and UpdateOps overwrite descriptions; completed process timeout/output and wait deadline boundaries; listed capabilities outside this proposal as follow-up goals.
- 2026-10-21: Adopted LifecycleAction as the canonical action type, marked UpgradeAction Deprecated and retained it as an alias, and clarified generation validation and Go compatibility requirements.
- 2026-10-21: Following review, separated PrePauseHook from pause progress, moved retries across reconciles with non-blocking concurrency limits, and clarified resource field permissions and the paused external projection. Retry counters and backoff remain in memory, with diagnostic attempt numbers in Condition messages; strict cross-restart guarantees and additional execution-status fields are outside this proposal. This proposal does not implement the hooks.
- 2026-10-22: Following review, added the optional `user` field (empty defaults to the envd/container user; upgrade hooks keep root), documented that memory-persistent pause/resume bypasses preStop/postStart entirely, and listed startup-overlapping runtime initialization as a follow-up goal. This proposal does not implement the hooks.
