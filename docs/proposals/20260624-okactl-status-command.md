---
title: "okactl: Status Command, Short Name Aliases, and Help Display Optimization"
authors:
  - "@mahe"
creation-date: 2026-06-24
status: implemented
---

# okactl: Status Command, Short Name Aliases, and Help Display Optimization

## Background

After implementing the `okactl` CLI tool, users faced several usability issues:

1. **Unable to confirm update success**: After running `set image`, the CLI only returns "image updated", but users have no way to know whether the image update actually took effect or whether Pods were successfully rebuilt.
2. **Difficult error diagnosis**: When image pulls fail (e.g., ImagePullBackOff) or resources are insufficient, users need to manually run multiple kubectl commands to troubleshoot.
3. **Confusing status display**: The previous display showed `0/2 available` where the denominator was the actual available count rather than the desired replica count, causing the numbers to fluctuate during rolling updates and making progress hard to understand.
4. **Missing resource short names**: Commands like `okactl scale sandboxset` require typing the full resource name, unlike `kubectl` which supports short names (e.g., `sbs`, `sbx`).
5. **Short names not visible in help**: Even when aliases are configured via cobra, the default help output does not display them in the "Available Commands" list.
6. **Status command placement**: The original `set image status` subcommand was nested too deeply; a top-level `status` command is more intuitive and extensible.

## Goals

- Provide a top-level `okactl status` command group for one-click visibility into resource update progress, supporting both SandboxSet (`sbs`) and SandboxUpdateOps (`suo`).
- When an update is stalled, automatically diagnose the root cause (image pull failure, insufficient resources, etc.) without requiring manual troubleshooting.
- Unify the status display format, using the desired replica count as the denominator for clear progress tracking.
- Add cobra `Aliases` to all resource-type subcommands so users can use short names (`sbs`, `suo`, etc.).
- Customize the cobra usage template so that subcommand aliases are visible in `okactl --help` and all parent command help outputs.

## Proposal

### 1. Architecture: Top-level `status` Command

**Problem**: Previously status was nested under `set image status`, making it hard to discover and not extensible to other resources (e.g., SandboxUpdateOps).

**Solution**:
- Created a top-level `okactl status` command group.
- `okactl status sbs NAME` shows SandboxSet rolling update progress.
- `okactl status suo NAME` shows SandboxUpdateOps batch update progress.
- The `--wait` flag is exclusive to the `set image` command (not `status`), reflecting the "operation-then-wait" workflow.

### 2. Short Name Aliases via Cobra `Aliases`

**Problem**: Users have to type full resource names like `sandboxset`, `sandboxupdateops`, unlike `kubectl` which supports short forms.

**Solution**:
- Add `Aliases` field to all resource-type cobra subcommands.
- `scale sandboxset` → also accessible as `scale sbs`
- `status sbs` → also accessible as `status sandboxset`
- `status suo` → also accessible as `status sandboxupdateops`
- `set image` already supports `sbs` as a resource argument via switch statement.

### 3. Custom Usage Template for Alias Display

**Problem**: Cobra's default usage template does not display subcommand aliases in the "Available Commands" list. For example, `okactl scale --help` shows `sandboxset` but not `sbs`.

**Solution**:
- Custom usage template registered via `root.SetUsageTemplate()`.
- Added `join` template function via `cobra.AddTemplateFunc("join", strings.Join)`.
- Aliases shown as `(sbs)` after the command name in all Available Commands lists.

**Example output**:
```
$ okactl scale --help
Available Commands:
  sandboxset  (sbs) Scale a SandboxSet to the specified number of replicas

$ okactl status --help
Available Commands:
  sbs         (sandboxset) Show the update progress of a SandboxSet
  suo         (sandboxupdateops) Show the update progress of a SandboxUpdateOps
```

### 4. `status sbs`: SandboxSet Status with Auto-Diagnosis

**Command format**:
```bash
okactl status sbs NAME
```

**Features**:
- Reads the SandboxSet's `status.updatedReplicas` and `status.availableReplicas` fields.
- Display format: `my-pool  Updating  1/3 updated  1/3 available`
- Shows `Complete` when `updatedReplicas == spec.replicas && availableReplicas == spec.replicas`.
- Performs a one-shot status check with auto-diagnosis when the update appears stalled.

### 5. Auto-Diagnosis Mechanism

**Trigger conditions**:
- When the user runs `okactl status sbs`, if the update is not progressing, diagnosis runs immediately.
- In `set image --wait` mode, if progress does not change for 3 consecutive polls (~10 seconds), diagnosis is triggered.

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

