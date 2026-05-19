# Checkpoint Owns SandboxTemplate — Design

- Date: 2026-05-19
- Branch: `refactor/cp-owner-260519`
- Scope: `pkg/sandbox-manager/infra/sandboxcr/`, `api/v1alpha1/checkpoint_types.go`

## 1. Problem

`pkg/sandbox-manager/infra/sandboxcr/clone.go:299` (`CreateCheckpoint`) sets the
new `Checkpoint` CR as a dependent of the freshly-created `SandboxTemplate` via
`OwnerReferences`. As a consequence, when the `Checkpoint` is later deleted by
the external Checkpoint TTL controller (driven by `CheckpointSpec.TtlAfterFinished`),
Kubernetes garbage collection does not reach the `SandboxTemplate`, and the
template leaks indefinitely.

The leak is observable on snapshots created through
`POST /sandboxes/{sandboxID}/snapshots` (handler at
`pkg/servers/e2b/snapshot.go:33-75`).

## 2. Goal & Non-goals

### Goal

Make the deletion of a `Checkpoint` — through any channel, including the external
TTL controller — cascade-delete the matching `SandboxTemplate`, while preserving
strict compatibility with all existing data and API behaviors.

### Non-goals

- Do not modify the external Checkpoint TTL controller (lives outside this repo).
- Do not migrate or clean up legacy orphan `SandboxTemplate` resources that
  already leaked under today's behavior. New code must not regress existing
  resources, but it also will not retroactively fix them.
- Do not introduce a new in-cluster controller, finalizer, or annotation.
- Do not break E2B HTTP API behavior (status codes, response shape, idempotency).

## 3. Constraints (compatibility envelope)

- New code must not write to existing `Checkpoint` / `SandboxTemplate` resources
  on disk; only newly created snapshots adopt the new ownership shape.
- All API surfaces (`POST /sandboxes/{id}/snapshots`, `POST /sandboxes` clone
  path, `GET /v2/sandboxes`, `GET /snapshots`, `DELETE /templates/{id}`) must
  remain functionally identical to callers.
- Both the legacy ownership shape and the new ownership shape must remain
  fully serviceable (List, Clone, Delete) by both old and new sandbox-manager
  binaries during rolling upgrade and rollback windows.

## 4. Approach

### 4.1 Reverse the ownership direction for new snapshots

`CreateCheckpoint` will be reordered to:

1. Create the `Checkpoint` first using `GenerateName: sbx.Name + "-"`. The
   `Checkpoint` carries no `OwnerReferences`. Its `Spec.PersistentContents`
   fallback source switches from `tmpl.Spec.PersistentContents` to
   `sbx.Spec.PersistentContents` (the data is equivalent — `SandboxTemplate`
   itself copies from the same `Sandbox`).
2. Create the `SandboxTemplate` with `Name: cp.Name` and a single
   `OwnerReference` pointing back at the `Checkpoint`:
   ```go
   {
       APIVersion:         v1alpha1.CheckpointControllerKind.GroupVersion().String(),
       Kind:               v1alpha1.CheckpointControllerKind.Kind,
       Name:               cp.Name,
       UID:                cp.UID,
       Controller:         ptr.To(true),
       BlockOwnerDeletion: ptr.To(true),
   }
   ```
3. Wait for `Checkpoint` to reach `Succeeded` via the existing
   `cache.NewCheckpointTask(...).Wait(...)` and refresh logic.

This order improves failure semantics over today's code:

| Step that fails | New approach (Checkpoint then Template) | Today (Template then Checkpoint) |
|---|---|---|
| Step 1 | Nothing created. Clean error. | Same. |
| Step 2 | Orphan `Checkpoint` only — drained naturally by TTL. | Orphan `SandboxTemplate` — leaks until manually cleaned. |
| Step 3 (wait) | Both exist; `Checkpoint` owns `SandboxTemplate`; external TTL on `Checkpoint` cascades both. | Both exist; external TTL deletes `Checkpoint` but `SandboxTemplate` persists as the orphan owner — today's headline bug. |

