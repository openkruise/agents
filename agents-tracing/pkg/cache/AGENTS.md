# pkg/cache — AI Agent Guide

## Overview

This package provides a read-through, informer-backed local cache for the sandbox-manager component. It wraps
controller-runtime's cache layer to deliver low-latency lookups of CRD objects (Sandbox, SandboxSet, Checkpoint,
SandboxTemplate) without hitting the API server on every request.

## ⚠️ CRITICAL: Object Pointer Safety

**All objects returned from cache Get/List methods are raw pointers into the informer store
(`DefaultUnsafeDisableDeepCopy: true`). You MUST call `.DeepCopy()` before any mutation.**

```go
// ✅ CORRECT — DeepCopy before modification
sbx, err := cacheProvider.GetClaimedSandbox(ctx, cache.GetClaimedSandboxOptions{SandboxID: sandboxID})
if err != nil { return err }
sbxCopy := sbx.DeepCopy()
sbxCopy.Labels["foo"] = "bar"
err = client.Update(ctx, sbxCopy)

// ❌ WRONG — Directly mutating cache object corrupts shared state
sbx, err := cacheProvider.GetClaimedSandbox(ctx, cache.GetClaimedSandboxOptions{SandboxID: sandboxID})
sbx.Labels["foo"] = "bar"  // DATA RACE! Corrupts informer store!
```

This applies to ALL methods that return `*agentsv1alpha1.Sandbox`, `*agentsv1alpha1.Checkpoint`,
`*agentsv1alpha1.SandboxSet`, or slices thereof (`ListSandboxes`, `ListSandboxesInPool`, etc.).

## Architecture

```
pkg/cache/
├── interface.go        — Provider interface (public API contract)
├── cache.go            — Cache struct implementation (controller-runtime manager based)
├── index.go            — Field index definitions (GetIndexFuncs, AddIndexesToCache)
├── controllers/        — Internal reconcilers that power wait-hooks and custom event handlers
│   ├── cache_controllers.go          — Generic CustomReconciler[T] and WaitReconciler[T]
│   ├── cache_controller_sandbox_wait.go     — Sandbox wait reconciler
│   ├── cache_controller_checkpoint_wait.go  — Checkpoint wait reconciler
│   ├── cache_controller_sandbox_custom.go   — Sandbox custom reconciler (external handler registration)
│   ├── cache_controller_sandboxset_custom.go — SandboxSet custom reconciler
│   └── test_helpers.go               — MockManager and test infrastructure
├── cachetest/          — Test helper: NewTestCache (fake client + mock manager)
└── utils/              — WaitEntry, WaitAction, CheckFunc, singleflight helpers
```

## Key Concepts

### 1. Provider Interface (`interface.go`)

The `Provider` interface is the public API. Consumers should depend on `Provider`, not `*Cache` directly.

### 2. Namespace Semantics

For every cache `*` method that accepts `Namespace`, an empty `opts.Namespace` means
"do not add an explicit namespace filter". It does **not** mean "cluster scope" by definition.

The effective scope is "all namespaces visible to the current cache/client". In production this is
often already narrower than the whole cluster, because the cache itself may be pre-filtered by
`SandboxManagerOptions.SandboxNamespace` in `BuildCacheConfig`.

Callers that require a specific namespace must set `opts.Namespace` explicitly. Do not rely on an
empty namespace to mean "search the whole cluster".

### 3. Field Indexes (`index.go`)

All indexes are defined in `GetIndexFuncs()` — the single source of truth shared by production (`AddIndexesToCache`)
and testing (`cachetest.NewTestCache`). Available indexes:

| Index Name            | Resource    | Purpose                                  |
|-----------------------|-------------|------------------------------------------|
| `sandboxPool`         | Sandbox     | Find available/creating sandboxes by template |
| `sandboxID`           | Sandbox     | Lookup claimed sandbox by logical ID     |
| `user`                | Sandbox/CP  | List resources by owner annotation       |
| `templateID`          | SandboxSet  | Lookup SandboxSet by name                |
| `checkpointID`        | Checkpoint  | Lookup checkpoint by status.checkpointId |

### 4. Wait Mechanism (WaitTask factories)

Informer-driven wait that blocks until a resource satisfies a predefined condition. The public API is a
family of factory methods on `*Cache`: `NewSandboxPauseTask` / `NewSandboxResumeTask` /
`NewSandboxWaitReadyTask` / `NewCheckpointTask`. Each factory binds an immutable
`(Action, UpdateFunc, CheckFunc)` tuple so concurrent callers sharing the same `(type, namespace, name, action)`
are guaranteed to use the same checker (see `pkg/cache/tasks.go`). Internally each task calls
`WaitForObjectSatisfied`, which consults `waitHooks` (a `sync.Map`) on every reconcile event via
`WaitReconciler[T]`.

### 5. Singleflight Deduplication

`GetClaimedSandbox`, `GetCheckpoint`, `PickSandboxSet`, and `ListSandboxesInPool` use `singleflight.Group`
to deduplicate concurrent identical queries.

### 6. Custom Reconcilers

`CacheSandboxCustomReconciler` and `CacheSandboxSetCustomReconciler` allow external callers (e.g., sandbox-manager
infra layer) to register event handlers via `AddReconcileHandlers()`.

## Testing

Use `cachetest.NewTestCache(t, initObjs...)` to create a `*Cache` with a fake client and mock manager.
The mock manager supports wait simulation for the `NewXxxTask` family (Pause / Resume / WaitReady / Checkpoint).
For ad-hoc `(action, checker)` combinations that do not correspond to a production factory - typically
when exercising the low-level waitHooks semantics - use `pkg/cache/cachetest.NewAdHocTask`. That
package is banned in production code; import it **only** from `_test.go` files.

## Common Patterns

### Reading from cache (read-only)
```go
    sbx, err := cacheProvider.GetClaimedSandbox(ctx, cache.GetClaimedSandboxOptions{SandboxID: id})
// Use sbx for read-only inspection — no DeepCopy needed if not mutating
```

### Reading from cache then updating
```go
sbx, err := cacheProvider.GetClaimedSandbox(ctx, cache.GetClaimedSandboxOptions{SandboxID: id})
if err != nil { return err }
sbxCopy := sbx.DeepCopy()  // MUST DeepCopy before mutation
sbxCopy.Spec.DesiredState = "Paused"
return client.Update(ctx, sbxCopy)
```

### Waiting for state transition
```go
task, err := cacheProvider.NewSandboxResumeTask(ctx, sbx)
if err != nil { return err }
defer task.Release()
err = task.Wait(30 * time.Second)
```

For the four production transitions the API is fixed: Pause, Resume, WaitReady, Checkpoint.
Use the corresponding `NewXxxTask` factory. The checker is hard-wired - callers cannot pair the same
`(action, object)` with a different predicate.
Pause and Resume tasks pre-acquire their wait hook, so release them if execution returns before `Wait`.
`Wait` also releases the hook when it returns, and `Release` is idempotent. WaitReady and Checkpoint tasks remain
one-value lazy wait tasks.

## Dependencies

- `controller-runtime` (manager, cache, client, reconciler)
- `golang.org/x/sync/singleflight`
- `pkg/sandbox-manager/config` (SandboxManagerOptions for cache filtering)
- `pkg/utils/sandboxutils` (state helpers, sandbox ID extraction)
- `pkg/sandbox-manager/consts` (log levels)
