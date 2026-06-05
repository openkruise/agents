# Pause/Resume Pod Checkpoint — Design

- Date: 2026-05-28
- Branch: `sandbox-pause-resume-checkpoint`
- Scope: `api/v1alpha1/`, `pkg/controller/sandbox/`, `pkg/features/`, `pkg/utils/checkpoint/`, `pkg/utils/fieldindex/`

## 1. Problem

The Sandbox Controller deletes the Pod on pause and creates a new Pod on resume. The recreated Pod must have the same YAML as the Pod at pause time.

Currently, the Pod is created from `sandbox.spec.template`, but the live Pod YAML can drift from `sandbox.spec.template` for the following reasons:

1. **Labels / Annotations drift**: Some labels and annotations are written by external components (scheduler, monitoring, VK, etc.) after Pod creation and do not exist in `sandbox.spec.template`. Currently preserved via `syncInfoFromPod` with a whitelist from `configuration.GetSandboxResumePodPersistentContent()` into `sandbox.status.podInfo`, then injected at resume via `InjectResumedPod`. The whitelist filtering mechanism is retained, but the storage location migrates from `sandbox.status.podInfo` to the Checkpoint CR for unified management.

2. **User container Resources changes**: VPA or similar components may modify `resources` fields (CPU/Memory requests/limits) of containers defined in `sandbox.spec.template`. This behavior is supported, but currently the changes are not captured at pause time, causing resume to use original resources from `sandbox.spec.template`, inconsistent with the pre-pause state.

3. **Runtime-injected containers**: Sidecar containers (e.g., agent-runtime, csi, traffic-proxy) injected via `sandbox.spec.runtimes` configuration by `InjectSandboxRuntimesUsingCache` reading templates from ConfigMap. The ConfigMap contents may be updated during the Sandbox lifecycle, causing resume to inject different container configurations than at pause time.

4. **Webhook-injected containers**: Containers (init containers and sidecar containers) injected by third-party Admission Webhooks at Pod Create time. These containers do not exist in `sandbox.spec.template` or `sandbox.spec.runtimes` configuration, only in the live Pod YAML.

5. **User container Image changes**: Users directly modifying images via `kubectl update pod` is **not supported**; the pause operation should validate and reject this.

## 2. Goal & Non-goals

### Goals

- At pause time, create a Checkpoint CR that triggers a separate checkpoint controller to compute and store the Pod diff snapshot data (annotations/labels/containers/resources) in the Checkpoint CR status.
- At resume time, reconstruct the full Pod YAML from `sandbox.spec.template` + Checkpoint CR status, ensuring consistency with the pre-pause Pod.
- At pause time, validate user container images; if they differ from `sandbox.spec.template`, reject the pause with an error.
- Gate the entire checkpoint flow behind a feature gate (`SandboxPauseCheckpoint`, default disabled) for safe rollout.

### Non-goals

- Do not modify external component behavior (VPA, Webhook).
- Do not handle PVC changes. Pod volumes are immutable fields with no runtime drift; even if PVCs are manually modified by users, the sandbox controller is unaware, and PVCs will not change at resume time — no checkpoint needed.
- Do not implement the checkpoint controller itself (provided in a future version). The sandbox controller assumes the checkpoint controller exists and watches its status.

## 3. Constraints

- Checkpoint data is stored in the Checkpoint CR's status by the checkpoint controller. The Checkpoint CR is associated with the Sandbox via OwnerReference.
- At resume time, lookup is performed via ownerRef UID field index (`fieldindex.IndexNameForOwnerRefUID`) for performance, combined with a label filter for checkpoint type (`agents.kruise.io/checkpoint-type=pod-info`).
- Resume still follows the `GeneratePodFromSandbox` → `createPod` path; Checkpoint CR data is merged within this flow.
- Backward compatibility: existing sandboxes without a Checkpoint CR should degrade to current behavior at resume (using `sandbox.spec.template` + runtime injection) without errors.

## 4. Approach

### 4.1 Feature Gate

A new feature gate `SandboxPauseCheckpoint` is added to `pkg/features/features.go`, default **disabled** (Alpha):

```go
SandboxPauseCheckpointGate featuregate.Feature = "SandboxPauseCheckpoint"
```

All three public methods on `CheckpointControl` (`PreparePodInfo`, `GetPodTemplateDelta`, `Cleanup`) check the gate at entry and return no-op when disabled.