### 4.2 Flip the deletion path to align

`pkg/sandbox-manager/infra/sandboxcr/infra.go:216` `DeleteCheckpoint` will:

1. Keep Step 1 (`findCheckpointAndTemplateById`) and Step 2 (`AnnotationOwner`
   verification) unchanged.
2. Step 3: delete the `Checkpoint` first via `DefaultDeleteCheckpointCR`. For
   new-shape data, GC cascades the `SandboxTemplate` after the
   `agents.kruise.io/checkpoint` finalizer is processed.
3. Step 4: invert the legacy guard from `IsControlledBy(cp, tmpl)` to
   `IsControlledBy(tmpl, cp)`. When the `SandboxTemplate` is *not* controlled
   by the `Checkpoint` (legacy shape), explicitly delete the
   `SandboxTemplate` via `DefaultDeleteSandboxTemplate`.

| Data shape | Step 3 outcome | `IsControlledBy(tmpl, cp)` | Step 4 |
|---|---|---|---|
| New (Checkpoint owns Template) | Checkpoint enters Terminating; GC will sweep Template after finalizer | `true` | skip |
| Legacy (Template owns Checkpoint) | Checkpoint deletion is independent | `false` | explicitly delete Template |

Both shapes converge to "Checkpoint and Template are both removed".

### 4.3 New constant `CheckpointControllerKind`

`api/v1alpha1/checkpoint_types.go` gains:

```go
var CheckpointControllerKind = GroupVersion.WithKind("Checkpoint")
```

This mirrors `SandboxSetControllerKind` (`api/v1alpha1/sandboxset_types.go:54`)
and `SandboxTemplateControllerKind`
(`api/v1alpha1/sandboxtemplate_types.go:74`), avoiding string literals at
`OwnerReference` construction sites.

## 5. Compatibility Matrix

### 5.1 Data shapes coexisting at upgrade time

| Shape | OwnerReferences | Origin |
|---|---|---|
| A. Legacy Checkpoint+Template | Checkpoint owned by Template; Template has no owner | Pre-upgrade snapshots |
| B. New Checkpoint+Template | Template owned by Checkpoint; Checkpoint has no owner | Post-upgrade snapshots created by this design |
| C. Orphan Template (legacy leak) | Template has no owner; matching Checkpoint already TTL'd | Pre-upgrade external-TTL leak (out of scope) |
| D. SandboxSet-backed Template | Template owned by SandboxSet | Unrelated to this design |

### 5.2 API surfaces × data shapes

