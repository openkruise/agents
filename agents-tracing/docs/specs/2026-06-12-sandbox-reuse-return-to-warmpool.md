# Sandbox Reuse & Return-to-Pool — Design Spec

- Date: 2026-06-12
- Branch: `sandbox-reuse-docs`
- Scope: `api/v1alpha1/`, `pkg/controller/sandbox/`, `pkg/controller/sandboxset/`, `pkg/sandbox-manager/`

## 1. Background

Currently, after a SandboxClaim completes and the user finishes using the sandbox, the sandbox is destroyed (via `shutdownTime` or manual deletion). This is wasteful: the sandbox Pod has already gone through creation, scheduling, initialization, and warm-up. Destroying and recreating it introduces unnecessary overhead:

- Pod scheduling latency
- Container image pull time
- Runtime initialization cost (agent-runtime, CSI mounts, sidecar injection)
- IP address allocation overhead

In many scenarios (e.g., short-lived AI agent tasks, code execution environments), sandbox environments are inherently stateless — user data can be cleaned up, and the sandbox can be returned to the pool for the next user.

## 2. Goals & Non-Goals

### Goals

- Allow claimed sandboxes to be **reused** (user state cleaned up) and **returned** to the SandboxSet pool.
- Use **annotations** as the sole trigger mechanism — no new CRD spec fields, no ReclaimPolicy.
- Add a `Reusing` phase to the Sandbox lifecycle, indicating cleanup is in progress.
- During reuse, SandboxSet is **completely unaware** of the sandbox (OwnerReference is not restored). OwnerReference is only restored after reuse succeeds and the sandbox is back to Running+Ready.
- Ensure the reused sandbox appears identical to a newly created Available sandbox from SandboxSet's perspective.

### Non-Goals

- Defining specific cleanup actions (filesystem wipe, process termination, etc.) — these are the responsibility of `SandboxReuser` interface implementations. This design only orchestrates the lifecycle and defines the interface contract.
- Supporting sandbox reuse with persistent volumes (PVC) — the initial version targets stateless sandboxes only.
- Automatic health checks after reuse — reuse the existing readiness probe mechanism.

## 3. Current Lifecycle

```
SandboxSet creates Sandbox
    → Sandbox Phase: Pending → Running (Ready condition = True)
    → SandboxSet classifies it as: Available

SandboxClaim / E2B API claims Sandbox
    → Record keys of labels/annotations added/modified during claim to updated-metadata-in-claim annotation
    → Remove OwnerReference (SandboxSet no longer owns the sandbox)
    → Label sandbox-claimed = "true"
    → Label claim-name = "<claim-name>"
    → Annotation owner = "<user>"
    → SandboxSet: sandbox disappears from list → creates replacement

User finishes → Sandbox is deleted
    → Pod is deleted, resources released
```

Key observation: the claim flow **removes the OwnerReference**, causing SandboxSet to consider the sandbox as "no longer mine" and trigger a replacement. The reuse flow needs to reverse this operation.

## 4. New Lifecycle

```
User finishes → sets annotation agents.kruise.io/reuse=true (trigger) on sandbox
    → Prerequisite: sandbox already has agents.kruise.io/reuse-enabled=true (capability marker)
    → Sandbox controller detects both annotations present
    → Non-Running phase:
        → Set Reusing condition to ReuseRejected + Warning Event
        → No changes (OwnerRef, labels, annotations, phase all remain as-is)
        → Upper layer decides next action (see 5.2, 5.3)
    → Running phase:
        → Clear shutdownTime / pauseTime (prevent termination during reuse)
        → Sandbox Phase: Running → Reusing
        → Do NOT restore OwnerReference → SandboxSet is completely unaware
        → Execute cleanup via SandboxReuser interface:
            → Clean user data + container data (processes, filesystem, volumes, etc.)
            → Reset claim-modified metadata: for each key in updated-metadata-in-claim,
              restore to SandboxSet template value or remove if not in template
            → Remove agents.kruise.io/reuse annotation (keep reuse-enabled)
        → Cleanup success:
            → Cooldown period (--reuse-grace-period, default 10s, phase remains Reusing)
              Purpose: allow TrafficPolicy / SecurityProfile controllers to observe label changes
              and clean up routing rules / security policies before the sandbox returns to pool
            → After cooldown:
                → Sandbox Phase: Reusing → Running (Ready)
                → Restore OwnerReference to original SandboxSet
                → SandboxSet becomes aware → counts it directly as Available
                → If unclaimed total > replicas → SandboxSet scales down excess
        → Failure / Timeout:
            → Set Reusing condition (ReuseFailed / ReuseTimeout) + Warning Event
            → Sandbox controller directly deletes the sandbox → SandboxSet replenishes as needed
```