### 4.2 CheckpointControl Struct

Checkpoint lifecycle logic is encapsulated in a reusable `CheckpointControl` struct (similar to `RateLimiter`), allowing other control implementations (e.g., `acs_common`) to share the same logic:

```go
// pkg/controller/sandbox/core/checkpoint.go
type CheckpointControl struct {
    client.Client
    recorder record.EventRecorder
}

func NewCheckpointControl(cli client.Client, recorder record.EventRecorder) *CheckpointControl
```

`CheckpointControl` is injected into `SandboxControlArgs` and stored on `commonControl`:

```go
// pkg/controller/sandbox/core/interface.go
type SandboxControlArgs struct {
    Client            client.Client
    APIReader         client.Reader
    Recorder          record.EventRecorder
    RateLimiter       *RateLimiter
    CheckpointControl *CheckpointControl
}
```

Created in `sandbox_controller.go:Add()`:

```go
checkpointControl := core.NewCheckpointControl(mgr.GetClient(), recorder)
```

#### Public Methods

| Method | Description | Caller |
|--------|-------------|--------|
| `PreparePodInfo(ctx, pod, box, newStatus, cond) bool` | Pause flow: validate images → manage checkpoint lifecycle. Returns true if pause should wait. | `EnsureSandboxPaused` |
| `GetPodTemplateDelta(ctx, box) *runtime.RawExtension` | Resume flow: retrieve the latest checkpoint's pod template delta. | `EnsureSandboxResumed` |
| `Cleanup(ctx, box)` | Resume success: delete all checkpoint CRs for the sandbox. | `EnsureSandboxResumed` |

### 4.3 Type Changes

#### 4.3.1 Paused Condition Reasons

New reason constants in `api/v1alpha1/sandbox_types.go`:

```go
SandboxPausedReasonCheckpointCreating  = "CheckpointCreating"
SandboxPausedReasonCheckpointSucceeded = "CheckpointSucceeded"
SandboxPausedReasonCheckpointFailed    = "CheckpointFailed"
```

These reasons on the `SandboxConditionPaused` condition track the checkpoint lifecycle state during pause.

#### 4.3.2 Extend `CheckpointStatus`

A `PodTemplateDelta` field in `CheckpointStatus` stores a Strategic Merge Patch capturing the delta between the live Pod at pause time and the base Pod generated at resume:

```go
type CheckpointStatus struct {
    // ... existing fields ...

    // PodTemplateDelta stores a Strategic Merge Patch that captures the delta between
    // the running Pod at pause time and the base Pod generated from sandbox.spec.template
    // + runtime injection. Applied at resume time to reconstruct the Pod faithfully.
    // +optional
    // +kubebuilder:pruning:PreserveUnknownFields
    // +kubebuilder:validation:Schemaless
    PodTemplateDelta runtime.RawExtension `json:"podTemplateDelta,omitempty"`
}
```

#### 4.3.3 Checkpoint CR Labels and OwnerReference

Each Checkpoint CR carries:

- **OwnerReference**: Controller ref pointing to the owning Sandbox, enabling cascade deletion.
- **Labels**:
  - `agents.kruise.io/sandbox-name: <sandbox-name>` — used by `CheckpointEventHandler` to enqueue the sandbox.
  - `agents.kruise.io/checkpoint-type: pod-info` — used as a list filter.

### 4.4 Pause Path — `PreparePodInfo`

`PreparePodInfo` is called from `EnsureSandboxPaused` before pod deletion. It uses the `SandboxConditionPaused` condition's `Reason` field as a state marker to distinguish between "fresh pause" and "checkpoint in progress":

```
PreparePodInfo(ctx, pod, box, newStatus, cond)
  │
  ├─ feature gate disabled? → return false (skip checkpoint)
  │
  ├─ validateContainerImages(pod, box)
  │     └─ image mismatch → set Reason=ImageChanged, return true (block pause)
  │
  ├─ listCheckpointsForSandbox(ctx, cli, box)
  │
  ├─ cond.Reason == CheckpointCreating && checkpoints exist?
  │     ├─ Checkpoint Succeeded → set Reason=CheckpointSucceeded, return false (proceed to delete pod)
  │     ├─ Checkpoint Failed → set Reason=CheckpointFailed, return true (block pause)
  │     └─ default (Pending/Creating) → return true (wait)
  │
  ├─ else (fresh pause):
  │     ├─ delete stale checkpoints from previous cycles (with ScaleExpectation)
  │     ├─ createCheckpoint(ctx, pod, box) (with ScaleExpectation)
  │     └─ set Reason=CheckpointCreating, return true (wait for checkpoint controller)
```