| API path | A | B | C | D |
|---|---|---|---|---|
| `POST /sandboxes/{id}/snapshots` (CreateCheckpoint) | never touches A | new flow | never touches | never touches |
| `POST /sandboxes` clone via Checkpoint | name-based pairing, works | works | NotFound (today's behavior) | uses SandboxSet claim path |
| `GET /v2/sandboxes`, `GET /snapshots` | reads only Checkpoint Phase | same | Checkpoint absent, naturally hidden | not in ListSnapshots scope |
| `DELETE /templates/{id}` → `DeleteCheckpoint` | Step 4 explicitly deletes Template | GC cascade-deletes Template | Step 1 returns NotFound, handler returns 204 | rejected by `HasTemplate` guard with 401 |
| `GET /templates`, `GET /templates/{id}` | not in ListTemplates scope | not in scope | not in scope | works |
| External Checkpoint TTL controller | deletes Checkpoint, leaks Template (today's behavior preserved) | deletes Checkpoint, GC cascades Template | N/A | N/A |

### 5.3 Rolling upgrade and rollback

| Operator → Data | Behavior | Outcome |
|---|---|---|
| Old replica deletes B | Old code: deletes Template (the dependent) → `IsControlledBy(cp, tmpl)` = false → explicitly deletes Checkpoint | both removed |
| New replica deletes A | New code: deletes Checkpoint (the dependent) → `IsControlledBy(tmpl, cp)` = false → explicitly deletes Template | both removed |
| Old replica creates snapshot | Produces shape A; new replicas can List/Clone/Delete | compatible |
| New replica creates snapshot | Produces shape B; old replicas can List/Clone/Delete | compatible |

Rollback to the old image after the new one ran for some time is safe: shape B
data continues to be servicable by the old code path; TTL behavior is owned by
the external controller and is not version-coupled.

## 6. File-level change list

### 6.1 `api/v1alpha1/checkpoint_types.go`

Add `var CheckpointControllerKind = GroupVersion.WithKind("Checkpoint")` near
the existing constant block. No CRD schema impact.

### 6.2 `pkg/sandbox-manager/infra/sandboxcr/clone.go` (`CreateCheckpoint`)

- Reorder: build and create `Checkpoint` first, then `SandboxTemplate`.
- `Checkpoint.ObjectMeta` uses `GenerateName: sbx.Name + "-"`, no
  `OwnerReferences`.
- `Checkpoint.Spec.PersistentContents` fallback source: `sbx.Spec.PersistentContents`
  filtered by `CheckpointPersistentContent{Memory,Filesystem}`.
- `SandboxTemplate.ObjectMeta.Name` = `cp.Name`; `OwnerReferences` set to a
  single controller ref pointing at the `Checkpoint`.
- Continue to call `checkpointUtils.PropagateAnnotationsToCheckpoint(sbx, cp)`
  before creating the `Checkpoint`.
- Preserve `DefaultCreateSandboxTemplate` and `DefaultCreateCheckpoint` package
  variables (test seams; required by directory `AGENTS.md`).
- Preserve `cache.NewCheckpointTask(ctx, cp).Wait(opts.WaitSuccessTimeout)` and
  the post-wait `Get` to refresh `Status.CheckpointId`.

### 6.3 `pkg/sandbox-manager/infra/sandboxcr/infra.go` (`DeleteCheckpoint`)

- Step 3: replace `DefaultDeleteSandboxTemplate(...)` with
  `DefaultDeleteCheckpointCR(ctx, i.Cache.GetClient(), cp.Namespace, cp.Name)`.
- Step 4: change guard from `if !metav1.IsControlledBy(cp, tmpl)` to
  `if !metav1.IsControlledBy(tmpl, cp)`; in the body, call
  `DefaultDeleteSandboxTemplate(...)`.
- Update inline comments to describe both ownership directions.

### 6.4 `pkg/sandbox-manager/infra/sandboxcr/clone_test.go`

- `TestCreateCheckPoint` (line 955): switch the fake-client name/UID priming
  hook from `DefaultCreateSandboxTemplate` to `DefaultCreateCheckpoint` (the
  first object created is now the `Checkpoint`).
- For each successful case (lines 1022, 1049, 1085): drop assertions that
  `cp.OwnerReferences[0].Kind == "SandboxTemplate"`; assert
  `cp.OwnerReferences` is empty; load the `SandboxTemplate` and assert its
  `OwnerReferences[0]` has `Kind == "Checkpoint"`, `Name == cp.Name`,
  `UID == cp.UID`.
- Add a new case "template create fails after checkpoint create": inject error
  on `DefaultCreateSandboxTemplate`, assert `CreateCheckpoint` returns an
  error and the `Checkpoint` exists in the fake store while `SandboxTemplate`
  does not.

### 6.5 `pkg/sandbox-manager/infra/sandboxcr/infra_test.go`

- `TestInfra_DeleteCheckpointWithOptions_NamespaceScoped` (line 301): existing
  fixture is shape A (no `OwnerReferences`), still valid; assertions remain.
- Add a "new-data-shape delete" mirror case where `SandboxTemplate.OwnerReferences`
  points at the `Checkpoint`. Stub or count `DefaultDeleteSandboxTemplate`
  invocations to verify Step 4 is skipped (the fake client does not simulate
  GC cascade by default; verifying skip is the contract under test).
- `TestInfra_DeleteCheckpoint_OwnerVerification` (line 335): orthogonal,
  unchanged. Optionally add a new-shape variant for parity.

### 6.6 Files explicitly not modified

| File | Reason |
|---|---|
| `pkg/servers/e2b/snapshot.go` | Calls `CreateCheckpoint`, signature/return unchanged |
| `pkg/servers/e2b/templates.go` | Calls `DeleteCheckpoint`, HTTP status mapping unchanged |
| `pkg/servers/e2b/list.go` | Reads only Checkpoint state, owner-direction agnostic |
| `pkg/cache/...` | Phase-based logic, owner-direction agnostic |
| External Checkpoint controller | Out of repo |

## 7. Risks & mitigations

| Risk | Trigger | Mitigation |
|---|---|---|
| Orphan Checkpoint after Template create fails | Process restart / API throttling / network blip between Step 1 and Step 2 | Accepted. Drained naturally by external TTL. Strictly better than today's orphan-Template failure mode (which never self-heals). |
| Wrong field in `OwnerReference` (Kind / APIVersion / UID / Name) | Code typo | Centralize via `CheckpointControllerKind`; assert `Kind == "Checkpoint"` and UID/Name pairing in unit tests. |
| Mixed-version replicas during rolling upgrade write inconsistent data | Canary deploy window | Verified in §5.3 — all four cross-operations close cleanly. |
| External Checkpoint controller starts work before Template exists | Fast watch | Verified in §4.1 — the controller reads only `Checkpoint` + `Sandbox`/`Pod`, never the sibling `SandboxTemplate`. |
| `BlockOwnerDeletion: true` blocks under foregroundDeletion | Caller misuse of foreground propagation | All in-repo deletes use background propagation; field setting kept identical to today's. |
| Test fixture hook relocation breaks unrelated tests | Shared test seam | Verified by `grep -n "DefaultCreateSandboxTemplate" pkg/sandbox-manager/infra/sandboxcr/` during review. |

## 8. Rollback

Revert the sandbox-manager image to the pre-change tag. No CR migration is
required: shape-B resources written by the new image remain serviceable by the
old image (clone, list, delete). TTL behavior is governed by the external
controller and Kubernetes GC, not by the sandbox-manager version. Shape-B
`SandboxTemplate.OwnerReferences` persists after rollback; this is harmless
and re-engages on the next forward deploy.

## 9. Verification

### 9.1 Pre-merge

- [ ] `make generate manifests` (sanity; no schema change expected)
- [ ] `go vet ./...`, `go build ./...` succeed
- [ ] `golangci-lint run` passes; cyclomatic complexity of `CreateCheckpoint`
      does not exceed 32
- [ ] `go test ./pkg/sandbox-manager/infra/sandboxcr/...` green, including the
      new cases listed in §6.4 and §6.5
- [ ] `go test ./pkg/servers/e2b/...` green (regression check, no source change)
- [ ] PR diff inspected for any string-literal `"Checkpoint"` /
      `"SandboxTemplate"` in `OwnerReference` blocks
- [ ] `grep -n "IsControlledBy" pkg/sandbox-manager/infra/sandboxcr/` shows
      exactly one occurrence with argument order `(tmpl, cp)`

### 9.2 Post-deploy

- [ ] Create a new snapshot via `POST /sandboxes/{id}/snapshots`.
      `kubectl get checkpoint <name> -o yaml` shows empty `ownerReferences`;
      `kubectl get sbt <name> -o yaml` shows
      `ownerReferences[0].kind == Checkpoint`.
- [ ] Clone from the new snapshot via `POST /sandboxes`; the resulting sandbox
      reaches Ready.
- [ ] `DELETE /templates/{checkpointID}` removes both objects within the
      finalizer + GC sweep window.
- [ ] Repeat clone / delete against a known pre-upgrade legacy snapshot;
      behavior unchanged.
- [ ] Allow a fresh snapshot to expire via the external TTL controller and
      confirm the `SandboxTemplate` is cascade-deleted (the headline goal of
      this design).

## 10. Metrics

No new metrics. Existing counters / histograms (`snapshotTotal`,
`snapshotDuration` at `pkg/servers/e2b/snapshot.go:62,67`) continue to cover
the create path with unchanged label dimensions.