---
title: "okactl create suo: Batch Update Claimed Sandboxes"
authors:
  - "@mahe"
creation-date: 2026-06-16
status: implemented
---

# okactl create suo: Batch Update Claimed Sandboxes

## Background

After implementing `set image sandboxset` for updating pool sandboxes and `set image status` for monitoring progress, users still need a way to update **claimed** sandboxes (those already allocated to users, not controlled by SandboxSet).

The SandboxUpdateOps (SUO) resource provides this capability, but creating SUO manually via `kubectl` requires writing YAML and understanding the CRD schema. This proposal adds a dedicated `okactl create suo` command to simplify this workflow.

## Goals

- Provide a simple command to create SandboxUpdateOps for batch updating claimed sandboxes
- Automatically validate container names and label selectors before creating SUO
- Handle webhook conflicts by automatically cleaning up stale SUOs
- Generate SUO names automatically using `GenerateName` prefix

## Non-Goals

- Support lifecycle hooks (preUpgrade/postUpgrade) - can be added later if needed
- Support maxUnavailable configuration - use default rolling update strategy
- Support manual SUO naming - automatic naming is simpler and avoids conflicts

## Command Format

```bash
okactl create suo -l <selector> CONTAINER=IMAGE [CONTAINER=IMAGE ...]
```

### Examples

```bash
# Update gateway container for all claimed sandboxes with app=openclaw
okactl create suo -l app=openclaw gateway=nginx:1.27

# Update multiple containers
okactl create suo -l app=openclaw gateway=nginx:1.27 sidecar=envoy:1.28

# Update in a specific namespace
okactl -n production create suo -l app=openclaw gateway=nginx:1.27
```

## Implementation Details

### 1. New File: `pkg/cli/create.go`

Contains:
- `NewCreateCommand()` - Top-level create command
- `newCreateSuoCommand()` - SUO subcommand with `-l/--selector` flag
- `createSuoOptions` - Options struct with global options and selector
- `runCreateSuoWithClient()` - Core logic: validate, build patch, create SUO

Helper functions:
- `parseImageArgs()` - Parse container=image arguments (shared with `set image`, defined in `setimage.go`)
- `buildSuoImagePatch()` - Build Strategic Merge Patch JSON
- `validateSuoImageContainers()` - Verify container names exist
- `parseSuoSelectorToMap()` - Parse selector string to map
- `formatSuoImagePairs()` - Format output for display
- `deleteActiveSandboxUpdateOps()` - Clean up stale SUOs
- `waitForSUODeletion()` - Wait for SUO deletion to complete

### 2. Modified: `cmd/okactl/main.go`

Register the create command:
```go
createCmd := cli.NewCreateCommand(globalOpts)
createCmd.GroupID = "resource"
root.AddCommand(scaleCmd, setCmd, restartCmd, createCmd)
```

### 3. Test Coverage: `pkg/cli/create_test.go`

Test cases:
- Normal SUO creation (single container, multiple containers)
- Missing selector error
- No matching sandboxes error
- Container name not found error
- Invalid image argument format error

## Key Design Decisions

### 1. Why use `GenerateName` instead of user-specified names?

**Decision**: Use `GenerateName: "suo-"` to auto-generate unique names.

**Rationale**:
- Users don't need to think of unique names
- After auto-cleanup of old SUOs, new SUOs get fresh names for easy identification
- Follows Kubernetes conventions (similar to Jobs, Pods)

### 2. Why only support image updates?

**Decision**: Only support `CONTAINER=IMAGE` arguments, no lifecycle hooks or maxUnavailable.

**Rationale**:
- Current user requirement is clear: only need to update images
- Lifecycle hooks and maxUnavailable add complexity, can be added later if needed
- Keep the command simple and easy to use

### 3. Why auto-cleanup old SUOs?

**Decision**: Automatically delete Pending/Updating SUOs before creating new ones.

**Rationale**:
- Webhook prevents multiple active SUOs in the same namespace
- Manual `kubectl delete` then create is poor UX
- Auto-cleanup provides seamless experience

**Implementation**:
- List all SUOs in namespace
- Delete those with `phase: Pending` or `phase: Updating`
- Wait up to 30 seconds for deletion to complete (finalizer cleanup)
- Then create new SUO

### 4. Why validate container names?

**Decision**: Check that all specified container names exist in at least one matching sandbox.