#### 4.4.1 Image Validation

Compares each user container's Image in the live Pod against `sandbox.spec.template`. If any image differs, the pause is rejected:

```go
func validateContainerImages(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) error {
    var templateContainers []corev1.Container
    if box.Spec.Template != nil {
        templateContainers = box.Spec.Template.Spec.Containers
    }
    for _, tc := range templateContainers {
        for _, pc := range pod.Spec.Containers {
            if tc.Name == pc.Name && tc.Image != pc.Image {
                return fmt.Errorf("container %q image changed from %q to %q, pause is not allowed",
                    tc.Name, tc.Image, pc.Image)
            }
        }
    }
    return nil
}
```

#### 4.4.2 Creating the Checkpoint CR

The sandbox controller only creates the CR with spec fields. The checkpoint controller (separate component) is responsible for computing the pod template delta and updating the status:

```go
func (c *CheckpointControl) createCheckpoint(ctx context.Context, pod *corev1.Pod, box *agentsv1alpha1.Sandbox) error {
    cpName := box.Name + "-" + utils.HashData([]byte(pod.Name+string(pod.UID)))
    cp := &agentsv1alpha1.Checkpoint{
        ObjectMeta: metav1.ObjectMeta{
            Name:      cpName,
            Namespace: box.Namespace,
            OwnerReferences: []metav1.OwnerReference{
                *metav1.NewControllerRef(box, sandboxControllerKind),
            },
            Labels: map[string]string{
                agentsv1alpha1.CheckpointLabelSandboxName: box.Name,
                agentsv1alpha1.CheckpointLabelType:        agentsv1alpha1.CheckpointTypePodInfo,
            },
        },
        Spec: agentsv1alpha1.CheckpointSpec{
            SandboxName: &box.Name,
            PodName:     &pod.Name,
        },
    }
    ScaleExpectation.ExpectScale(GetControllerKey(box), expectations.Create, cpName)
    if err := c.Create(ctx, cp); err != nil {
        ScaleExpectation.ObserveScale(GetControllerKey(box), expectations.Create, cpName)
        return fmt.Errorf("failed to create checkpoint CR: %w", err)
    }
    return nil
}
```

Key design decisions:

- **Deterministic naming**: `box.Name + "-" + utils.HashData(pod.Name + pod.UID)` instead of `GenerateName`, enabling `ScaleExpectation` tracking.
- **ScaleExpectation**: Applied before create/delete to avoid informer cache delay issues causing repeated reconciliation.
- **Delegation**: The sandbox controller does NOT compute the pod delta or update checkpoint status. The checkpoint controller handles this asynchronously. The sandbox controller polls the checkpoint status on subsequent reconciles.

#### 4.4.3 Stale Checkpoint Cleanup

On a fresh pause (cond.Reason != CheckpointCreating), any existing checkpoints from previous pause/resume cycles are deleted before creating a new one. This prevents accumulation across multiple pause/resume cycles.

### 4.5 Resume Path

#### 4.5.1 GetPodTemplateDelta

Before creating the pod, `EnsureSandboxResumed` retrieves the delta:

```go
delta := r.checkpointControl.GetPodTemplateDelta(ctx, box)
_, err = r.createPod(ctx, CreatePodArgs{Box: box, NewStatus: newStatus, PodTemplateDelta: delta})
```

`GetPodTemplateDelta` lists checkpoints via ownerRef UID field index, returns the newest checkpoint's `status.podTemplateDelta`, or nil if no checkpoint exists.

#### 4.5.2 Applying the Delta

Inside `createPod`, the delta is applied after sidecar injection:

```
createPod(ctx, args)
  → EnsureAllCACerts (if SecurityIdentityProviderGate enabled)
  → GeneratePodFromSandbox (sandbox.spec.template → base pod)
  → InjectSandboxRuntimes (sandbox.spec.runtimes → sidecar injection from ConfigMap)
  → ApplyPodTemplateDelta (checkpoint.status.podTemplateDelta → Strategic Merge Patch)
  → Create pod (with ScaleExpectation)
```

