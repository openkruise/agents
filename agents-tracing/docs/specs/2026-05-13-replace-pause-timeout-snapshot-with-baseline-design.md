# Replace Pause Timeout Snapshot Annotation with Baseline-Aware Policy

## Context

`AnnotationPauseTimeoutSnapshot` (`api/v1alpha1/annotations.go:31`) stores the timeout observed at pause time as a JSON-encoded `infra.TimeoutOptions`. Its sole consumer is the `SnapshotAware` branch of `Sandbox.SaveTimeoutWithPolicy` (`pkg/sandbox-manager/infra/sandboxcr/sandbox.go:265`), which uses the snapshot to detect whether another concurrent paused→running connect request has already overwritten the timeout in the current resume cycle. When the snapshot still matches the current spec timeout, the policy treats the update as safe and falls through to `Always` semantics; when it diverges, the policy degrades to `ExtendOnly` so the later request cannot shrink the timeout written by an earlier one.

To make this snapshot reliably available across multiple sandbox-manager replicas without sticky routing, the system maintains it from three places:

- `Sandbox.Pause` writes the snapshot when the caller passes `PauseOptions.CaptureTimeoutSnapshot` (the e2b `PauseSandbox` handler always sets this).
- `Sandbox.Resume` repairs a missing snapshot when the caller passes `ResumeOptions.EnsureTimeoutSnapshotIfMissing` (e2b `ResumeSandbox` and `ConnectSandbox` always set this).
- `SandboxReconciler.ensurePauseTimeoutSnapshot` (`pkg/controller/sandbox/sandbox_controller.go:427`) keeps the snapshot in sync from the controller side, mainly so the controller-driven auto-pause path does not leave a paused sandbox without a snapshot.

The triple-fallback maintenance creates avoidable coupling. The annotation is internal coordination state and shows on the API surface; auto-pause requires controller-side fallback only because the snapshot is stored on the CR; concurrent connect handlers need this elaborate handshake purely because the baseline value lives outside their own request.

## Goals

- Eliminate `AnnotationPauseTimeoutSnapshot` and all helper functions that read or write it.
- Remove all three snapshot-maintenance fallbacks (`Sandbox.Pause` capture, `Sandbox.Resume` repair, controller `ensurePauseTimeoutSnapshot`).
- Preserve the existing concurrency semantics for paused→running connect: the first writer's timeout sticks; later concurrent writers cannot shrink it but may still extend it.
- Preserve cross-replica correctness: any sandbox-manager replica handling any paused→running request must arrive at the same outcome.
- Keep `Sandbox.SaveTimeoutWithPolicy` as the single timeout-write entry point.
- Behave at least as well as the current design under controller-driven auto-pause.

## Non-goals

- Do not change `Sandbox.SaveTimeoutWithPolicy`'s method shape or move timeout writes to a new entry point.
- Do not change the `Always` and `ExtendOnly` policy semantics.
- Do not change e2b request semantics observable to clients (timeout values, status codes, fall-back rules between Connect/Resume/SetTimeout).
- Do not introduce shared infrastructure (Redis, peer messaging, sticky routing).
- Do not change how auto-pause flips `spec.paused` in the controller.

## Design

### Move the baseline from the CR to the request

The snapshot answers exactly one question: "what was the timeout when this resume cycle began, before any concurrent connect could have overwritten it?" Each connect handler already reads the sandbox at the start of its request, so it already holds the answer in memory. Instead of persisting the value as an annotation on the CR, the handler attaches it to the timeout-update request it issues a few steps later.

This makes the baseline a per-request value, not a per-sandbox value. Cross-replica correctness comes from the fact that every replica reads the same authoritative spec from the API server when its request enters; there is no shared in-memory state to keep coherent.

### `TimeoutOptions` carries the baseline

`infra.TimeoutOptions` gains an optional `Baseline` field:

```go
type TimeoutOptions struct {
    ShutdownTime time.Time
    PauseTime    time.Time
    // Baseline is the timeout state the caller observed before issuing this update.
    // Only consulted when the policy passed to SaveTimeoutWithPolicy is BaselineAware.
    // For all other policies and for any read-side use of TimeoutOptions, this field is nil.
    Baseline *TimeoutOptions
}
```

