---
title: "okactl set image: Status Query and Auto-Diagnosis"
authors:
  - "@mahe"
creation-date: 2026-06-16
status: implemented
---

# okactl set image: Status Query and Auto-Diagnosis

## Background

After implementing the `okactl set image sandboxset` command, users faced the following issues:

1. **Unable to confirm update success**: After running `set image`, the CLI only returns "image updated", but users have no way to know whether the image update actually took effect or whether Pods were successfully rebuilt.
2. **Difficult error diagnosis**: When image pulls fail (e.g., ImagePullBackOff) or resources are insufficient, users need to manually run multiple kubectl commands to troubleshoot.
3. **Confusing status display**: The previous display showed `0/2 available` where the denominator was the actual available count rather than the desired replica count, causing the numbers to fluctuate during rolling updates and making progress hard to understand.

## Goals

- Provide a `set image status` subcommand for one-click visibility into SandboxSet image update progress.
- When an update is stalled, automatically diagnose the root cause (image pull failure, insufficient resources, etc.) without requiring manual troubleshooting.
- Unify the status display format, using the desired replica count as the denominator for clear progress tracking.

## Proposal

### 1. Architecture: Separate set image from SandboxUpdateOps

**Problem**: Previously `set image` supported both `sandboxset` and `sandbox` resource types, mixing concerns. The SandboxUpdateOps feature also depended on a controller that was not deployed in the cluster.

**Solution**:
- `set image` only supports `sandboxset` (sbs) resources, directly modifying the SandboxSet template.
- SandboxUpdateOps functionality will be implemented as a separate `okactl create suo` command in the future.

### 2. New `set image status` Subcommand

**Command format**:
```bash
okactl set image status NAME [--wait]
```

**Features**:
- Reads the SandboxSet's `status.updatedReplicas` and `status.availableReplicas` fields.
- Display format: `my-pool  Updating  1/3 updated  1/3 available`
- Shows `Complete` when `updatedReplicas == spec.replicas && availableReplicas == spec.replicas`.
- In `--wait` mode, polls every 3 seconds until the update is fully complete.

### 3. Auto-Diagnosis Mechanism

**Trigger conditions**:
- When the user runs `status`, if the update is not progressing, diagnosis runs immediately.
- In `--wait` mode, if progress does not change for 3 consecutive polls (~10 seconds), diagnosis is triggered.

**Diagnosis logic** (three-layer error source):
```
1. sandbox.Status.Message          -> Use directly if available
2. Pod conditions (PodScheduled)   -> Reports scheduling failures (e.g., "Insufficient cpu")
3. Container statuses              -> Reports runtime issues (ImagePullBackOff, CrashLoopBackOff, etc.)
```

**Output format** (kubectl-style, no emoji):
```
openclaw-sbs  Updating  0/3 updated  0/3 available
  Sandbox openclaw-sbs-xxx is Pending: gateway: ImagePullBackOff
  Sandbox openclaw-sbs-yyy is Pending: 0/3 nodes are available: insufficient cpu
```

**Deduplication**: Uses a `map[string]bool` to track reported sandboxes, preventing repeated output in `--wait` mode.

### 4. Status Display Optimization

**Before**:
```
my-pool  Updating  1/3 updated  1/2 available
```
The denominator `2` is `status.availableReplicas` (current available count), which changes during rolling updates and confuses users.

**After**:
```
my-pool  Updating  1/3 updated  1/3 available
```
The denominator `3` is `spec.replicas` (desired count), fixed and intuitive.

## Key Design Decisions

### 1. Why use `spec.replicas` instead of `status.availableReplicas` as the denominator?

- `spec.replicas` is the user's desired state, fixed and unchanging.
- `status.availableReplicas` fluctuates during rolling updates (old Pods go down, new Pods come up), making the denominator jump around.
- A fixed denominator gives users an intuitive understanding of progress: `1/3` means 1 of 3 is done.