`ApplyPodTemplateDelta` lives in `pkg/utils/checkpoint/utils.go` and applies a Strategic Merge Patch:

```go
func ApplyPodTemplateDelta(pod *corev1.Pod, podTemplateDelta runtime.RawExtension) error {
    podJSON, _ := json.Marshal(pod)
    patchedJSON, err := strategicpatch.StrategicMergePatch(podJSON, podTemplateDelta.Raw, &corev1.Pod{})
    if err != nil {
        return err
    }
    return json.Unmarshal(patchedJSON, pod)
}
```

#### 4.5.3 Cleanup

After successful resume (pod is Running and Ready), `EnsureSandboxResumed` calls `Cleanup` to delete all checkpoint CRs.

### 4.6 CreatePodArgs

`createPod` uses an args struct for clean parameter management:

```go
type CreatePodArgs struct {
    Box              *agentsv1alpha1.Sandbox
    NewStatus        *agentsv1alpha1.SandboxStatus
    PodTemplateDelta *runtime.RawExtension
}

func (r *commonControl) createPod(ctx context.Context, args CreatePodArgs) (*corev1.Pod, error)
```

### 4.7 Listing Checkpoints — OwnerRef UID Field Index

For efficient checkpoint listing, an ownerRef UID field index is registered on the Checkpoint type in `pkg/utils/fieldindex/register.go`:

```go
func RegisterFieldIndexes(c cache.Cache) error {
    // ... existing indexes ...
    if err = c.IndexField(context.TODO(), &agentsv1alpha1.Checkpoint{}, IndexNameForOwnerRefUID, OwnerIndexFunc); err != nil {
        return
    }
}
```

`listCheckpointsForSandbox` uses this index combined with a label filter for type:

```go
func listCheckpointsForSandbox(ctx context.Context, cli client.Client, box *agentsv1alpha1.Sandbox) ([]agentsv1alpha1.Checkpoint, error) {
    cpList := &agentsv1alpha1.CheckpointList{}
    err := cli.List(ctx, cpList,
        client.InNamespace(box.Namespace),
        client.MatchingFields{fieldindex.IndexNameForOwnerRefUID: string(box.UID)},
        client.MatchingLabels{agentsv1alpha1.CheckpointLabelType: agentsv1alpha1.CheckpointTypePodInfo},
    )
    // sort newest-first by creation timestamp
    sort.Slice(cpList.Items, func(i, j int) bool {
        return cpList.Items[j].CreationTimestamp.Before(&cpList.Items[i].CreationTimestamp)
    })
    return cpList.Items, nil
}
```

### 4.8 CheckpointEventHandler

A dedicated event handler in `pkg/controller/sandbox/checkpoint_event_handler.go` watches Checkpoint CR events and:

- **Create**: Observes `ScaleExpectation` (Create) + enqueues owning Sandbox for reconciliation.
- **Update**: Enqueues owning Sandbox (so the controller re-evaluates checkpoint status).
- **Delete**: Observes `ScaleExpectation` (Delete) + enqueues owning Sandbox.

The owning Sandbox is extracted from `metav1.GetControllerOfNoCopy(obj)` (ownerReference), not from labels:

```go
func checkpointOwnerKey(obj client.Object) (string, reconcile.Request) {
    owner := metav1.GetControllerOfNoCopy(obj)
    if owner == nil {
        return "", reconcile.Request{}
    }
    nn := types.NamespacedName{Namespace: obj.GetNamespace(), Name: owner.Name}
    return nn.String(), reconcile.Request{NamespacedName: nn}
}
```

Registered in `SetupWithManager`:

```go
Watches(&agentsv1alpha1.Checkpoint{}, &CheckpointEventHandler{})
```

## 5. Compatibility

### 5.1 Existing sandboxes without Checkpoint CR

For Sandboxes already in Paused state with no associated Checkpoint CR:
- `listCheckpointsForSandbox` returns empty; `GetPodTemplateDelta` returns nil.
- `createPod` receives nil `PodTemplateDelta`, skips `ApplyPodTemplateDelta`.
- Behavior is identical to current: uses `sandbox.spec.template` + latest ConfigMap runtime injection.

### 5.2 Feature gate disabled (default)