`Baseline` is a self-referential pointer to keep the type cycle-free at the value level and to make "no baseline" trivially representable as `nil`. All existing consumers of `TimeoutOptions` (`setTimeout`, `Equal`, `ShouldExtendTimeout`, `timeoutEndAt`, `GetTimeoutFromSandbox`) read only `ShutdownTime` and `PauseTime`, so `Baseline` is invisible to read-side use.

### `BaselineAware` replaces `SnapshotAware`

`infra.TimeoutUpdatePolicy` remains a `string` enum. The `SnapshotAware` constant is removed and replaced with `BaselineAware`:

```go
const (
    TimeoutUpdatePolicyAlways        TimeoutUpdatePolicy = "Always"
    TimeoutUpdatePolicyExtendOnly    TimeoutUpdatePolicy = "ExtendOnly"
    TimeoutUpdatePolicyBaselineAware TimeoutUpdatePolicy = "BaselineAware"
)
```

Inside `Sandbox.SaveTimeoutWithPolicy`, the `SnapshotAware` branch is replaced with the following equivalent:

```go
case infra.TimeoutUpdatePolicyBaselineAware:
    if opts.Baseline == nil {
        return fmt.Errorf("BaselineAware policy requires opts.Baseline to be set")
    }
    if timeout.Equal(current, *opts.Baseline) {
        // No concurrent writer has changed the timeout since the caller's observation.
        // Treat as Always: overwrite if different.
        shouldUpdate = !timeout.Equal(current, opts)
    } else {
        // Some other request has already written a new timeout in this cycle.
        // Degrade to ExtendOnly so we never shrink an already-extended timeout.
        shouldUpdate = timeout.ShouldExtendTimeout(current, opts)
    }
```

A `nil` baseline is a programmer error and is reported, not silently treated as `Always`.

### Pause and Resume options are simplified

`PauseOptions.CaptureTimeoutSnapshot` and `ResumeOptions.EnsureTimeoutSnapshotIfMissing` are removed. The corresponding code paths in `Sandbox.Pause` and `Sandbox.Resume` are removed.

`ResumeOptions` is kept as an empty struct so future extensions can add fields without changing call sites.

### Controller no longer maintains the snapshot

`SandboxReconciler.ensurePauseTimeoutSnapshot` is removed. `EnsureSandboxPaused` no longer calls any snapshot-related helper.

### Connect handlers attach the baseline

In `pkg/servers/e2b/pause_resume.go`, the connect-time timeout update changes from passing `SnapshotAware` to passing `BaselineAware` together with a `Baseline` value derived from the sandbox state read at handler entry.

`getSandboxOfUser` returns a sandbox whose `GetTimeout()` reflects the current spec at entry. The handler captures this as the baseline once, before calling `ResumeSandbox`, and threads it into the `TimeoutOptions` it later passes to `SaveTimeoutWithPolicy`:

```go
// updateConnectTimeout (preConnectState == Paused branch)
observed := sbx.GetTimeout() // captured at handler entry
opts := sc.buildSetTimeoutOptions(autoPause, now, timeoutSeconds)
opts.Baseline = &observed
policy := infra.TimeoutUpdatePolicyBaselineAware
result, err := sbx.SaveTimeoutWithPolicy(ctx, opts, policy)
```

The non-paused branch of `updateConnectTimeout` continues to use `ExtendOnly` and does not set a baseline.

The legacy `ResumeSandbox` handler (kept for old SDK compatibility) follows the same pattern: capture baseline before calling `ResumeSandbox`, attach it to the `BaselineAware` update.

`PauseSandbox` no longer sets `CaptureTimeoutSnapshot` (the field is gone) and the snapshot is never written.

## Pre-resume baseline invariant

The BaselineAware policy's "current == baseline ⇒ overwrite" shortcut depends on the baseline being the timeout value as it stood **before any resumer in this cycle could have written a new one**. This subsection establishes why that property holds for every baseline that ever reaches the BaselineAware branch.

### Statement

When a connect or resume handler observes `state == Paused` at request entry, the timeout fields read in the same `Get` are necessarily a pre-resume value. No prior resumer in the current paused→running cycle has yet executed `SaveTimeoutWithPolicy`.