## 5. Trigger Mechanism

### 5.1 Annotation-Based Trigger

The reuse flow uses two annotations working together:

```go
const (
    // AnnotationReuseEnabled marks a sandbox as supporting reuse.
    // When set to "true", the sandbox has the reuse capability.
    // Preserved after successful reuse so the sandbox can be reused again after the next claim.
    AnnotationReuseEnabled = InternalPrefix + "reuse-enabled"

    // AnnotationReuse triggers the sandbox reuse flow.
    // When set to "true", the sandbox controller will clean up the sandbox and return it to the original SandboxSet pool.
    // Only effective when AnnotationReuseEnabled is also "true".
    // Removed by the controller after successful reuse.
    AnnotationReuse = InternalPrefix + "reuse"
)
```

- `reuse-enabled`: **Capability marker** — indicates the sandbox supports reuse. Set by upper layers during creation or claim. Preserved after successful reuse, enabling the sandbox to be reused again in the next claim-reuse cycle.
- `reuse`: **Trigger** — means "start reuse now". Set by upper layers when the user is done. Removed by the controller after successful return to pool.

Both annotations must be present with value `"true"` to trigger the reuse flow. A sandbox with only `reuse=true` but without `reuse-enabled=true` will not enter reuse.

No new spec fields. No ReclaimPolicy.

### 5.2 SandboxClaim Scenario

Users managing sandboxes via SandboxClaim must ensure the sandbox has the `reuse-enabled` capability marker, then set the trigger annotation:

```bash
kubectl annotate sandbox <name> agents.kruise.io/reuse=true
```

`reuse-enabled` is typically set at an earlier stage (e.g., via SandboxSet template or by an upper-layer controller during claim).

If the sandbox's current phase is not `Running`, the sandbox controller sets a `ReuseRejected` condition. The user's upper-layer controller is responsible for watching this condition and deciding on a fallback strategy (e.g., deleting the sandbox directly).

### 5.3 E2B API Scenario

`SandboxManager.DeleteSandbox()` determines behavior based on the `AnnotationReuseEnabled` annotation — no additional environment variable needed. It checks whether the sandbox has `AnnotationReuseEnabled=true` (capability marker), has a SandboxSet origin label (`LabelSandboxPool`), and is currently in `Running` phase. When all three conditions are met, it sets the `AnnotationReuse` annotation instead of actually deleting, handing off to the sandbox controller. If conditions are not met (e.g., no `reuse-enabled`, phase is not `Running`), it falls through to the delete path.

```go
func (m *SandboxManager) DeleteSandbox(ctx context.Context, sbx infra.Sandbox) error {
    if sbx.IsReuseEnabled() && hasSandboxPool(sbx) && sbx.Phase() == SandboxRunning {
        return sbx.TriggerReuse(ctx)  // Set AnnotationReuse, don't actually delete
    }
    // Default: delete (including cases without reuse-enabled or non-Running phase)
    return m.doDeleteSandbox(ctx, sbx)
}
```

## 6. API Changes

### 6.1 Sandbox: Reusing Phase

New `SandboxPhase`:

```go
const (
    // SandboxReusing indicates the sandbox is being cleaned up, preparing to return to pool.
    SandboxReusing SandboxPhase = "Reusing"
)
```

New condition type:

```go
const (
    // SandboxConditionReusing tracks reuse progress.
    SandboxConditionReusing SandboxConditionType = "Reusing"
)

const (
    SandboxReusingReasonRejected  = "ReuseRejected"
    SandboxReusingReasonStarted   = "ReuseStarted"
    SandboxReusingReasonSucceeded = "ReuseSucceeded"
    SandboxReusingReasonFailed    = "ReuseFailed"
    SandboxReusingReasonTimeout   = "ReuseTimeout"
)
```

