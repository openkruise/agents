# Wake-on-Traffic via Direct Spec Patch (No SystemToken)

## Motivation

PR #495 implements wake-on-traffic by having sandbox-gateway call sandbox-manager's
`/connect` HTTP API using a scoped system credential (systemtoken). This introduces
cross-component HTTP coupling, a new credential type, and credential lifecycle
management. This proposal replaces that mechanism with a direct Go function call,
eliminating the systemtoken entirely while reusing the exact same resume logic.

## Design

The sandbox-gateway directly calls `sandboxcr.AsSandbox(sbx, cache).Resume(ctx, opts)`
— a Go function call, not an HTTP call. This reuses the entire sandbox-manager connect
path: spec patch (`Spec.Paused = false`), wait-for-running (`NewSandboxResumeTask().Wait()`),
concurrent dedup (first-writer-wins via `retryUpdate`), conflict retries, and post-resume
refresh (`InplaceRefresh`).

```
Traffic -> Envoy Filter -> Registry lookup
  |-> sandbox Running: forward (current behavior)
  |-> sandbox Paused + WakeOnTraffic enabled:
       |-> sandboxcr.AsSandbox(sbx, cacheProvider).Resume(ctx, opts)
       |     ^-- reuses existing sandbox-manager connect implementation:
       |         1. refreshFromAPIReader (fresh fetch)
       |         2. IsSandboxResumable check
       |         3. NewSandboxResumeTask (pre-acquired wait task)
       |         4. retryUpdate: patches Spec.Paused=false + setTimeout
       |         5. resumeTask.Wait() -- blocks until Ready condition True
       |         6. InplaceRefresh + expectations
       |     Concurrent dedup: first-writer-wins via retryUpdate
       |-> syncRoute: update local registry + sync to peer gateways
       |-> Forward request or return 503 on timeout/failure
  |-> sandbox Paused + WakeOnTraffic disabled: return 502 (current behavior)
```

## Key Components

### Annotation Lifecycle

- `AnnotationWakeOnTraffic` ("agents.kruise.io/wake-on-traffic"): Set to "true" at
  creation time when `autoResume=true` is passed in the E2B create request. Updated
  via connect/resume API when `autoResume` is provided.
- `AnnotationWakeTimeoutSeconds` ("agents.kruise.io/wake-timeout-seconds"): Stores the
  auto-pause timeout (in seconds) to apply when the sandbox is woken by traffic. The
  gateway reads this to set `ResumeOptions.Timeout.PauseTime`, re-arming auto-pause
  after wake.

### Wake Package (`pkg/sandbox-gateway/wake/`)

The `Waker` struct wraps `sandboxcr.AsSandbox(sbx, cache).Resume()` and syncs the route
after Resume succeeds. It does NOT reimplement spec patching or wait-for-running — it
delegates entirely to the existing `Resume()` method.

After Resume succeeds, `syncRoute` mirrors the manager's `syncRoute` flow:
1. Get route from refreshed sandbox (`sandbox.GetRoute()`)
2. Update local gateway registry (`registry.GetRegistry().Update`)
3. Sync route to peer gateways (`proxy.SyncRouteWithPeers`)

### Cache Provider

`cache.NewCache(mgr)` creates the same informer-backed cache + `WaitReconciler` used by
sandbox-manager. The gateway's controller-runtime manager hosts both the existing
`SandboxReconciler` (for local registry updates) and the cache provider's wait
reconciler (for `NewSandboxResumeTask().Wait()`).

### Route Sync

`proxy.SyncRouteWithPeers` was extracted as a package-level function so the gateway can
call it without creating a full `proxy.Server` instance. The gateway's `server.Server`
exposes its `peerManager` via `GetPeerManager()` for use by the Waker.

## Authorization

K8s RBAC grants the gateway ServiceAccount `update`/`patch` on `sandboxes` resources.
No systemtoken, no HTTP call to sandbox-manager.

## Comparison with PR #495

| Aspect | PR #495 | This Proposal |
|--------|---------|---------------|
| Wake trigger | Gateway calls manager `/connect` HTTP API | Gateway calls `sandboxcr.Sandbox.Resume()` directly (Go function) |
| Auth | System key (systemtoken) | K8s RBAC (ServiceAccount) |
| Resume logic | Manager's `ResumeSandbox` HTTP handler | Same `sandboxcr.Sandbox.Resume()` method (imported, not HTTP) |
| Wait mechanism | Manager's `NewSandboxResumeTask().Wait()` | Same (reused via Resume call) |
| Spec patching | Manager's `retryUpdate` inside Resume | Same (reused via Resume call) |
| Concurrent dedup | Manager's first-writer-wins | Same (reused via Resume call) |
| Route sync after wake | Manager's `syncRoute` | Gateway mirrors: `registry.Update` + `proxy.SyncRouteWithPeers` |
| Timeout update | Manager's connect API handles it | Gateway passes `ResumeOptions.Timeout.PauseTime` |
| New components | `systemkey.go`, `wake/client.go`, `wake/wake.go` | `wake/wake.go` (thin wrapper) |
| Cross-component deps | Gateway -> Manager HTTP | Gateway imports `pkg/sandbox-manager/infra/sandboxcr` (Go import) |
| Cache provider | Manager has its own | Gateway creates its own via `cache.NewCache(mgr)` |

## Timeout Handling

The `AnnotationWakeTimeoutSeconds` annotation stores the auto-pause timeout. When the
gateway wakes a sandbox:
1. It reads the annotation to determine `PauseTime` (re-arming auto-pause)
2. If the annotation is absent, it uses the filter's `WakeTimeoutSeconds` config default
3. The timeout is passed as `ResumeOptions.Timeout.PauseTime`, which is written atomically
   with `Spec.Paused = false` inside `retryUpdate` — closing the auto-pause race