**Deduplication**: Uses a `map[string]bool` to track reported sandboxes, preventing repeated output in `set image --wait` mode.

### 6. Status Display Optimization

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

### 7. `status suo`: SandboxUpdateOps Status Monitoring

**Command format**:
```bash
okactl status suo NAME
```

**Features**:
- Reads the SandboxUpdateOps' `status.phase`, `status.replicas`, `status.updatedReplicas`, `status.updatingReplicas`, and `status.failedReplicas` fields.
- Display format: `suo-zk7h7  Updating   0/2 updated  1 updating  0 failed`
- Shows `Completed` when `status.phase == Completed` or all replicas are updated and none are updating.
- Performs a one-shot status check; use `set image --wait` for polling.
- Returns an error if the SUO enters the `Failed` phase.

**Output example**:
```
$ okactl status suo suo-zk7h7
suo-zk7h7                      Updating   0/2 updated  1 updating  0 failed
```

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

### 4. Why a top-level `status` command instead of nested subcommands?

- `okactl set image status` is deeply nested and hard to discover.
- A top-level `okactl status` command is more consistent with `kubectl` conventions.
- Easily extensible: future resources (e.g., Checkpoint) can be added as new subcommands.
- Both `sbs` and `suo` share the same parent command, providing a unified status view.

### 5. Why customize the cobra usage template?

- Cobra's default template only shows command names, not aliases.
- Users need to see short names in help to discover them.
- Custom template adds `{{if .Aliases}} ({{join .Aliases ", "}}){{end}}` to each Available Commands entry.
- `join` function registered via `cobra.AddTemplateFunc` since cobra's built-in template funcs don't include `strings.Join`.


## Implementation Details

### Files Changed

| File | Change |
|------|--------|
| `pkg/cli/status.go` | **New file**. Implements `NewStatusCommand`, `newStatusSandboxSetCommand`, `newStatusSandboxUpdateOpsCommand`, `runSetImageStatusWithClient`, `runSuoStatusWithClient`, `waitForSandboxSetUpdate`, `isSuoComplete`, `printSuoStatus`, `printSandboxSetStatus`, `diagnoseSandboxSetUpdate`. |
| `pkg/cli/scale.go` | Added `Aliases: []string{"sbs"}` to `sandboxset` subcommand. Updated examples. |
| `pkg/cli/setimage.go` | Removed `newSetImageStatusCommand` function and its registration. Updated examples to reference `okactl status sbs` instead of `okactl set image status`. |
| `cmd/okactl/main.go` | Added `strings` import. Registered custom `join` template function. Set custom usage template. Registered `statusCmd` under Resource Commands group. |
| `pkg/cli/commands_test.go` | Replaced `TestNewSetImageStatusCommand` with `TestNewStatusCommand` covering both `sbs` and `suo` subcommands. Added `sbs` alias test for scale command. |
| `pkg/cli/setimage_test.go` | Added `TestIsSuoComplete`, `TestPrintSuoStatus`, `TestRunSuoStatusWithClient`, `TestWaitForSandboxSetUpdate`. |

### Core Functions

**status.go (new)**:
- `NewStatusCommand`: Top-level `status` command group.
- `newStatusSandboxSetCommand`: `status sbs` subcommand (alias: `sandboxset`).
- `newStatusSandboxUpdateOpsCommand`: `status suo` subcommand (alias: `sandboxupdateops`).
- `runSuoStatusWithClient`: SUO status query entry point.
- `isSuoComplete`: Checks if SUO is fully updated.
- `printSuoStatus`: One-line SUO status display.
- `runSetImageStatusWithClient`: SandboxSet status query entry point (moved from setimage.go).
- `waitForSandboxSetUpdate`: SandboxSet polling with auto-diagnosis (moved from setimage.go).
- `printSandboxSetStatus`: SandboxSet status display (moved from setimage.go).
- `diagnoseSandboxSetUpdate`: Three-layer diagnosis logic (moved from setimage.go).

**main.go**:
- `usageTemplate`: Custom cobra usage template with alias display.
- `cobra.AddTemplateFunc("join", strings.Join)`: Registers `join` for template rendering.

### Test Coverage