### 6.2 Sandbox Status: ReuseCount

```go
type SandboxStatus struct {
    // ...existing fields...

    // ReuseCount records the number of times this sandbox has been reused.
    // +optional
    ReuseCount int32 `json:"reuseCount,omitempty"`
}
```

### 6.3 Labels & Annotations

Reuse the existing `LabelSandboxPool` (`agents.kruise.io/sandbox-pool`) to identify the sandbox's origin SandboxSet. This label is set when SandboxSet creates the sandbox, its value is always the SandboxSet name, and it is preserved during claim — no new label needed.

> **Note**: `LabelSandboxPool` was previously marked as deprecated, but the reuse scenario gives it a clear purpose — identifying the origin SandboxSet. Remove its deprecated marker.

```go
const (
    // AnnotationReuseEnabled marks a sandbox as supporting reuse.
    AnnotationReuseEnabled = InternalPrefix + "reuse-enabled"

    // AnnotationReuse triggers the sandbox reuse flow. Removed by the controller after successful reuse.
    AnnotationReuse = InternalPrefix + "reuse"

    // AnnotationUpdatedMetadataInClaim stores the keys of labels/annotations added or modified
    // during the claim flow (JSON format, keys only — no values).
    // The reuse flow uses these keys to reset metadata back to SandboxSet template values.
    // Removed by the controller after successful reuse.
    AnnotationUpdatedMetadataInClaim = InternalPrefix + "updated-metadata-in-claim"
)
```

The value of `AnnotationUpdatedMetadataInClaim` is a JSON object containing only the **keys** of labels and annotations that were added or modified during claim:

```json
{
  "labels": ["sandbox-claimed", "claim-name"],
  "annotations": ["owner"]
}
```

### 6.4 Sandbox State Constants

The entire reuse lifecycle (cleanup, cooldown) is handled internally by the sandbox controller. When the sandbox restores its OwnerReference and returns to pool, it is already in Running+Ready state, and SandboxSet treats it directly as Available. Therefore, **no new SandboxSet-level state constants are needed**.

## 7. SandboxSet Controller Changes

The entire reuse lifecycle (cleanup, cooldown) is handled internally by the sandbox controller. When the sandbox restores its OwnerReference, it is already Running+Ready. Therefore, **SandboxSet requires no new groups or states** — a reused sandbox is indistinguishable from a newly created Available sandbox.

### 7.1 GroupedSandboxes

No changes needed. The existing `GroupedSandboxes` structure remains unchanged:

```go
type GroupedSandboxes struct {
    Creating      []*agentsv1alpha1.Sandbox
    Available     []*agentsv1alpha1.Sandbox
    Used          []*agentsv1alpha1.Sandbox
    Dead          []*agentsv1alpha1.Sandbox
}
```

Reused sandboxes naturally fall into the `Available` group.

### 7.2 Scale Delta

Reusing sandboxes have no OwnerReference, so SandboxSet is unaware of them and will replenish normally (same behavior as when a sandbox is deleted). After reuse succeeds and cooldown ends, the sandbox restores its OwnerReference. If total > replicas at that point, SandboxSet's existing scale-down logic handles the excess.

```
delta = spec.replicas - status.replicas
```

- Sandbox enters reuse → SandboxSet is unaware → replenishes normally.
- Reuse succeeds + cooldown ends → sandbox returns → total > replicas → SandboxSet scales down excess.

### 7.3 Scale-Down Priority

When `delta < 0` (scale-down needed), `scaleDown` selects sandboxes to delete in the following priority order (higher priority = deleted first):

1. **Creating sandboxes** (newest first) — not yet initialized, lowest discard cost.
2. **Reused sandboxes** (`reuseCount > 0`, higher `reuseCount` deleted first) — higher reuse count means higher risk of accumulated state leakage, prioritize for reclamation.
3. **Running but not Ready sandboxes** — not ready, cannot be claimed.
4. **Available sandboxes** (newest first) — ready and stable, highest discard cost.

## 8. Sandbox Controller Changes

### 8.1 Reuse Flow

The sandbox controller executes reuse upon detecting the `AnnotationReuse` annotation:

1. **Detect trigger**: Sandbox has both `agents.kruise.io/reuse-enabled=true` (capability) and `agents.kruise.io/reuse=true` (trigger).
   - If only `reuse=true` is present without `reuse-enabled=true`, ignore — do not enter reuse flow.
   - **Only `Running` phase is allowed**. If the sandbox is in another phase (e.g., `Paused`, `Pending`, `Terminating`), reject reuse:
     - Set `Reusing` condition to `ReuseRejected`, message explains the current phase does not allow reuse.
     - Emit Warning Event (reason: `ReuseRejected`).
     - Preserve both `AnnotationReuse` and `AnnotationReuseEnabled` — do not remove.
   - Upper-layer components handle the fallback strategy after rejection (see 5.2, 5.3).
2. **Enter Reusing (without restoring OwnerRef)**:
   - Clear `spec.shutdownTime` and `spec.pauseTime` (prevent termination during reuse by the shutdown controller).
   - Set `status.phase = Reusing`, set `Reusing` condition to `ReuseStarted`.
   - **Do NOT restore OwnerReference** — SandboxSet remains completely unaware of this sandbox.
3. **Execute cleanup** (includes user/container data cleanup and metadata restoration):
   - Clean user data and container data via `SandboxReuser.Reuse()`. Implementations are runtime-specific (e.g., kill processes, reset filesystem layers, clear tmp volumes).
   - Timeout configured via `SandboxControlArgs.ReuseTimeout` (controller flag `--reuse-timeout`, default 60s).
   - **Reset claim-modified metadata via `AnnotationUpdatedMetadataInClaim`**: for each key listed in the annotation, restore to the value from the SandboxSet template (looked up via `LabelSandboxPool`), or remove the key if it does not exist in the template. This approach stores only keys (not values), reducing annotation size on the apiserver.
   - Remove `AnnotationUpdatedMetadataInClaim` (restoration complete).
   - Remove `AnnotationReuse` (trigger), preserve `AnnotationReuseEnabled` (capability marker).
   - Increment `status.reuseCount`.
4. **On cleanup success — cooldown then return to pool**:
   - **Cooldown period** (`--reuse-grace-period`, default 10s): phase remains `Reusing`, OwnerReference not restored. SandboxSet remains unaware. Implemented via requeue delay. Purpose: the label/annotation changes above are visible to other controllers — this window allows **TrafficPolicy** and **SecurityProfile** controllers to observe the removal of claim-specific labels and clean up associated routing rules / security policies before the sandbox re-enters the pool.
   - **After cooldown — return to pool**:
     - Set `status.phase = Running`, Ready condition to True.
     - Look up the original SandboxSet via `LabelSandboxPool`, **restore OwnerReference**. SandboxSet now becomes aware and treats the sandbox directly as Available.
5. **On failure / timeout**:
   - Set `Reusing` condition: `ReuseFailed` for explicit errors, `ReuseTimeout` for timeout.
   - Emit corresponding Warning Event.
   - **Sandbox controller directly deletes the sandbox**, without relying on SandboxSet.
   - Timeout duration controlled by controller flag `--reuse-timeout` (default 60s), corresponding to `SandboxControlArgs.ReuseTimeout`.

### 8.2 OwnerRef Restoration Details (called after successful reuse)

```go
func (c *control) restoreOwnerRef(ctx context.Context, sbx *agentsv1alpha1.Sandbox) error {
    poolName := sbx.Labels[agentsv1alpha1.LabelSandboxPool]
    if poolName == "" {
        return fmt.Errorf("sandbox %s has no sandbox-pool label", sbx.Name)
    }

    sbs := &agentsv1alpha1.SandboxSet{}
    if err := c.Get(ctx, client.ObjectKey{Namespace: sbx.Namespace, Name: poolName}, sbs); err != nil {
        return fmt.Errorf("origin SandboxSet %s not found: %w", poolName, err)
    }

    sbx.OwnerReferences = []metav1.OwnerReference{
        *metav1.NewControllerRef(sbs, agentsv1alpha1.SandboxSetControllerKind),
    }
    return nil
}
```

## 9. Claim Flow Changes

### 9.1 Updated Metadata in Claim