All three `CheckpointControl` methods are no-ops:
- `PreparePodInfo` returns false (no checkpoint blocking pause).
- `GetPodTemplateDelta` returns nil (no delta applied at resume).
- `Cleanup` returns immediately (no deletion).

The pause/resume flow operates exactly as before.

### 5.3 Rolling upgrade

| Operator version | Pause behavior | Resume behavior |
|---|---|---|
| Old (no checkpoint) | No Checkpoint CR created | Resume uses template + fresh runtime injection |
| New (gate disabled) | Same as old | Same as old |
| New (gate enabled) | Checkpoint CR created, waits for checkpoint controller | Resume uses template + apply PodTemplateDelta |
| New operator, old sandbox (no Checkpoint CR) | Checkpoint CR created on next pause | Resume falls back gracefully |

### 5.4 Rollback

After rolling back to the old operator:
- Existing Checkpoint CRs do not cause errors; the old operator does not recognize the `status.podTemplateDelta` field but will not error.
- Resume uses the old path, ignoring podTemplateDelta data in the Checkpoint CR.
- Checkpoint CRs are cascade-deleted with the Sandbox via OwnerReference and will not leak.

## 6. File-level change list

### 6.1 `api/v1alpha1/checkpoint_types.go`

- Add `PodTemplateDelta runtime.RawExtension` field to `CheckpointStatus`.
- Label and type constants already existed: `CheckpointLabelSandboxName`, `CheckpointLabelType`, `CheckpointTypePodInfo`.
- Run `make generate manifests` to update deepcopy and CRD yaml.

### 6.2 `api/v1alpha1/sandbox_types.go`

- Add condition reason constants: `SandboxPausedReasonCheckpointCreating`, `SandboxPausedReasonCheckpointSucceeded`, `SandboxPausedReasonCheckpointFailed`.

### 6.3 `pkg/features/features.go`

- Add `SandboxPauseCheckpointGate` feature gate (default false, Alpha).

### 6.4 `pkg/controller/sandbox/core/checkpoint.go` (new file)

Contains:
- `CheckpointControl` struct and `NewCheckpointControl` constructor.
- Public methods: `PreparePodInfo`, `GetPodTemplateDelta`, `Cleanup`.
- Private method: `createCheckpoint`.
- Package-level functions: `validateContainerImages`, `listCheckpointsForSandbox`.

### 6.5 `pkg/controller/sandbox/core/checkpoint_test.go` (new file)

Table-driven tests covering:
- `TestValidateContainerImages`: image match, image changed, nil template, extra containers.
- `TestListCheckpointsForSandbox`: no checkpoints, single, multiple (sort order), different sandbox.
- `TestPreparePodInfo`: feature gate disabled, image changed, fresh pause, checkpoint succeeded/failed/in-progress, stale cleanup.
- `TestGetPodTemplateDelta`: feature gate disabled, no checkpoints, with delta, empty delta.
- `TestCleanup`: feature gate disabled, deletes all.
- `TestCreateCheckpoint`: verifies CR fields (spec, labels, ownerRef).

### 6.6 `pkg/controller/sandbox/core/interface.go`

- Add `CheckpointControl *CheckpointControl` to `SandboxControlArgs`.

### 6.7 `pkg/controller/sandbox/core/common_control.go`

- Add `checkpointControl *CheckpointControl` field to `commonControl` struct.
- `EnsureSandboxPaused`: call `r.checkpointControl.PreparePodInfo(...)` before pod deletion.
- `EnsureSandboxResumed`: call `r.checkpointControl.GetPodTemplateDelta(...)` before `createPod`, call `r.checkpointControl.Cleanup(...)` after successful resume.
- `createPod`: refactored to accept `CreatePodArgs` struct; applies `PodTemplateDelta` when present.
- `CreatePodArgs` struct defined here.

### 6.8 `pkg/controller/sandbox/sandbox_controller.go`

- Create `CheckpointControl` in `Add()` and pass to `SandboxControlArgs`.
- Register `CheckpointEventHandler` in `SetupWithManager`.

### 6.9 `pkg/controller/sandbox/checkpoint_event_handler.go` (new file)

- `CheckpointEventHandler` struct implementing `handler.EventHandler`.
- `checkpointOwnerKey` extracts sandbox info from ownerReference (not labels).

### 6.10 `pkg/utils/fieldindex/register.go`

- Register ownerRef UID field index for `Checkpoint` type.