### 2. Why does the diagnosis logic query Pod status?

- The Sandbox's `status.message` may be empty (controller does not always populate it).
- Pod `containerStatuses` contain detailed error information (e.g., `ImagePullBackOff`, `CrashLoopBackOff`).
- Pod `conditions[PodScheduled]` contain scheduling failure reasons (e.g., `insufficient cpu`).
- Three-layer querying ensures coverage of all common failure scenarios.

### 3. Why only trigger diagnosis when stalled?

- Avoids querying all Pods on every `status` invocation (performance overhead).
- Under normal conditions, users only need to see progress, not diagnostics.
- Diagnosis is needed only when stuck, matching the user's mental model.

### 4. Why remove emoji symbols (checkmark, warning)?

- Follows kubectl's plain-text output style for CLI consistency.
- Plain text is easier to parse in scripts and capture in logs.
- Avoids rendering issues across different terminal environments.

## Implementation Details

### Core Functions

- `runSetImageStatusWithClient`: Status query entry point, supports immediate query and `--wait` mode.
- `waitForSandboxSetUpdate`: Polling logic, tracks progress and triggers diagnosis.
- `diagnoseSandboxSetUpdate`: Diagnosis logic, queries sandbox and pod status.
- `printSandboxSetStatus`: Status printing, uses `spec.replicas` as the denominator.

### New Dependencies

- `kubernetes.Interface`: For querying Pod status (via `globalOpts.KubeClient()`).
- `options.go` adds a new `KubeClient()` method.

### Test Coverage

- `TestSetImageStatus`: Covers sandboxset not found, update complete, and updating in progress.
- `TestIsSandboxSetUpdateComplete`: Covers complete, in-progress, updated-but-unavailable, and zero-replicas.
- All existing tests continue to pass (8 + 6 = 14 cases).

## Usage Examples

### Normal Update Flow

```bash
$ okactl set image sbs openclaw-sbs gateway=mirrors-ssl.aliyuncs.com/ghcr.io/openclaw/openclaw:2026.4.24
sandboxset.agents.kruise.io/openclaw-sbs image updated (gateway)

$ okactl set image status openclaw-sbs
openclaw-sbs  Updating  0/3 updated  0/3 available

$ okactl set image status openclaw-sbs --wait
openclaw-sbs  Updating  0/3 updated  0/3 available
openclaw-sbs  Updating  1/3 updated  1/3 available
openclaw-sbs  Updating  2/3 updated  2/3 available
openclaw-sbs  Updating  3/3 updated  3/3 available
Update completed (3/3 updated, 3/3 available)
```

### Image Pull Failure

```bash
$ okactl set image sbs openclaw-sbs gateway=nginx:invalid-tag
sandboxset.agents.kruise.io/openclaw-sbs image updated (gateway)

$ okactl set image status openclaw-sbs
openclaw-sbs  Updating  0/3 updated  0/3 available
  Sandbox openclaw-sbs-xxx is Pending: gateway: ImagePullBackOff
  Sandbox openclaw-sbs-yyy is Pending: gateway: ImagePullBackOff
```

### Insufficient Resources

```bash
$ okactl set image status openclaw-sbs
openclaw-sbs  Updating  0/3 updated  0/3 available
  Sandbox openclaw-sbs-xxx is Pending: 0/3 nodes are available: insufficient cpu
```

## Impact

- **setimage.go**: Removed ~100 lines of SUO code, added ~80 lines for status subcommand and diagnosis.
- **setimage_test.go**: Removed SUO tests, added status and completion tests.
- **options.go**: Added `KubeClient()` method.
- **Help text**: Updated `set image --help` and `set image status --help` with real image examples.

## Future Work

- Implement standalone `okactl create suo` command for updating claimed sandboxes via SandboxUpdateOps.
- Add `--output` flag to `set image status` for JSON/YAML output.
- Add timeout parameter (e.g., `--timeout=5m`) to prevent indefinite waiting in `--wait` mode.