The claim flow records the **keys** of all labels and annotations it adds or modifies into the `AnnotationUpdatedMetadataInClaim` annotation. Only keys are stored — no values — to minimize annotation size on the apiserver. The reuse flow uses these keys to determine which metadata to reset.

```go
type UpdatedMetadataInClaim struct {
    Labels      []string `json:"labels,omitempty"`
    Annotations []string `json:"annotations,omitempty"`
}
```

Recording timing: **after** the claim flow sets `sandbox-claimed`, `owner`, and other labels/annotations. The annotation lists only the keys that were added or modified during this claim (e.g., `sandbox-claimed`, `claim-name`, `owner`). During reuse, the controller looks up the origin SandboxSet template via `LabelSandboxPool` and resets each listed key to its template value, or removes it if the key does not exist in the template.

### 9.2 LabelSandboxPool Is Naturally Preserved

`LabelSandboxPool` is set when SandboxSet creates the sandbox and is not removed during claim. The reuse flow can directly read this label to find the origin SandboxSet.

## 10. SandboxReuser Interface

The sandbox controller invokes cleanup logic through the `SandboxReuser` interface, decoupled from any specific implementation. Different container runtimes and scenarios can provide different implementations (e.g., via in-Pod sidecar API, exec command, or external service).

```go
// SandboxReuser handles the actual cleanup of user and container data inside a sandbox.
// Implementations are runtime-specific and injected into the sandbox controller at startup.
type SandboxReuser interface {
    // Reuse triggers the cleanup of user and container data in the sandbox.
    // This may be asynchronous — use IsReuseComplete to poll for the result.
    Reuse(ctx context.Context, sandbox *agentsv1alpha1.Sandbox) error

    // IsReuseComplete checks whether a previously triggered reuse has finished.
    // Returns (true, nil) on success, (true, err) on failure, (false, nil) if still in progress.
    IsReuseComplete(ctx context.Context, sandbox *agentsv1alpha1.Sandbox) (complete bool, err error)
}
```

Sandbox controller reconcile flow:
1. First time entering Reusing phase → call `Reuse()` to trigger cleanup
2. Subsequent reconciles → call `IsReuseComplete()` to poll result
3. Returns `(true, nil)` → success, transition back to Running
4. Returns `(true, err)` → failure, set `ReuseFailed` condition
5. Returns `(false, nil)` → in progress, requeue for next check (subject to `--reuse-timeout`)

Implementations should perform the following cleanup (specifics are up to the implementation):

1. **Kill user processes** — terminate all user-started processes.
2. **Clean filesystem** — remove user-created files, restore initial filesystem state.
3. **Clear container data** — reset container filesystem layers, clear tmp volumes (runtime-specific).
4. **Reset environment variables** — clear environment variables set during claim.
5. **Reset network state** — clear user-created network rules or connections.
6. **Health check** — verify the sandbox is in a clean, usable state.

## 11. Metrics & Events

### 11.1 Prometheus Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `sandbox_reuse_total` | Counter | `namespace`, `result` (success/failure) | Total sandbox reuse operations |
| `sandbox_reuse_duration_seconds` | Histogram | `namespace` | Sandbox reuse operation duration |

### 11.2 Kubernetes Events

The sandbox controller emits K8s Events at key points in the reuse lifecycle, reusing condition reason constants:

| Timing | EventType | Reason | Example Message |
|--------|-----------|--------|-----------------|
| Phase does not allow reuse (non-Running) | Warning | `ReuseRejected` | `Reuse rejected: sandbox is in %s phase, only Running is allowed` |
| Reuse started (enters Reusing phase) | Normal | `ReuseStarted` | `Reuse started for sandbox %s` |
| Reuse succeeded (sandbox returned to Available) | Normal | `ReuseSucceeded` | `Reuse succeeded, sandbox returned to pool (reuseCount: %d)` |
| Reuse failed | Warning | `ReuseFailed` | `Reuse failed: %s` |
| Reuse timed out | Warning | `ReuseTimeout` | `Reuse timed out after %s` |

All reason constants are defined in section 6.1.

## 12. Edge Cases & Failure Handling

1. **SandboxSet deleted during reuse**: The sandbox cannot restore OwnerReference, is marked as Failed, and is garbage collected.