- `TestNewStatusCommand`: Verifies `sbs`/`sandboxset` and `suo`/`sandboxupdateops` subcommands and confirms `--wait` is NOT a status flag.
- `TestSetImageStatus`: Covers sandboxset not found, update complete, and updating in progress.
- `TestWaitForSandboxSetUpdate`: Tests `set image --wait` mode with immediate completion.
- `TestIsSandboxSetUpdateComplete`: Covers complete, in-progress, updated-but-unavailable, and zero-replicas.
- `TestIsSuoComplete`: Covers Completed phase, all-updated, in-progress, and Pending phase.
- `TestPrintSuoStatus`: Covers updating and completed phases.
- `TestRunSuoStatusWithClient`: Covers SUO not found, updating, and completed states.
- `TestDiagnoseSandboxSetUpdate`: Covers skip-diagnosis, Pending with message, Failed, scheduling failure, ImagePullBackOff, no pod, and Running skip.

## Usage Examples

### Normal Update Flow

```bash
$ okactl set image sbs openclaw-sbs gateway=mirrors-ssl.aliyuncs.com/ghcr.io/openclaw/openclaw:2026.4.24
sandboxset.agents.kruise.io/openclaw-sbs image updated (gateway)

$ okactl status sbs openclaw-sbs
openclaw-sbs                       Updating   0/3 updated  0/3 available

$ okactl set image sbs openclaw-sbs gateway=nginx:1.27 --wait
sandboxset.agents.kruise.io/openclaw-sbs image updated (gateway)
openclaw-sbs                       Updating   0/3 updated  0/3 available
openclaw-sbs                       Updating   1/3 updated  1/3 available
openclaw-sbs                       Updating   2/3 updated  2/3 available
openclaw-sbs                       Updating   3/3 updated  3/3 available
Update completed (3/3 updated, 3/3 available)
```

### Image Pull Failure

```bash
$ okactl set image sbs openclaw-sbs gateway=nginx:invalid-tag
sandboxset.agents.kruise.io/openclaw-sbs image updated (gateway)

$ okactl status sbs openclaw-sbs
openclaw-sbs                       Updating   0/3 updated  0/3 available
  Sandbox openclaw-sbs-xxx is Pending: gateway: ImagePullBackOff
  Sandbox openclaw-sbs-yyy is Pending: gateway: ImagePullBackOff
```

### Insufficient Resources

```bash
$ okactl status sbs openclaw-sbs
openclaw-sbs                       Updating   0/3 updated  0/3 available
  Sandbox openclaw-sbs-xxx is Pending: 0/3 nodes are available: insufficient cpu
```

### Scale with Short Name

```bash
$ okactl scale sbs my-pool --replicas=5
sandboxset.agents.kruise.io/my-pool scaled to 5
```

### SandboxUpdateOps Status

```bash
$ okactl create suo -l agents.kruise.io/sandbox-claimed=true gateway=nginx:2.0
sandboxupdateops.agents.kruise.io/suo-zk7h7 created (selector: agents.kruise.io/sandbox-claimed=true, images: gateway=nginx:2.0)

$ okactl status suo suo-zk7h7
suo-zk7h7                          Updating   0/2 updated  1 updating  0 failed
```

### Help Output with Aliases

```bash
$ okactl --help
Resource Commands:
  create      Create a resource
  restart     Restart containers in a Sandbox
  scale       Scale a resource to a desired replica count
  set         Update specific fields on a resource
  status      Show the status of a resource

$ okactl scale --help
Available Commands:
  sandboxset  (sbs) Scale a SandboxSet to the specified number of replicas

$ okactl status --help
Available Commands:
  sbs         (sandboxset) Show the update progress of a SandboxSet
  suo         (sandboxupdateops) Show the update progress of a SandboxUpdateOps
```

## Impact

- **pkg/cli/status.go**: New file, ~165 lines. Implements `status sbs` and `status suo` commands.
- **pkg/cli/scale.go**: Added `Aliases` to sandboxset subcommand.
- **pkg/cli/setimage.go**: Removed `newSetImageStatusCommand` (~30 lines). Updated examples.
- **cmd/okactl/main.go**: Added custom usage template, `join` template function, `statusCmd` registration.
- **pkg/cli/commands_test.go**: Added tests for `status` and scale alias commands.
- **pkg/cli/setimage_test.go**: Added SUO status tests (~180 lines).
- **set image --help**: Examples now reference `okactl status sbs` instead of `okactl set image status`.

## Future Work

- Add `--output` flag to status commands for JSON/YAML output.
- Add `okactl status sbx` for individual Sandbox status.
- Add `okactl status cp` for Checkpoint operation status.