### 6.11 `pkg/utils/checkpoint/utils.go` (new file)

- `ApplyPodTemplateDelta(pod, delta)` — applies Strategic Merge Patch to a Pod.

### 6.12 `pkg/utils/configuration/configuration.go` (new file)

- `GetSandboxResumePodPersistentContent()` — reads whitelist config for annotation/label filtering.

### 6.13 Files explicitly not modified

| File | Reason |
|------|--------|
| `pkg/sandbox-manager/` | Manager layer is not involved in controller-level pause/resume |
| `pkg/controller/sandbox/core/util.go` | `GeneratePodFromSandbox` unchanged; delta application is in `createPod` |

## 7. Risks & mitigations

| Risk | Trigger | Mitigation |
|------|---------|------------|
| Checkpoint controller not deployed | Gate enabled without checkpoint controller running | Checkpoint stays in Pending/Creating phase indefinitely; pause flow blocks at `PreparePodInfo` (returns true). Sandbox stays Paused with condition reason=CheckpointCreating. User can cancel pause by setting `spec.paused=false`. |
| Checkpoint CR size exceeds etcd value limit (1.5MB) | Sandbox with many large containers + sidecars | Unlikely in practice. Container specs are typically small. If hit, checkpoint controller should set phase=Failed with a message. |
| Multiple pause/resume cycles leave stale checkpoints | Rapid pause/resume toggling | Fresh pause deletes all existing checkpoints before creating a new one. |
| Informer cache delay causes duplicate reconciliation | Checkpoint create/delete event not yet reflected in cache | ScaleExpectation pattern prevents premature reconciliation. CheckpointEventHandler observes expectations on create/delete events. |
| Image validation blocks legitimate pause | Edge case where image tag resolves to same digest but different tag | Compare image strings literally (current approach). This matches the stated requirement: image modification is not supported. |
| Template resolved via `TemplateRef` changes between pause and resume | User updates `SandboxTemplate` CR | Checkpoint delta was computed against the template-at-pause-time baseline. If template changes, delta may conflict or produce unexpected results. Acceptable: `sandbox.spec.template` is expected to be immutable for a given sandbox instance. |

## 8. Rollback

Revert the operator image to the pre-change tag. Existing Checkpoint CRs with
`status.podTemplateDelta` are harmless: the old operator does not read the
`podTemplateDelta` field and uses the old resume path. Checkpoint CRs are cleaned
up by OwnerReference cascade when the Sandbox is deleted. No CR migration
required.

## 9. Verification

### 9.1 Pre-merge

- [ ] `make generate manifests` succeeds, CRD yaml includes `podTemplateDelta` field in CheckpointStatus
- [ ] `go vet ./...`, `go build ./...` succeed
- [ ] `golangci-lint run` passes; cyclomatic complexity of modified functions does not exceed 32
- [ ] `go test ./pkg/controller/sandbox/core/...` green, including `checkpoint_test.go`
- [ ] Feature gate disabled by default; no behavior change without explicit opt-in

### 9.2 Post-deploy (with feature gate enabled)

- [ ] Create a sandbox with `spec.runtimes` configured.
      Wait for Running. Verify containers include template + runtime-injected containers.
- [ ] Pause the sandbox. Verify:
      - `SandboxConditionPaused` reason transitions: DeletePod → CheckpointCreating → CheckpointSucceeded → ConditionTrue.
      - A Checkpoint CR is created with correct labels, ownerReference, and spec fields.
      - After checkpoint controller processes it, `status.podTemplateDelta` contains a valid Strategic Merge Patch.
- [ ] Resume the sandbox. Verify the recreated Pod has identical spec
      (same annotations, labels, containers, images, resources) as the Pod before pause.
- [ ] Verify the Checkpoint CR is deleted after successful resume.
- [ ] Attempt to pause a sandbox where a user manually changed a container image.
      Verify pause is blocked with condition reason=ImageChanged.
- [ ] Resume a sandbox that was paused before this change (no Checkpoint CR).
      Verify resume succeeds using the old behavior (template + fresh runtime injection).
- [ ] Pause/resume multiple times rapidly. Verify stale checkpoints are cleaned up
      and only one checkpoint exists per pause cycle.

## 10. Metrics

No new metrics. The existing pause/resume duration metrics continue to cover the
checkpoint write/apply as part of the overall pause/resume latency.