### Why state, not spec.paused

`GetSandboxState` (`pkg/utils/sandboxutils/utils.go:47`) returns `StatePaused` in three sub-cases:

- (a) `status.Phase == Running && spec.Paused == true` — paused but the controller has not yet moved the Phase off Running.
- (b) `status.Phase == Paused` regardless of `spec.Paused` — possibly during the transient window where some resumer X has already flipped `spec.Paused = false` but the controller has not yet advanced Phase.
- (c) `status.Phase == Resuming` regardless of `spec.Paused` — controller is actively reconciling the resume.

Sub-cases (b) and (c) include states where `spec.Paused` is already `false`. Gating only on `spec.Paused == true` would miss these transient states and silently degrade them to ExtendOnly. Gating on `state == Paused` covers them — provided we can show the timeout reading is still pre-resume in those sub-cases.

### Proof chain

The invariant rests on three concrete facts about the resume code path:

1. **`Sandbox.Resume` does not modify timeout fields.** Its `retryUpdate` modifier only flips `spec.Paused = false` (`pkg/sandbox-manager/infra/sandboxcr/sandbox.go`, after this change removes the `EnsureTimeoutSnapshotIfMissing` branch).
2. **`Sandbox.Resume` blocks until `status.Phase == Running`.** It waits on `resumeTask.Wait` (`sandbox.go:436`), which only fires when the controller advances Phase to Running.
3. **`SaveTimeoutWithPolicy` is only called after `Resume()` returns.** Both `ConnectSandbox` and the legacy `ResumeSandbox` handlers in `pkg/servers/e2b/pause_resume.go` enforce this ordering.

Combining: any `SaveTimeoutWithPolicy` call that could change a sandbox's timeout in a resume cycle happens strictly after that cycle's Phase has reached Running. Therefore an observation of `Phase ∈ {Paused, Resuming}` (sub-cases b and c) is sufficient evidence that no resumer has yet written a new timeout. Sub-case (a) trivially predates any resume.

### Cache staleness does not break correctness

Informer cache can only lag behind the API server; it can never be ahead. A stale cache may return an older timeout value as the baseline, causing `Equal(current, baseline)` to evaluate as false even when no concurrent writer has acted in this resume cycle. In that case the policy degrades from `Always` to `ExtendOnly`, meaning the handler cannot shrink the timeout but may still extend it.

This degradation is safe and expected: the concurrency contract for paused→running resume is "the longest timeout wins among concurrent resumers." A spurious ExtendOnly caused by cache lag produces the same observable outcome as a genuine concurrent write — the longest timeout survives. There is no scenario where cache lag causes an incorrect overwrite or data loss.

The retry loop inside `SaveTimeoutWithPolicy` re-reads `current` from the API reader on conflict, so the comparison `Equal(current, baseline)` always sees server-authoritative state at decision time. A conflict triggers a re-fetch and re-evaluation, ensuring the final Update reflects the true current state.

### Consequences for callers

The handler-side gate `state == Paused → BaselineAware; otherwise ExtendOnly` is sufficient and reliable. No additional check on `spec.Paused` is needed. The choice to use `state` rather than `spec.Paused` also matches existing code (`pause_resume.go:162`) and yields more precise behavior in transient resume windows: requests that legitimately belong to the paused→running cycle continue to flow through BaselineAware instead of falling back to ExtendOnly when their `Get` happens to land on a Paused/Resuming Phase with `spec.Paused` already flipped.

## Behavior under auto-pause

Auto-pause flips `spec.paused = true` in the controller without modifying `spec.shutdownTime` or `spec.pauseTime`. Throughout an auto-pause-induced paused window, no replica writes timeout fields. When a connect handler later resumes the sandbox:

- It reads the sandbox at handler entry. The baseline equals the user's last set timeout values.
- It calls `Resume`, which flips `paused = false`.
- It calls `SaveTimeoutWithPolicy(BaselineAware{baseline})`. Because no one has written timeout fields since the baseline was captured, `current == baseline`, the policy degrades to `Always`, and the new timeout is written.

Under concurrent connect to an auto-paused sandbox:

- Both handlers capture identical baselines.
- One wins the `Resume` race; the other becomes a loser whose modifier is a no-op.
- The first `SaveTimeoutWithPolicy` call sees `current == baseline` and writes its requested timeout.
- The second `SaveTimeoutWithPolicy` call sees `current != baseline` and degrades to `ExtendOnly`, never shrinking the timeout the first writer set.

This matches the current SnapshotAware behavior. It is in fact more robust: there is no time window in which the snapshot has not yet been written by the controller fallback, so the second writer can never accidentally see "no snapshot, use Always" the way it can today before `ensurePauseTimeoutSnapshot` runs.

## Cross-replica correctness

The baseline travels with the request, not the object. Any replica handling a connect captures its own baseline at request entry from the API server's authoritative state. There is no shared baseline that needs to be kept consistent between replicas. The optimistic concurrency on `SaveTimeoutWithPolicy`'s underlying `Update` is unchanged (k8s resource-version CAS). The `Equal(current, baseline)` check inside the retry loop sees server-side state at the moment of comparison, so a baseline-mismatch that arises only on retry is correctly observed as such.

## Code Impact

Removed:

- `api/v1alpha1/annotations.go` — `AnnotationPauseTimeoutSnapshot` constant.
- `pkg/utils/timeout/timeout.go` — `SetTimeoutSnapshot`, `GetTimeoutSnapshot`, `ClearPauseTimeoutSnapshot`, `IsTimeoutMatchedSnapshot`, the package-level `jsonMarshalTimeoutOptions` indirection.
- `pkg/sandbox-manager/infra/interface.go` — `TimeoutUpdatePolicySnapshotAware`, `PauseOptions.CaptureTimeoutSnapshot`, `ResumeOptions.EnsureTimeoutSnapshotIfMissing`.
- `pkg/sandbox-manager/infra/sandboxcr/sandbox.go` — `Pause` capture branch, `Resume` ensure-if-missing branch, `SnapshotAware` switch case.
- `pkg/controller/sandbox/sandbox_controller.go` — `ensurePauseTimeoutSnapshot`, the call from `EnsureSandboxPaused`.

Added or changed:

- `pkg/sandbox-manager/infra/interface.go` — `TimeoutOptions.Baseline *TimeoutOptions`, `TimeoutUpdatePolicyBaselineAware` constant. `ResumeOptions` remains as an empty struct.
- `pkg/sandbox-manager/infra/sandboxcr/sandbox.go` — `BaselineAware` switch case in `SaveTimeoutWithPolicy`.
- `pkg/servers/e2b/pause_resume.go` — `ConnectSandbox`/`ResumeSandbox` capture baseline at entry and attach it to the `TimeoutOptions` sent to `SaveTimeoutWithPolicy`.
- Tests across `pkg/utils/timeout`, `pkg/sandbox-manager/infra/sandboxcr`, `pkg/controller/sandbox`, `pkg/servers/e2b` are rewritten to use the new policy and remove all references to the snapshot annotation.

## Test Plan

Unit tests on `Sandbox.SaveTimeoutWithPolicy`:

- `BaselineAware` with `current == baseline` and `requested != current` writes the new value.
- `BaselineAware` with `current == baseline` and `requested == current` is a no-op.
- `BaselineAware` with `current != baseline` and `requested` extending the end time writes the new value.
- `BaselineAware` with `current != baseline` and `requested` not extending is a no-op.
- `BaselineAware` with `Baseline == nil` returns an error.
- `Always` and `ExtendOnly` paths remain unchanged.

End-to-end tests on the connect path:

- After auto-pause, a single Connect resumes and writes the requested timeout.
- After auto-pause, two concurrent Connects resume the sandbox once; the surviving timeout is the first writer's; the loser does not shrink it; both calls return success.
- After manual pause, the same two scenarios behave the same way.
- Stale snapshot annotation on an existing paused sandbox does not affect the new code path.

Removal tests: the old `TestEnsurePauseTimeoutSnapshot`, snapshot-related tests in `pkg/utils/timeout`, and the snapshot assertions in `pkg/sandbox-manager/infra/sandboxcr/pause_resume_test.go` and `pkg/servers/e2b/timeout_test.go` are deleted alongside the code they covered.