2. **SandboxSet template changes during reuse**: See open question #6.

3. **Reuse timeout**: Configured via controller flag `--reuse-timeout` (default 60s). After timeout, the sandbox controller sets `ReuseTimeout` condition, emits a Warning Event, and **directly deletes** the sandbox.

4. **Concurrent reuse and delete**: If the user explicitly deletes the sandbox during reuse, delete takes precedence (native K8s deletion semantics).

5. **Multiple reuses**: A sandbox can be reused multiple times. `status.reuseCount` tracks the count with no hard limit; operators can set policies based on this value.

6. **Reuse after in-place update**: If the sandbox underwent image/resource changes during claim, the reuse flow does not roll back those changes. After returning to pool, SandboxSet's rolling update mechanism naturally handles template-mismatched sandboxes.

7. **Over-provisioning race**: If SandboxSet already created a replacement before the reuse annotation was set, the returning sandbox causes total > replicas. SandboxSet's existing scale-down logic deletes the excess Creating or Available sandboxes.

8. **Missing SandboxPool label**: Sandboxes not created via SandboxSet do not have `LabelSandboxPool`. The reuse annotation on such sandboxes is ignored; sandbox-manager falls back to delete.

## 13. Backward Compatibility

- No new CRD spec fields — fully backward compatible.
- `LabelSandboxPool` is an existing label, requiring no additional setup. All sandboxes created via SandboxSet already have it.
- Existing sandboxes and claims continue to work normally.
- sandbox-manager only triggers reuse when the sandbox has `AnnotationReuseEnabled`. Sandboxes without this annotation behave unchanged.

## 14. Implementation Order

1. **Phase 1 — API types**: Add `SandboxReusing` phase, `AnnotationReuseEnabled`, `AnnotationReuse`, `AnnotationUpdatedMetadataInClaim` constants, `UpdatedMetadataInClaim` type, `status.reuseCount`. Remove `LabelSandboxPool` deprecated marker.
2. **Phase 2 — Sandbox controller**: Implement reuse reconciliation (detect annotations → Reusing phase → call `SandboxReuser.Reuse()` → reset claim metadata via template → cooldown → Running+Ready + restore OwnerRef → Available). Add `--reuse-timeout`, `--reuse-grace-period` flags.
3. **Phase 3 — SandboxSet controller**: No new groups or states needed. Verify existing scale-down logic correctly handles reused sandboxes returning to pool (total > replicas).
4. **Phase 4 — Sandbox-manager**: Update `DeleteSandbox` to check `AnnotationReuseEnabled` and set reuse annotation or delete directly.
5. **Phase 5 — SandboxReuser implementation**: Implement cleanup API (separate spec).
6. **Phase 6 — Metrics & observability**: Add Prometheus metrics.

## 15. Open Questions

1. ~~**Should reuse roll back in-place updates?**~~ — **Decided: No**. After returning to pool, if the sandbox doesn't match the current SandboxSet template, SandboxSet's rolling update mechanism handles it naturally.

2. ~~**Reuse timeout configuration**~~ — **Decided: Global controller flag `--reuse-timeout`**, passed via `SandboxControlArgs.ReuseTimeout`, default 60s. Per-SandboxSet annotation override can be added later as needed.

3. **Maximum reuse count** — Is an upper limit needed? After N reuses, switch to delete to prevent accumulated state leakage. Could start with a global flag.

4. ~~**LabelSandboxPool reuse**~~ — **Decided: Reuse `LabelSandboxPool`**. Its value is always the SandboxSet name (unaffected by `TemplateRef`, which is `LabelSandboxTemplate`'s semantics). Naturally preserved during claim, no need for a new `LabelSandboxSetOrigin`. Also remove `LabelSandboxPool`'s deprecated marker.

5. **CSI dynamic mount cleanup** — Should CSI mount annotations be removed during reuse? Dynamic mounts may involve actual operations like unmount (not just annotation removal). The cleanup flow and execution timing need to be clarified.

6. **SandboxSet template changes during reuse** — If the SandboxSet template changes while reuse is in progress, the returning sandbox holds the old template. Should the sandbox be deleted directly (to avoid returning a sandbox that will immediately need rolling update), or should rolling update handle it naturally?
