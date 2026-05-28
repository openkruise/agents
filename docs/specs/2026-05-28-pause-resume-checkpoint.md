# Pause/Resume Pod Checkpoint — Design

- Date: 2026-05-28
- Branch: `feat/pause-resume-checkpoint-260528`
- Scope: `pkg/controller/sandbox/core/`, `api/v1alpha1/checkpoint_types.go`

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

- At pause time, create a Checkpoint CR whose status stores the Pod diff snapshot data (annotations/labels/containers/resources) without performing actual CRIU checkpoint operations.
- At resume time, reconstruct the full Pod YAML from `sandbox.spec.template` + Checkpoint CR status, ensuring consistency with the pre-pause Pod.
- At pause time, validate user container images; if they differ from `sandbox.spec.template`, reject the pause with an error.

### Non-goals

- Do not modify external component behavior (VPA, Webhook).
- Do not handle PVC changes. Pod volumes are immutable fields with no runtime drift; even if PVCs are manually modified by users, the sandbox controller is unaware, and PVCs will not change at resume time — no checkpoint needed.

## 3. Constraints

- Checkpoint data is stored in the Checkpoint CR's status. The Checkpoint CR is associated with the Sandbox via OwnerReference. At resume time, lookup is performed via the label `agents.kruise.io/sandbox-name` on the Checkpoint CR.
- Resume still follows the `GeneratePodFromSandbox` → `createPod` path; Checkpoint CR data is merged within this flow.
- Backward compatibility: existing sandboxes without a Checkpoint CR should degrade to current behavior at resume (using `sandbox.spec.template` + runtime injection) without errors.

## 4. Approach

### 4.1 Type Changes

#### 4.1.1 Checkpoint CR Label Association

No new fields are added to `SandboxStatus`. Instead, the Checkpoint CR uses Labels to associate with the Sandbox. At resume time, lookup is performed via label selector:

```go
// Labels on the Checkpoint CR
labels:
  agents.kruise.io/sandbox-name: <sandbox-name>
  agents.kruise.io/checkpoint-type: pod-info
```

Resume lookup logic:

```go
func getCheckpointForSandbox(ctx context.Context, cli client.Client, namespace, sandboxName string) (*v1alpha1.Checkpoint, error) {
    cpList := &v1alpha1.CheckpointList{}
    err := cli.List(ctx, cpList,
        client.InNamespace(namespace),
        client.MatchingLabels{
            v1alpha1.CheckpointLabelSandboxName: sandboxName,
            v1alpha1.CheckpointLabelType:        "pod-info",
        },
    )
    if err != nil {
        return nil, err
    }
    if len(cpList.Items) == 0 {
        return nil, nil
    }
    // Return the most recently created one
    sort.Slice(cpList.Items, func(i, j int) bool {
        return cpList.Items[j].CreationTimestamp.Before(&cpList.Items[i].CreationTimestamp)
    })
    return &cpList.Items[0], nil
}
```

#### 4.1.2 Extend `CheckpointStatus`

Add a `PodTemplateDelta` field to `CheckpointStatus` in `api/v1alpha1/checkpoint_types.go`, using `runtime.RawExtension` to store a Strategic Merge Patch that captures the delta between the live Pod at pause time and the base Pod generated at resume:

```go
// CheckpointStatus defines the observed state of Checkpoint.
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

**Important**: `podTemplateDelta` does not store the complete Pod. It stores the **incremental delta** between the live Pod and the base Pod (generated from `sandbox.spec.template` + runtime injection). At resume time, the controller first generates the base Pod via the normal path, then applies this delta to produce a Pod identical to the one at pause time.

**Example**: Suppose at pause time a Sandbox Pod has the following drift relative to the base — the scheduler wrote extra annotations, VPA modified the main container resources, and a Webhook injected a sidecar container:

```json
{
  "metadata": {
    "annotations": {
      "scheduling.k8s.io/group-name": "sandbox-pool-a"
    },
    "labels": {
      "topology.kubernetes.io/zone": "cn-hangzhou-b"
    }
  },
  "spec": {
    "containers": [
      {
        "name": "main",
        "resources": {
          "requests": { "cpu": "2", "memory": "4Gi" },
          "limits": { "cpu": "4", "memory": "8Gi" }
        }
      },
      {
        "name": "istio-proxy",
        "image": "istio/proxyv2:1.20.3",
        "ports": [{ "containerPort": 15090, "name": "http-envoy-prom" }],
        "resources": {
          "requests": { "cpu": "100m", "memory": "128Mi" },
          "limits": { "cpu": "2", "memory": "1Gi" }
        }
      }
    ]
  }
}
```

In the above delta:
- `metadata.annotations` / `labels`: Whitelisted annotations/labels written by external components such as the scheduler (diff only, not full set).
- `spec.containers[name=main].resources`: Resources as modified by VPA (Strategic Merge merges by container name, overwriting only the resources field without affecting image or other fields).
- `spec.containers[name=istio-proxy]`: Full definition of the Webhook-injected sidecar (this container does not exist in the base Pod; the delta adds it).

**Advantages**:
- Generality: Automatically captures all Pod-level differences (labels, annotations, containers, resources, volumes, etc.) without enumerating each one.
- Extensibility: If new runtime drift scenarios emerge in the future (e.g., tolerations, nodeSelector modified by external components), no type definition changes are needed.
- Consistency: Uses the same pattern as `SandboxUpdateOps.Spec.Patch`, reducing cognitive overhead.

### 4.2 Creating Checkpoint CR at Pause Time (Pause Path)

In the `SandboxPausedReasonSetPause` case of `EnsureSandboxPaused`, perform Image validation **before patching pod paused annotations**. If validation fails, do not enter the pause flow — set condition to failed and return.

#### 4.2.1 Image Validation

Compare each user container's (from `sandbox.spec.template`) Image in the live Pod against the Image defined in the template. If inconsistent, set the `SandboxPaused` condition to failed and return an error, blocking the pause:

```go
func validateContainerImages(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) error {
    templateContainers := getTemplateContainers(box)
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

Condition setting on validation failure:

```go
if err := validateContainerImages(pod, box); err != nil {
    cond.Status = metav1.ConditionFalse
    cond.Reason = "ImageChanged"
    cond.Message = err.Error()
    utils.SetSandboxCondition(newStatus, *cond)
    r.recorder.Event(box, corev1.EventTypeWarning, "PauseFailed", err.Error())
    return nil
}
```

Note that this returns `nil` instead of `err`: validation failure does not trigger a requeue. The Sandbox stays in the Paused phase but the condition records the failure reason. Users can cancel the pause by setting `spec.paused = false`, and the controller will return the Sandbox to Running state normally (since the Pod is still running — paused annotations were never patched).

#### 4.2.2 Building the Pod Delta

At pause time, compute a Strategic Merge Patch between the live Pod and the "base Pod" that would be generated at resume. The base Pod is what `GeneratePodFromSandbox` (without checkpoint) would produce.

```go
func buildPodTemplateDelta(pod *corev1.Pod, basePod *corev1.Pod) (runtime.RawExtension, error) {
    // 1. Sanitize: remove runtime fields that should not be included in the delta
    //    (e.g., status, managedFields, resourceVersion, uid, etc.)
    pausedPod := sanitizePodForDelta(pod)

    // 2. Compute Strategic Merge Patch: pausedPod vs basePod
    originalJSON, _ := json.Marshal(basePod)
    modifiedJSON, _ := json.Marshal(pausedPod)

    // Use the Pod schema for strategic merge patch diff
    patchBytes, err := strategicpatch.CreateTwoWayMergePatch(originalJSON, modifiedJSON, &corev1.Pod{})
    if err != nil {
        return runtime.RawExtension{}, fmt.Errorf("failed to create strategic merge patch: %w", err)
    }

    return runtime.RawExtension{Raw: patchBytes}, nil
}

// sanitizePodForDelta strips runtime-only fields from the Pod before computing the delta.
// Only metadata (labels, annotations) and spec are preserved.
func sanitizePodForDelta(pod *corev1.Pod) *corev1.Pod {
    cleaned := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Labels:      filterLabelsByWhitelist(pod.Labels),
            Annotations: filterAnnotationsByWhitelist(pod.Annotations),
        },
        Spec: *pod.Spec.DeepCopy(),
    }
    return cleaned
}

// filterAnnotationsByWhitelist applies the existing whitelist from
// configuration.GetSandboxResumePodPersistentContent() to select annotations to preserve.
func filterAnnotationsByWhitelist(annotations map[string]string) map[string]string {
    content := configuration.GetSandboxResumePodPersistentContent()
    if content == nil {
        return nil
    }
    result := map[string]string{}
    for _, key := range content.AnnotationKeys {
        if v, ok := annotations[key]; ok && v != "" {
            result[key] = v
        }
    }
    if len(result) == 0 {
        return nil
    }
    return result
}
```

**Base Pod generation**: In the pause path, call `GeneratePodFromSandbox(box, nil)` (passing nil delta) to produce the base Pod without checkpoint restoration. Diff this base Pod against the actual live Pod to produce the delta.

#### 4.2.3 Creating the Checkpoint CR

A new Checkpoint CR is created on each pause:

```go
// 1. Generate base pod (what resume would produce without checkpoint)
basePod, err := GeneratePodFromSandbox(box, nil)

// 2. Compute delta
podTemplateDelta, err := buildPodTemplateDelta(pod, basePod)

// 3. Create Checkpoint CR
cp := &v1alpha1.Checkpoint{
    ObjectMeta: metav1.ObjectMeta{
        GenerateName: box.Name + "-",
        Namespace:    box.Namespace,
        OwnerReferences: []metav1.OwnerReference{
            *metav1.NewControllerRef(box, v1alpha1.SandboxControllerKind),
        },
        Labels: map[string]string{
            v1alpha1.CheckpointLabelSandboxName: box.Name,
            v1alpha1.CheckpointLabelType:        "pod-info",
        },
    },
    Spec: v1alpha1.CheckpointSpec{
        SandboxName: &box.Name,
        PodName:     &box.Name,
    },
}
err = r.Create(ctx, cp)

// 4. Update Checkpoint status with delta data
cp.Status.PodTemplateDelta = podTemplateDelta
cp.Status.Phase = v1alpha1.CheckpointSucceeded
err = r.Status().Update(ctx, cp)
```

The Checkpoint CR is associated with the Sandbox via `OwnerReference`, enabling cascade deletion when the Sandbox is deleted. At resume time, lookup is performed via labels `agents.kruise.io/sandbox-name` + `agents.kruise.io/checkpoint-type=pod-info`.

### 4.3 Applying Checkpoint at Resume Time (Resume Path)

At resume time, find the associated Checkpoint CR via label selector, read `status.podTemplateDelta`, and apply it as a Strategic Merge Patch to the generated base Pod:

```go
func applyPodTemplateDelta(pod *corev1.Pod, podTemplateDelta runtime.RawExtension) error {
    if podTemplateDelta.Raw == nil || len(podTemplateDelta.Raw) == 0 {
        return nil
    }

    // Marshal the current pod to JSON
    podJSON, err := json.Marshal(pod)
    if err != nil {
        return fmt.Errorf("failed to marshal pod: %w", err)
    }

    // Apply the strategic merge patch
    patchedJSON, err := strategicpatch.StrategicMergePatch(podJSON, podTemplateDelta.Raw, &corev1.Pod{})
    if err != nil {
        return fmt.Errorf("failed to apply strategic merge patch: %w", err)
    }

    // Unmarshal back into the pod
    if err := json.Unmarshal(patchedJSON, pod); err != nil {
        return fmt.Errorf("failed to unmarshal patched pod: %w", err)
    }

    return nil
}
```

### 4.4 Resume Path Integration Point

Current resume flow Pod generation chain:

```
EnsureSandboxResumed
  → createPod
    → GeneratePodFromSandbox
      → GeneratePodFromSandbox     (sandbox.spec.template → base pod)
      → InjectSandboxRuntimesUsingCache  (sandbox.spec.runtimes → sidecar injection from ConfigMap)
    → InjectResumedPod             (sandbox.status.podInfo → annotations/labels/nodeName)
```

After modification:

```
EnsureSandboxResumed
  → getCheckpointForSandbox        (by label selector: sandbox-name + checkpoint-type)
  → createPod
    → GeneratePodFromSandbox
      → GeneratePodFromSandbox     (sandbox.spec.template → base pod)
      → InjectSandboxRuntimesUsingCache  (sandbox.spec.runtimes → sidecar injection from ConfigMap)
      → applyPodTemplateDelta               (checkpoint.status.podTemplateDelta → Strategic Merge Patch applied on base pod)
```

`applyPodTemplateDelta` executes after `InjectSandboxRuntimesUsingCache`, restoring all runtime drift via SMP:
- **Labels / Annotations**: The delta contains the whitelisted annotations/labels diff.
- **Container Resources**: The delta naturally contains resource changes.
- **Runtime / Webhook containers**: The delta contains full definitions of all non-template containers. Strategic Merge merges by container name, replacing freshly-injected versions.

If no Checkpoint CR exists (backward compatibility), degrade to current behavior: use `InjectResumedPod` to inject annotations/labels saved in `podInfo`.

> **Backward compatibility note**: `createPod` must check whether a Checkpoint CR was found. If present, skip `InjectResumedPod` (Checkpoint CR already includes annotations/labels); if absent, still call `InjectResumedPod` to maintain legacy behavior.

### 4.5 Helper Methods

With the delta approach, there is no need to enumerate and distinguish container origins. `buildPodTemplateDelta` automatically captures all differences by comparing two complete Pods. Only the following helpers are needed:

```go
// getTemplateContainers returns the containers defined in sandbox.spec.template
// Used by validateContainerImages to check image drift.
func getTemplateContainers(box *agentsv1alpha1.Sandbox) []corev1.Container {
    if box.Spec.Template != nil {
        return box.Spec.Template.Spec.Containers
    }
    return nil
}

// sanitizePodForDelta strips runtime-only metadata and status from a Pod,
// keeping only the fields that should be captured in the checkpoint delta.
func sanitizePodForDelta(pod *corev1.Pod) *corev1.Pod { ... }

// filterAnnotationsByWhitelist / filterLabelsByWhitelist apply the existing whitelist.
func filterAnnotationsByWhitelist(annotations map[string]string) map[string]string { ... }
func filterLabelsByWhitelist(labels map[string]string) map[string]string { ... }
```

### 4.6 Checkpoint CR Lifecycle Management

**Creation**: A new CR is created on each pause with `GenerateName: sbx.Name + "-"` and OwnerReference pointing to the Sandbox.

**Lookup**: At resume time, find the corresponding Checkpoint CR via label selector (`agents.kruise.io/sandbox-name` + `agents.kruise.io/checkpoint-type=pod-info`), taking the most recently created one.

**Deletion**: After successful resume (Pod enters Running and Ready), delete the Checkpoint CR in `EnsureSandboxResumed`. OwnerReference ensures cascade deletion when the Sandbox is deleted.

### 4.7 Pause State Machine Extension

In the current `EnsureSandboxPaused` `SandboxPausedReasonSetPause` → `PausedSucceed` path, add checkpoint creation steps before setting `PausedSucceed`:

```
SetPause
  → validateContainerImages(pod, box)   [NEW, before any pause action]
  → patch pod paused annotations
  → wait for PodConditionContainersPaused == true
  → generate basePod via GeneratePodFromSandbox(box, nil)  [NEW]
  → buildPodTemplateDelta(pod, basePod)         [NEW]
  → create Checkpoint CR with podTemplateDelta  [NEW]
  → PausedSucceed (or DeletePod)
```

If `validateContainerImages` returns an error, set the `SandboxPaused` condition (Status=False, Reason=ImageChanged, Message=specific error), record a Warning Event, and the Sandbox stays in the `SetPause` phase without entering the pause flow.

## 5. Compatibility

### 5.1 Existing sandboxes without Checkpoint CR

For Sandboxes already in Paused state with no associated Checkpoint CR:
- Label selector List returns empty; `applyPodTemplateDelta` receives an empty delta and skips.
- `createPod` detects no Checkpoint CR and still calls `InjectResumedPod` to restore annotations/labels/nodeName from `podInfo`.
- Behavior is identical to current: uses `sandbox.spec.template` + latest ConfigMap runtime injection + `podInfo` annotations/labels.

### 5.2 Rolling upgrade

| Operator version                              | Pause behavior                                    | Resume behavior                                   |
| --------------------------------------------- | ------------------------------------------------- | ------------------------------------------------- |
| Old (no checkpoint)                           | No Checkpoint CR created                          | Resume uses template + fresh runtime injection    |
| New (with checkpoint)                         | Checkpoint CR created with PodTemplateDelta (SMP) | Resume uses template + apply PodTemplateDelta     |
| New operator, old sandbox (no Checkpoint CR)  | Checkpoint CR created on next pause               | Resume falls back gracefully                      |

### 5.3 Rollback

After rolling back to the old operator:
- Existing Checkpoint CRs do not cause errors; the old operator does not recognize the `status.podTemplateDelta` field but will not error.
- Resume uses the old path, ignoring podTemplateDelta data in the Checkpoint CR.
- Checkpoint CRs are cascade-deleted with the Sandbox via OwnerReference and will not leak.

## 6. File-level change list

### 6.1 `api/v1alpha1/checkpoint_types.go`

- Add `PodTemplateDelta runtime.RawExtension` field to `CheckpointStatus` (with `+kubebuilder:pruning:PreserveUnknownFields` and `+kubebuilder:validation:Schemaless`).
- Add label constant `CheckpointLabelType = "agents.kruise.io/checkpoint-type"`.
- Run `make generate manifests` to update deepcopy and CRD yaml.

### 6.2 `pkg/controller/sandbox/core/common_control.go`

**`EnsureSandboxPaused` modifications**:
- In the `SandboxPausedReasonSetPause` case:
  1. **Before pause**: call `validateContainerImages(pod, box)` to validate image consistency. If failed, set `SandboxPaused` condition (Status=False, Reason=ImageChanged), record Warning Event, return error, do not enter pause.
  2. After `PodConditionContainersPaused == true`, before setting `PausedSucceed`:
     - Call `GeneratePodFromSandbox(box, nil)` to generate the base Pod.
     - Call `buildPodTemplateDelta(pod, basePod)` to compute the Strategic Merge Patch.
     - Create Checkpoint CR (GenerateName, OwnerReference, labels including sandbox-name and checkpoint-type), write delta to status.

**`EnsureSandboxResumed` modifications**:
- Before resume, find the associated Checkpoint CR via label selector.
- At `newStatus.Phase = SandboxRunning`, delete the Checkpoint CR.

**`createPod` modifications**:
- When a Checkpoint CR is found, skip the `InjectResumedPod` call (Checkpoint CR already contains annotations/labels).
- When no Checkpoint CR is found (backward compatibility with old sandboxes), still call `InjectResumedPod`.

**`syncInfoFromPod` modifications**:
- Retain this function (Running phase still needs to sync podInfo to status for other features), but the pause path no longer depends on it for saving resume data.

### 6.3 `pkg/controller/sandbox/core/control.go` — `GeneratePodFromSandbox`

- Signature change: accept the Checkpoint CR's `PodTemplateDelta runtime.RawExtension` (or empty value).
- After `InjectSandboxRuntimesUsingCache`, add `applyPodTemplateDelta(pod, podTemplateDelta)` call.

### 6.4 `pkg/controller/sandbox/core/checkpoint.go` (new file)

New file containing:
- `validateContainerImages(pod *corev1.Pod, box *agentsv1alpha1.Sandbox) error`
- `buildPodTemplateDelta(pod *corev1.Pod, basePod *corev1.Pod) (runtime.RawExtension, error)`
- `applyPodTemplateDelta(pod *corev1.Pod, podTemplateDelta runtime.RawExtension) error`
- `sanitizePodForDelta(pod *corev1.Pod) *corev1.Pod`
- `filterAnnotationsByWhitelist(annotations map[string]string) map[string]string`
- `filterLabelsByWhitelist(labels map[string]string) map[string]string`
- `getCheckpointForSandbox(ctx context.Context, cli client.Client, namespace, sandboxName string) (*v1alpha1.Checkpoint, error)`
- `getTemplateContainers(box *agentsv1alpha1.Sandbox) []corev1.Container`

### 6.5 `pkg/controller/sandbox/core/checkpoint_test.go` (new file)

New test file covering all checkpoint-related function unit tests (table-driven).

### 6.6 Files explicitly not modified

| File                                                   | Reason                                                                                                          |
| ------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------- |
| `api/v1alpha1/sandbox_types.go`                        | No new fields needed; Checkpoint CR uses labels for association                                                  |
| `pkg/controller/sandbox/core/normal_pause_resume.go`   | Only affects pre-cutoff sandboxes, out of scope                                                                 |
| `pkg/controller/sandbox/core/common_control.go`        | Uses `GeneratePodFromSandbox`                                                     | 
| `pkg/utils/configuration/configuration.go`             | Whitelist mechanism still used by `buildPodInfo` for filtering annotations/labels                                |
| `pkg/sandbox-manager/`                                 | Manager layer is not involved in controller-level pause/resume                                                  |

## 7. Risks & mitigations

| Risk | Trigger | Mitigation |
| --- | --- | --- |
| Checkpoint CR size exceeds etcd value limit (1.5MB) | Sandbox with many large containers + sidecars | Unlikely in practice. Container specs are typically small. If hit, add a size check before writing and fall back to no-checkpoint behavior with a warning event. |
| Checkpoint CR creation succeeds but status update fails | Network/API error between create and status update | Orphan Checkpoint CR with empty podTemplateDelta. Resume falls back to old behavior (empty delta = no-op). Next pause creates a new Checkpoint CR. Old one cleaned up by OwnerReference cascade or manual cleanup. |
| Injected container names change between versions | Operator upgrade changes sidecar naming or Webhook changes container names | Strategic Merge Patch merges by container name: if a checkpoint container name matches a freshly-injected one, the patch version wins. If the name changed, both old (from patch) and new (from injection) will be present. This is acceptable — the checkpoint faithfully restores the pause-time Pod; the extra new container is harmless and will be resolved on the next pause-resume cycle. |
| Image validation blocks legitimate pause | Edge case where image tag resolves to same digest but different tag | Compare image strings literally (current approach). This matches the stated requirement: image modification is not supported. Tag-level comparison is correct. |
| Multiple Checkpoint CRs for one Sandbox | Repeated pause without resume (edge case) | Label selector List sorts by CreationTimestamp and takes the newest one. Old ones are cleaned up by OwnerReference cascade on Sandbox deletion. |
| Template resolved via `TemplateRef` changes between pause and resume | User updates `SandboxTemplate` CR | Checkpoint delta was computed against the template-at-pause-time baseline. If template changes, delta may conflict or produce unexpected results. Acceptable: `sandbox.spec.template` is expected to be immutable for a given sandbox instance. |
| `applyPodTemplateDelta` | Code path mixup | `applyPodTemplateDelta` is called only from `GeneratePodFromSandbox`. Common path uses `GeneratePodFromSandbox` without checkpoint. |

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
- [ ] `go test ./pkg/controller/sandbox/core/...` green, including new `checkpoint_test.go`
- [ ] No string-literal container names in checkpoint logic (use constants or computed sets)

### 9.2 Post-deploy

- [ ] Create a sandbox with `spec.runtimes` configured (e.g., agent-runtime + csi).
      Wait for Running. Verify containers include template + runtime-injected containers.
- [ ] Pause the sandbox. Verify a Checkpoint CR is created with labels `CheckpointLabelSandboxName` + `CheckpointLabelType`:
      - `status.podTemplateDelta` contains a valid Strategic Merge Patch JSON capturing the diff (annotations, labels, extra containers, resource changes).
- [ ] Resume the sandbox. Verify the recreated Pod has identical spec
      (same annotations, labels, containers, images, resources) as the Pod before pause.
- [ ] Verify the Checkpoint CR is deleted after successful resume.
- [ ] Modify the sidecar injection ConfigMap (change a sidecar image tag).
      Create a new sandbox — it should get the new sidecar image.
      Resume the previously paused sandbox — it should get the old sidecar image from checkpoint.
- [ ] Attempt to pause a sandbox where a user manually changed a container image
      via `kubectl edit pod`. Verify pause fails with a clear error event.
- [ ] Resume a sandbox that was paused before this change (no Checkpoint CR).
      Verify resume succeeds using the old behavior (template + fresh runtime injection).

## 10. Metrics

No new metrics. The existing pause/resume duration metrics continue to cover the
checkpoint write/apply as part of the overall pause/resume latency.