**Rationale**:
- Prevents silent failures where SUO is created but patch doesn't match any containers
- Provides immediate feedback to user
- Only validates against first matching sandbox (not all) for performance

## Error Handling

| Error Scenario | Error Message |
|----------------|---------------|
| Missing `-l` flag | `--selector (-l) is required` |
| No matching sandboxes | `no sandboxes found matching selector "app=xxx" in namespace "default"` |
| Container not found | `container "xxx" not found in sandbox "sbx-yyy"` |
| Invalid image format | `invalid container=image argument "xxx", expected format CONTAINER=IMAGE` |
| Failed to create SUO | `failed to create SandboxUpdateOps: <error>` |

## Usage Workflow

### 1. Update Claimed Sandboxes

```bash
$ okactl create suo -l agents.kruise.io/sandbox-claimed=true gateway=nginx:test
sandboxupdateops.agents.kruise.io/suo-abc123 created (selector: agents.kruise.io/sandbox-claimed=true, images: gateway=nginx:test)
```

### 2. Check Update Progress

```bash
$ kubectl get suo
NAME         PHASE      TOTAL   UPDATED   UPDATING   FAILED   AGE
suo-abc123   Updating   2       0         1          0        10s
```

### 3. Monitor Sandbox Status

```bash
$ kubectl get sbx -l agents.kruise.io/update-ops=suo-abc123
NAME                 STATUS      AGE   CLAIMED
openclaw-sbs-vqqlk   Upgrading   4d    true
openclaw-sbs-wqvrp   Running     4d    true
```

### 4. Handle Stale SUOs

If a previous SUO is stuck, creating a new one will auto-cleanup:

```bash
$ okactl create suo -l app=openclaw gateway=nginx:v2
sandboxupdateops.agents.kruise.io/suo-abc123 deleted (was Updating)
sandboxupdateops.agents.kruise.io/suo-def456 created (selector: app=openclaw, images: gateway=nginx:v2)
```

## Verification Steps

1. **Code quality**: `go vet ./pkg/cli/`
2. **Unit tests**: `go test ./pkg/cli/ -v`
3. **Build binary**: `go build -o okactl ./cmd/okactl/`
4. **Check help**: `okactl create suo --help`
5. **Cluster test** (requires controller deployment):
   - `okactl create suo -l agents.kruise.io/sandbox-claimed=true gateway=nginx:test`
   - `kubectl get suo` to confirm creation

## Comparison with Previous Design

### Previous: `set image sandbox`

```bash
okactl set image sandbox -l selector container=image
```

**Problems**:
- Mixed two different update mechanisms (SandboxSet template vs SUO) in one command
- Confusing for users: which one should I use?
- Architecture not clean

### Current: `create suo`

```bash
okactl create suo -l selector container=image
```

**Benefits**:
- Clear separation: `set image` for SandboxSet, `create suo` for claimed sandboxes
- Follows Kubernetes convention: `kubectl create <resource>`
- Easier to understand and document

## Future Enhancements

1. **Add `--name` flag**: Allow users to specify SUO name manually
2. **Add `--lifecycle` flag**: Support preUpgrade/postUpgrade hooks
3. **Add `--max-unavailable` flag**: Configure rolling update strategy
4. **Add `--dry-run` flag**: Preview the SUO without creating it
5. **Add `okactl delete suo`**: Convenient command to delete SUOs

## Impact

- **pkg/cli/create.go**: New file, ~200 lines
- **pkg/cli/create_test.go**: New file, ~150 lines, 9 test cases
- **cmd/okactl/main.go**: Added 3 lines to register create command
- **docs/proposals/20260615-okactl-cli-tool.md**: Updated Summary section

## Implementation History

- 2026-06-16: Initial implementation of `create suo` command
- 2026-06-24: Code review fixes:
  - Selector parsing: replaced `parseSuoSelectorToMap` with `metav1.ParseToLabelSelector`
    to support full label selector syntax (`key in (v1,v2)`, `key!=value`, etc.), ensuring
    consistency with the `labels.Parse` used for sandbox matching
  - Container validation: `validateSuoImageContainers` now checks all matching sandboxes
    instead of only the first; prints warnings for partial mismatches, errors only when
    a container is missing from ALL sandboxes
  - Error handling: `waitForSUODeletion` now uses `apierrors.IsNotFound` instead of
    string matching for K8s not-found errors
