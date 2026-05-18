# Wake-on-Traffic Auto-Resume

## Context

Today, a paused sandbox returns to running only when an SDK explicitly issues `/sandboxes/{id}/connect` or `/sandboxes/{id}/resume`. Data-plane traffic to a paused sandbox is rejected by `pkg/sandbox-gateway/filter/filter.go:95-105` with a 502 `sandbox_not_running`. The peer-refresh handler `pkg/sandbox-gateway/server/server.go:174-177` removes any non-Running route from the registry, so a paused entry can briefly disappear from a gateway replica between the manager push and the local informer write-back.

E2B's `POST /sandboxes` create API exposes `autoResume: SandboxAutoResumeConfig { enabled: bool }`, and the [auto-resume feature documentation](https://e2b.dev/docs/sandbox/auto-resume) defines the wake semantics: any HTTP traffic or SDK operation against a paused sandbox transparently resumes it; on resume, the timeout restarts with a 5-minute minimum, or the original duration if it was longer; the lifecycle configuration persists across pause/resume cycles.

The project does not implement these semantics. Adding them requires producing a wake event from the gateway data-plane filter, executing the resume in sandbox-manager, restoring an appropriate timeout, and forwarding the original request to the now-running pod.

## Goals

- Auto-resume any paused sandbox whose CR carries `agents.kruise.io/wake-on-traffic` when data-plane traffic arrives at the gateway.
- Mirror E2B `autoResume` semantics: enabled at create time via the top-level `autoResume.enabled` wire field; resume restores the timeout that was last set by create / connect / set-timeout; finite resume timeouts are clamped to a 5-minute minimum.
- Support a K8S-direct workflow: a user can patch the annotation on a Sandbox CR plus `spec.paused=true` and obtain wake-on-traffic without going through the E2B API.
- Single-PR scope; touch only the components needed to thread the wake path end-to-end.
- Cross-replica correct on both gateway side (multi-replica deployment) and manager side (existing first-writer-wins resume).

## Non-goals

- No CRD changes. The protocol is annotation-only.
- No new admission webhook. Annotation validation lives in the manager wake handler; non-wake paths do not validate.
- No owner/tenant identity check on the new internal endpoint. JWT/access-token-to-owner mapping is a separate workstream and is explicitly deferred.
- No changes to sandbox-controller. Auto-pause via `spec.PauseTime` and auto-shutdown via `spec.ShutdownTime` continue unchanged.
- No idle-detection or activity-tracking. "Wake on traffic" is wake-from-paused, not refresh-on-traffic; running sandboxes are forwarded without timeout side-effects.
- No changes to existing `/sandboxes/{id}/connect` and `/sandboxes/{id}/timeout` semantics other than an additive annotation-sync side-effect.
- No async upgrade for non-wake filter paths. The async pattern is added only on the wake branch of `DecodeHeaders`.

## Design

### Annotation protocol

A single annotation `agents.kruise.io/wake-on-traffic` carries the wake policy. Values use a `<kind>:<payload>` form so future kinds can be added without breaking the parser. Only one kind, `timeout`, is defined now.

```go
// api/v1alpha1/annotations.go
const AnnotationWakeOnTraffic = InternalPrefix + "wake-on-traffic"
```

Value grammar:

| Value | Meaning |
|---|---|
| absent | wake-on-traffic disabled; gateway returns existing 502 on paused traffic |
| `"timeout:never"` | enabled; on wake, do not write any new `ShutdownTime` or `PauseTime` |
| `"timeout:<duration>"` where duration is a Go duration ≥ 1s, lower-case (`"timeout:300s"`, `"timeout:5m"`, `"timeout:2h30m"`) | enabled; on wake, set the new deadline to `now + duration` |
| anything else (unknown kind, unparsable payload, etc.) | invalid; manager returns InvalidAutoResumePolicy; gateway falls back to 502 |

Parsing rules for the `timeout` kind:

- `never` is matched literally (case-sensitive). No leading/trailing whitespace tolerated.
- Duration: parsed via `time.ParseDuration`; reject `≤ 0` and anything below 1s.
- The parser tries `never` first, then duration. The two forms are textually disjoint.

The `<duration>` payload is exactly the wire form `metav1.Duration` uses across Kubernetes (`5m`, `300s`, `2h30m` — anything `time.ParseDuration` accepts). Hand-patched annotations therefore look indistinguishable from `metav1.Duration` fields in any other CR, which is the reason for choosing this form over an absolute-time alternative.

Strict parsing rejects `""`, `"true"`, `"0"`, `"timeout:"`, `"timeout:0"`, `"timeout:0s"`, `"timeout:-1s"`, `"timeout:500ms"`, `"timeout:Never"`, `"timeout: 300s"`, and any value whose kind is not `timeout`. No silent trim, no case folding, no minimum-floor clamp at parse time (the 5-minute floor is applied later, in `WakeSandbox`).

The `agents.kruise.io/` prefix is already on `BlackListPrefix` (`pkg/servers/e2b/metadata.go:24`), so E2B users cannot inject this key through the public `metadata` field; only the manager itself or a cluster admin via `kubectl` can write it.

### E2B create wire mapping

`models.NewSandboxRequest` (`pkg/servers/e2b/models/sandbox.go:50`) gains the field defined by E2B:

```go
type NewSandboxRequest struct {
    // ... existing fields ...
    AutoResume *AutoResumeConfig `json:"autoResume,omitempty"`
    // ...
}

type AutoResumeConfig struct {
    Enabled bool `json:"enabled"`
}
```

Pointer keeps "user did not send `autoResume` at all" distinguishable from `{enabled: false}` and matches E2B's nullable shape. The manager does not parse the SDK-side `lifecycle` wrapper; both stock E2B Python and JS SDKs flatten it to the wire shape before sending.

Annotation write decision in `basicSandboxCreateModifier` (`pkg/servers/e2b/create.go:269-296`):

```go
if request.AutoResume != nil && request.AutoResume.Enabled {
    var payload string
    if request.Extensions.NeverTimeout {
        payload = "timeout:never"
    } else {
        payload = fmt.Sprintf("timeout:%ds", request.Timeout)
    }
    sbx.SetAnnotation(v1alpha1.AnnotationWakeOnTraffic, payload)
}
```

`autoPause` and `autoResume.enabled` are orthogonal. `autoPause=false` + `autoResume.enabled=true` is legal: the sandbox is deleted on timeout in the auto-shutdown path, but a manual pause leaves the sandbox alive and wake-on-traffic restores it.

### Annotation updates piggyback on `SaveTimeoutWithPolicy`

Connect, set-timeout, and wake all need to update the wake-on-traffic annotation in lockstep with the timeout fields they write. Doing this as a separate `retryUpdate` adds an extra round trip, an extra conflict-retry surface, and a second observability point. Instead, `SaveTimeoutWithPolicy` is the single place that mutates the annotation alongside `Spec.ShutdownTime` / `Spec.PauseTime`.

`timeout.Options` gains a `SetAnnotations` field:

```go
// pkg/utils/timeout/types.go
type Options struct {
    ShutdownTime time.Time
    PauseTime    time.Time
    Baseline     *Options          `json:"-"`
    // SetAnnotations is applied to metadata.annotations inside the same retryUpdate
    // modifier that writes ShutdownTime / PauseTime. Empty-string values delete the key.
    SetAnnotations map[string]string `json:"-"`
}
```

Inside `Sandbox.SaveTimeoutWithPolicy`'s modifier, after the policy decides `shouldUpdate == true`, the modifier:

1. Calls `setTimeout(sbx, opts)` (existing behavior).
2. Iterates `opts.SetAnnotations`. For each key, sets `sbx.Annotations[key] = value`, or `delete(sbx.Annotations, key)` if value is empty string.
3. Returns `(true, nil)`.

When `shouldUpdate == false`, the annotation is also not changed — i.e., annotation drift never outpaces the timeout it describes.

This keeps the annotation atomic with the timeout from the K8s API server's perspective, simplifies the call sites, and gives `BaselineAware` / `ExtendOnly` a single decision point that covers both fields.

### E2B connect / set-timeout annotation sync rules

Handler logic for both `ConnectSandbox` and `SetSandboxTimeout`:

- If the sandbox carries no `wake-on-traffic` annotation, the handler does not put anything in `opts.SetAnnotations`. The handlers never silently enable wake-on-traffic.
- If the sandbox carries the annotation, the handler builds `opts.SetAnnotations[v1alpha1.AnnotationWakeOnTraffic] = "timeout:<request.TimeoutSeconds>s"` and passes it through `SaveTimeoutWithPolicy`. The policy machinery then decides whether to write both fields together.
- The handler does not need to special-case "annotation update should not happen on a no-op timeout write" — the modifier's `shouldUpdate == false` branch covers that automatically.

`Pause` does not touch the annotation. `Resume` does not touch the annotation. `WakeSandbox` does not touch the annotation either (see below). The annotation is set only at create and updated only at connect / set-timeout.

### Shared connect/wake core

`ConnectSandbox` and the new `WakeSandbox` share a manager-level helper that runs resume-if-paused plus the single timeout/annotation write. Auth, request parsing, the 5-minute clamp, and any handler-specific computation stay in the callers.

```go
// pkg/sandbox-manager/connect_core.go

// ConnectOrWakeInput describes the shared resume + timeout-write request.
// Exported only because callers live in other packages (pkg/servers/e2b for
// ConnectSandbox, this package for WakeSandbox); semantically it is an internal
// building block and should not be invoked by anything outside those handlers.
type ConnectOrWakeInput struct {
    // Snapshot fields captured by the caller before Resume:
    PreState   string                  // sbx.GetState() return at handler entry
    AutoPause  bool                    // ParseTimeout result at handler entry
    PreEndAt   time.Time               // ParseTimeout result at handler entry
    Baseline   timeout.Options         // sbx.GetTimeout() at handler entry

    // Caller-computed target deadline:
    NewEndAt   time.Time               // zero ⇒ leave timeouts cleared (never policy or never-timeout sandbox)

    // Annotations to set/delete atomically with the timeout write:
    SetAnnotations map[string]string
}

// ConnectOrWake is exported so the e2b ConnectSandbox handler can call it from
// pkg/servers/e2b. WakeSandbox in this package calls it directly.
func (m *SandboxManager) ConnectOrWake(ctx context.Context, sbx infra.Sandbox, in ConnectOrWakeInput) error {
    // 1. If paused, run Resume (first-writer-wins, shared task wait).
    if in.PreState == v1alpha1.SandboxStatePaused {
        if err := m.ResumeSandbox(ctx, sbx, infra.ResumeOptions{}); err != nil {
            return err
        }
    }

    // 2. Never-timeout sandbox stays never-timeout — skip the write entirely.
    //    This preserves the existing Rule 1 from updateConnectTimeout.
    if in.PreEndAt.IsZero() && in.NewEndAt.IsZero() && len(in.SetAnnotations) == 0 {
        return nil
    }

    // 3. Build the timeout payload from NewEndAt + autoPause.
    //    m.maxTimeout is the manager's hard upper bound on shutdown time, mirrored
    //    from the existing e2b Controller config; routing it through the manager
    //    is part of this refactor (see Code Impact).
    var opts timeout.Options
    if !in.NewEndAt.IsZero() {
        if in.AutoPause {
            opts.PauseTime    = in.NewEndAt
            opts.ShutdownTime = time.Now().Add(m.maxTimeout)
        } else {
            opts.ShutdownTime = in.NewEndAt
        }
    }
    opts.Baseline       = &in.Baseline
    opts.SetAnnotations = in.SetAnnotations

    // 4. Pick policy: BaselineAware for paused→resumed paths, ExtendOnly for running.
    policy := timeout.UpdatePolicyExtendOnly
    if in.PreState == v1alpha1.SandboxStatePaused {
        policy = timeout.UpdatePolicyBaselineAware
    }

    _, err := sbx.SaveTimeoutWithPolicy(ctx, opts, policy)
    return err
}
```

Both callers funnel into `ConnectOrWake`. The annotation update piggybacks on `SaveTimeoutWithPolicy` via `opts.SetAnnotations` (see the previous subsection). The "should we write at all" decision is centralised in two places: this function (the never-timeout fast return) and `SaveTimeoutWithPolicy`'s policy switch (extend-only / baseline-aware degrade).

The legacy `ResumeSandbox` HTTP handler in `pkg/servers/e2b/pause_resume.go` is left as-is (deprecated; old SDK only); it is not migrated to `ConnectOrWake` because its behavior is "always overwrite" rather than the policy-aware path connect/wake share.

### Manager `WakeSandbox`

A new entry point `WakeSandbox` on `*SandboxManager` consumes the annotation, computes the wake-specific target deadline (including the 5-minute floor), and dispatches to `ConnectOrWake`.

```
1. sbx := m.infra.GetClaimedSandbox(...)        // informer + APIReader fallback
2. policy := ParseWakeOnTrafficPolicy(sbx.Annotations)
     absent  → return AutoResumeDisabled (no write)
     invalid → return InvalidAutoResumePolicy (no write)
3. state, _ := sbx.GetState()
     running → return AlreadyRunning (no write)
     other than paused → map to Pausing / BadState / Gone / NotFound (no write)
4. if !sandboxutils.IsSandboxPausable(sbx) → return BadState (no write)
   // Filters out resuming/pending/terminating sandboxes that share the
   // "paused" gateway state but cannot be safely resumed yet.
5. autoPause, currentEndAt := ParseTimeout(sbx)
6. baseline := sbx.GetTimeout()
7. Compute newEndAt:
     - never policy           → zero time
     - never-timeout sandbox  → zero time (preserved by ConnectOrWake)
     - duration policy        → now + max(duration, wakeMinimumTimeout)
8. m.ConnectOrWake(ctx, sbx, ConnectOrWakeInput{
       PreState:       v1alpha1.SandboxStatePaused,
       AutoPause:      autoPause,
       PreEndAt:       currentEndAt,
       Baseline:       baseline,
       NewEndAt:       newEndAt,
       SetAnnotations: nil,                     // wake never rewrites the annotation
   })
9. m.syncRoute(ctx, sbx, true)                  // push running state to peers
10. return WakeResult{Action: Resumed, State: running, ResourceVersion: ...}
```

`WakeResult.ResourceVersion` is the manager replica's best-effort observed Sandbox version after the wake attempt. The gateway must not use it as a correctness gate; convergence still comes from polling the local registry until the route is observed as `Running`.

The 5-minute floor is a manager package-level constant `wakeMinimumTimeout = 5 * time.Minute`, applied only inside `WakeSandbox` and only on the duration form. The never policy bypasses it (no deadline written). `ConnectSandbox` does not see this constant.

`WakeSandbox` does not write the wake-on-traffic annotation. Annotation maintenance is the create / connect / set-timeout responsibility (per the previous section).

### `ConnectSandbox` refactor

`ConnectSandbox` after this change:

1. Auth + parse `request.TimeoutSeconds` (existing).
2. Get user's sandbox (existing).
3. Snapshot `state`, `autoPause`, `currentEndAt`, `baseline` (existing).
4. Build the desired `newEndAt` from `request.TimeoutSeconds` (relative to `now`); apply the existing extend-only "Rule 2" guard at this point if `state == running`, by skipping the call when the requested end-at would not extend.
5. Build `SetAnnotations` for the wake-on-traffic key only when the existing annotation is present (per the previous section).
6. Call `m.ConnectOrWake(...)` with the input above. Return its error to the SDK.

The existing `updateConnectTimeout` helper is removed; its Rule 1 (never-timeout) lives in `ConnectOrWake`, its Rule 2 (extend-only on running) becomes a guard in step 4 above (the policy degrade in `SaveTimeoutWithPolicy` would also catch it, but doing the check before the call avoids an unnecessary `retryUpdate` round-trip in the common no-op case).

### Internal HTTP endpoint

`POST /wake/{sandboxID}` is registered on the existing internal mux owned by `pkg/proxy/server.go` (port `proxy.SystemPort = 7789`), alongside the existing `/refresh`. The handler is anonymous, the same trust model `/refresh` already uses: protection comes from the network boundary (cluster-internal port, no Ingress, NetworkPolicy), not from request-level credentials.

Request body: empty.

Response body:

```json
{
  "action": "Resumed | AlreadyRunning | AutoResumeDisabled | InvalidAutoResumePolicy | NotFound | Pausing | BadState | Gone",
  "message": "...",
  "state": "running | paused | creating | dead",
  "resourceVersion": "..."
}
```

HTTP status mapping:

| Action | Status |
|---|---|
| Resumed, AlreadyRunning | 200 |
| AutoResumeDisabled, InvalidAutoResumePolicy | 422 |
| Pausing, BadState | 409 |
| Gone | 410 |
| NotFound | 404 |
| any infra failure | 500 |

A request-level shared token / mTLS / SA-token verification is **explicitly out of scope** for v1. If a future threat model demands it (operators auditing in-cluster blast radius, hostile co-tenancy, etc.), it lives in a follow-up ticket. The wake handler is one deny rule away from being unreachable, and that deny rule is the v1 boundary.

### Gateway Route extension

`proxy.Route` (`pkg/proxy/routes.go:39-46`) gains two fields:

- `WakeOnTraffic string` — raw annotation value verbatim; the gateway parses it on the wake path.
- `Pausable bool` — `sandboxutils.IsSandboxPausable(sbx)` evaluated at Route construction. The gateway uses this as the gate for triggering a wake call (see the filter section below). Computing it once at Route construction avoids re-walking conditions / phase fields per request.

JSON encoding for both fields is forward-compatible: old peers ignore unknown fields on inbound, new peers see zero values when receiving from old peers — both safe.

`proxyutils.DefaultGetRouteFunc` (`pkg/utils/sandbox-manager/proxyutils/default.go`) reads `sandbox.Annotations[v1alpha1.AnnotationWakeOnTraffic]` and `sandboxutils.IsSandboxPausable(sandbox)` and copies them to the Route. The gateway controller `pkg/sandbox-gateway/controller/gateway_controller.go:61-63` populates Route via this helper, so both fields flow from the CR to every replica's local registry through both the local informer and the manager's `SyncRouteWithPeers` push.

### Refresh handler relaxation

`pkg/sandbox-gateway/server/server.go:174-177` currently deletes any non-Running route. This was a defensive measure to avoid routing to unhealthy sandboxes; with wake-on-traffic, paused entries must remain visible so the filter can find them. The handler is relaxed to:

```go
switch route.State {
case v1alpha1.SandboxStateDead, "":
    registry.GetRegistry().Delete(route.ID)
default:
    registry.GetRegistry().Update(route.ID, route)
}
```

This matches the local controller's path (`gateway_controller.go:61-63`), which already keeps non-Running routes via `Update`.

### Gateway async filter

`pkg/sandbox-gateway/filter/filter.go` extends `DecodeHeaders` with an async branch on the paused + wake-on-traffic path. The non-wake paths remain synchronous and unchanged.

```
DecodeHeaders:
  parse / lookup registry / extract sandboxID, port (existing)

  if route.State == Running:
    set dynamic metadata (existing)
    return api.Continue

  // Wake gate: must satisfy ALL three:
  //   - State is paused (route.State == "paused")
  //   - The sandbox is in a state we can resume from (route.Pausable)
  //   - wake-on-traffic is configured on the CR (route.WakeOnTraffic != "")
  // Any other combination falls through to the existing 502 path.
  if !(route.State == Paused && route.Pausable && route.WakeOnTraffic != ""):
    SendLocalReply 502 (existing)
    return api.LocalReply

  spawn goroutine (parented to filter context — cancelled on OnDestroy):
    defer recover() — turn panics into SendLocalReply 500
    err := wakeAndWait(filterCtx, sandboxID)
    f.completeOnce.Do(func() {
      if err != nil:
        SendLocalReply with mapped status, no extraHeaders
      else:
        re-read registry, set extraHeaders + dynamic metadata, Continue
    })
  return api.Running
```

Invariants:

- `f.completeOnce sync.Once` guards `Continue` and `SendLocalReply`. Calling either twice on the same stream panics inside Envoy.
- `OnDestroy` (`api.StreamFilter.OnDestroy`) cancels the filter context and disables the `completeOnce` block, preventing callbacks on a destroyed stream. The client closing the TCP connection is what bounds the wait.
- The gateway does **not** apply its own wake-side timeout. Production wake operations regularly take tens of seconds (image pull, pod scheduling, runtime re-init) and a hardcoded gateway timeout would convert healthy slow waking into 503 storms. The bounding signals are: (a) the client connection going away (OnDestroy → ctx cancellation), (b) Envoy's listener-level `stream_idle_timeout` / route `timeout` configured in `config/sandbox-gateway/configmap.yaml` (currently 600s), (c) the manager's own `Resume` task wait inside `Sandbox.Resume` (1 minute hard cap). Any of these is sufficient to terminate a hopeless wake.
- `wakeAndWait` runs under a per-replica `singleflight.Group` keyed by sandbox ID. Concurrent requests against the same paused sandbox in the same replica share one manager call and one wait loop. Cross-replica deduplication is not attempted; manager-side `Resume`'s first-writer-wins handles it.
- `wakeAndWait` returns either nil (after observing `route.State == Running` in the registry) or a typed error mapped to HTTP. Manager response codes flow through.
- Header mutation happens only after the goroutine completes; the original `header` map's lifetime ends with the synchronous return from `DecodeHeaders`. The goroutine uses `f.callbacks` to set headers and dynamic metadata.

`wakeAndWait` body:

```
1. POST :7789/wake/{id} (no token); HTTP client timeout follows ctx, no extra deadline
2. on 422/410/404 → return mapped error immediately
3. on 200 → enter registry poll:
     for !ctx.Done():
       r := registry.Get(id)
       if r.State == Running: return nil
       sleep backoff (50ms → 100ms → 200ms → 500ms cap)
4. on ctx.Done() → return ctx.Err() (client cancelled or filter destroyed)
```

Error-to-status mapping at the gateway:

| wake error | HTTP | `Retry-After` |
|---|---|---|
| AutoResumeDisabled | 502 sandbox_not_running | none |
| InvalidAutoResumePolicy | 503 | 0 |
| Pausing | 503 | 5 |
| BadState (creating, etc.) | 503 | 15 |
| Gone | 502 sandbox_not_found | none |
| NotFound | 502 sandbox_not_found | none |
| transport / 5xx | 503 | 5 |
| ctx cancelled (client disconnect) | no reply emitted — Envoy already cleaning up | — |

`AutoResumeDisabled` is mapped to the existing 502 to keep the no-wake response shape stable for clients; this case only arises in races where the route says wake-on-traffic is set but the manager observes it absent.

### Network boundary

The wake endpoint has no application-level authentication; protection comes entirely from the network layer. This is consistent with the existing `/refresh` endpoint on the same port.

A new NetworkPolicy in `config/network-policy/` is required (or, if a manager NetworkPolicy already exists, the gateway label is added to its `:7789` allow rules). Allowed sources for `:7789`:

- Manager-to-manager peer mesh — existing rule, unchanged.
- Pods matching the gateway's `metadata.labels` (specific selector to be confirmed against `config/sandbox-gateway/deployment.yaml` at implementation).

Other in-cluster pods cannot reach `:7789`. The Service does not expose this port via Ingress.

A ClusterIP Service exposing `:7789` for cluster DNS resolution from the gateway is added if one does not already exist (port name: `internal`). The existing manager Service used by `/kruise/api/*` proxy traffic is at `:7788` and is unaffected.

## State machine and concurrency

### Wake outcomes by sandbox state

The `IsSandboxPausable` gate at the manager (and on the gateway via `Route.Pausable`) keeps the wake path narrow: only `Phase ∈ {Running, Paused}` qualifies, plus the additional rules below.

| Observed CR shape | gateway state | `IsSandboxPausable` | `IsSandboxResumable` | wake action | manager response |
|---|---|---|---|---|---|
| Phase=Running, Spec.Paused=false, Ready=true | running | true | true | no-op | AlreadyRunning |
| Phase=Running, Spec.Paused=true (pause in flight, controller has not yet deleted pod) | paused | true | true | Resume (fast path: Ready may already be true → no-op; else flips Spec.Paused=false) + ConnectOrWake (BaselineAware) | Resumed |
| Phase=Paused, cond[SandboxPaused]=True (pause completed) | paused | true | true | Resume + ConnectOrWake (BaselineAware) | Resumed |
| Phase=Paused, cond[SandboxPaused]=False (pod still terminating) | paused | true | false | reject inside Resume's `IsSandboxResumable` pre-check | Pausing (409) |
| Phase=Resuming | paused | false | true | reject at `IsSandboxPausable` gate (gateway pre-filtered; manager defensive) | BadState (409) |
| Phase=Pending / state=creating | creating | false | false | reject | BadState (409) |
| state=dead, `DeletionTimestamp != nil`, or `Phase ∈ {Failed, Succeeded, Terminating}` | dead | false | false | reject | Gone (410) |
| not found / not claimed | n/a | n/a | n/a | reject | NotFound (404) |

Phases for which both gateway and manager agree to reject without a Resume attempt are filtered at the gateway by the `route.Pausable` check; the manager-side `IsSandboxPausable` is a defensive double-check for direct K8S-driven traffic that bypasses the gateway.

### Concurrent wake / connect on same sandbox

- Two wake calls (different gateway replicas, both routed to one manager replica or split): both enter `Sandbox.Resume`. The `retryUpdate` modifier is `if !sbx.Spec.Paused { return false }`, so the second caller is `resumeUpdated == false` and skips post-resume `InitRuntime` / CSI. Both wait on the shared `resumeTask`. Both proceed to `ConnectOrWake` → `SaveTimeoutWithPolicy(BaselineAware)` with identical baselines (informer view at request entry).
- The first `SaveTimeoutWithPolicy` call sees `current == baseline` and (because `BaselineAware` falls through to `Always` semantics in this branch) writes its computed timeout. The second call sees `current != baseline` and degrades to `ExtendOnly`. For two finite values, the longer one survives.
- Connect concurrent with wake: connect captures its own baseline and threads through the same `ConnectOrWake`. The first writer applies `Always` semantics; the second degrades to `ExtendOnly`. For finite-vs-finite the longer survives.
- Cross-form race (one writer finite, the other `never` ⇒ zero-time): the first writer always wins. The first writer's `BaselineAware` matches and writes via `Always` semantics. The second writer's `BaselineAware` mismatches, degrades to `ExtendOnly`, and `ShouldExtendTimeout` treats either side being zero as "no comparable end-time" and returns false — so the second write is a no-op regardless of which form it carries. This is a deliberate property of the existing policy machinery: it never silently shrinks an already-extended timeout, and it never silently demotes "no deadline" to a finite one. The same property applies today to never-timeout sandboxes — connect and set-timeout's existing Rule 1 short-circuits the call when `currentEndAt.IsZero()`. Users who need to override a once-applied `never` reset the wake-on-traffic annotation and pause/resume cycle the sandbox; this is not a new constraint introduced by wake-on-traffic.

### Annotation drift

If `ConnectSandbox` succeeds in `Resume` but fails in `SaveTimeoutWithPolicy` (network glitch, conflict storm), the annotation is also not updated. The annotation remains consistent with the spec timeout. The handler returns 5xx and the client retries.

If `ConnectSandbox` succeeds in both timeout write and annotation update but the SDK never receives the response (network drop after manager commit), the next request finds a Running sandbox with the new timeout and matching annotation. No reconciliation is needed.

### Cross-replica correctness

- Per-replica gateway `singleflight` deduplicates within one replica. Multi-replica gateway: each replica independently calls manager, manager handles concurrency.
- Manager resume is first-writer-wins with shared wait. Manager `SaveTimeoutWithPolicy` is `BaselineAware` and resolves any concurrent timeout writes via "longest timeout wins" semantics.
- The baseline travels with the request, captured at the manager replica handling that request. The k8s API server is the only authoritative source consulted under conflict (via `retryUpdate`'s API-reader refresh).
- Informer staleness on the gateway can produce a transient `paused` Route after manager has resumed. The wake call then returns `AlreadyRunning`; the gateway retries the registry poll and converges once its own informer catches up. Worst case is a ~1s delay; never an incorrect routing decision.

## Behavior under existing flows

### Auto-pause (controller-driven via `Spec.PauseTime`)

The controller flips `Spec.Paused = true` without writing any timeout. The wake annotation, if present, was set at create / connect / set-timeout and is preserved. When traffic later arrives, gateway invokes manager wake; the manager observes the annotation, runs Resume, and writes the new timeout. Identical to manual-pause behavior from the wake perspective.

### Manual pause (E2B `/pause`)

Manual pause sets `Spec.Paused = true` and rewrites both `ShutdownTime` and `PauseTime` to `now+1000y` (`buildPauseTimeoutOptions`, `pause_resume.go:65-77`), so the controller does not auto-shutdown the paused sandbox. The annotation is untouched. Wake reads the annotation, calls Resume (which now lifts `Spec.Paused = false` but leaves the 1000y deadlines in spec), then `SaveTimeoutWithPolicy(BaselineAware)` overwrites them with `now + effective` (or clears them for `never`). The 1000y baseline matches what the handler observed pre-Resume, so `BaselineAware` uses `Always` semantics and the overwrite proceeds.

### Never-timeout sandbox (`Extensions.NeverTimeout=true` at create)

`Spec.PauseTime` and `Spec.ShutdownTime` are nil at all times. `ParseTimeout` returns `currentEndAt.IsZero() == true`. Connect, set-timeout, and wake all skip timeout writes for these sandboxes. The annotation, if present, is `"timeout:never"`. The pre-write guards work as follows:

- `WakeSandbox` produces `NewEndAt = zero` for never policy and for never-timeout sandboxes (the duration form only matters when the sandbox actually had a deadline before pause).
- `ConnectOrWake` then sees `PreEndAt.IsZero() && NewEndAt.IsZero() && len(SetAnnotations) == 0`, returns nil without calling `SaveTimeoutWithPolicy`.
- For the never-policy-on-previously-finite case, `PreEndAt` is non-zero and `NewEndAt` is zero, which falls through to `SaveTimeoutWithPolicy` with `opts` containing zero times; `setTimeout` then clears `Spec.PauseTime` / `Spec.ShutdownTime` to nil. This is the only path through which paused 1000y deadlines get reset to "no deadline".

### Wake on running sandbox (defensive)

Gateway only triggers wake when `route.State != Running`. Manager wake on a running sandbox (from informer races or admin debug) returns AlreadyRunning without touching timeout or annotation. No write.

## Code Impact

### New files

- `pkg/sandbox-manager/wakeontraffic.go` — `ParseWakeOnTrafficPolicy` (handles `timeout:never` / `timeout:<duration>`) and the resulting `Policy` type.
- `pkg/sandbox-manager/wake.go` — `WakeSandbox` orchestration plus the 5-minute floor constant.
- `pkg/sandbox-manager/connect_core.go` — exported `ConnectOrWake` helper plus `ConnectOrWakeInput` shared by `WakeSandbox` (same package) and the e2b connect handler (in `pkg/servers/e2b`).
- `pkg/proxy/wake_handler.go` — internal anonymous HTTP handler for `/wake/{id}`.
- `pkg/sandbox-gateway/wake/wake.go` — `wakeAndWait`, `singleflight.Group`, manager HTTP client (no auth header).
- `pkg/sandbox-gateway/filter/async.go` — async `DecodeHeaders` glue, `completeOnce`, `OnDestroy` integration.
- `config/network-policy/sandbox-manager-internal.yaml` — ingress allow rule for the gateway → manager `:7789` path.

### Modified files

- `api/v1alpha1/annotations.go` — `AnnotationWakeOnTraffic` constant.
- `pkg/proxy/routes.go` — `Route.WakeOnTraffic string`, `Route.Pausable bool` fields.
- `pkg/utils/timeout/types.go` — `Options.SetAnnotations map[string]string` field.
- `pkg/sandbox-manager/infra/sandboxcr/sandbox.go` — `SaveTimeoutWithPolicy` modifier applies `opts.SetAnnotations` together with `setTimeout`.
- `pkg/sandbox-manager/core.go` — `SandboxManager` gains a `maxTimeout time.Duration` field (mirrored from the CLI flag previously consumed only by the e2b Controller); `ConnectOrWake` reads it for the auto-pause `ShutdownTime` upper-bound. The e2b Controller continues to hold its own copy for request validation; the value is passed in once at builder time.
- `pkg/utils/sandbox-manager/proxyutils/default.go` — `DefaultGetRouteFunc` populates `WakeOnTraffic` and `Pausable`.
- `pkg/sandbox-gateway/server/server.go` — `handleRefresh` keeps non-Dead routes (the existing delete-on-non-Running guard is relaxed).
- `pkg/sandbox-gateway/filter/filter.go` — async branch with the three-condition wake gate.
- `pkg/sandbox-gateway/filter/config.go` — manager URL config (no token).
- `cmd/sandbox-gateway/main.go` — init wake module.
- `pkg/proxy/server.go` — register anonymous `/wake/{id}` on the internal mux.
- `pkg/servers/e2b/models/sandbox.go` — `AutoResume *AutoResumeConfig`, `AutoResumeConfig` type.
- `pkg/servers/e2b/create.go` — `basicSandboxCreateModifier` writes the annotation in `timeout:<seconds>s` / `timeout:never` form.
- `pkg/servers/e2b/pause_resume.go` — `ConnectSandbox` reshapes around `m.ConnectOrWake`; `updateConnectTimeout` is removed; annotation update is carried through `opts.SetAnnotations`.
- `pkg/servers/e2b/timeout.go` — `setSandboxTimeout` carries annotation update through `opts.SetAnnotations`.
- `config/sandbox-manager/service.yaml` — expose `:7789` (or add a new internal Service) for cluster DNS resolution from the gateway.

### Not modified

- `api/v1alpha1/sandbox_types.go` — no CRD field changes.
- `config/crd/bases/agents.kruise.io_sandboxes.yaml` — no CRD schema changes.
- `pkg/controller/sandbox/...` — no controller behavior changes.
- `config/rbac/...` — gateway already has read-only access to Sandbox CRs; manager already has full access. No new permissions needed.
- No new K8s Secret. No `KRUISE_INTERNAL_TOKEN` env var. No `Makefile` token target.

## Test Plan

### Annotation parser (`pkg/sandbox-manager/wakeontraffic_test.go`)

- Disabled: annotation absent on a populated map.
- Never: exact `"timeout:never"` string.
- Duration form: `"timeout:1s"`, `"timeout:30s"`, `"timeout:5m"`, `"timeout:2h30m"`.
- Absolute timestamp rejected: `"timeout:2099-01-01T00:00:00Z"` and other RFC3339-looking payloads return InvalidPolicy.
- Invalid kind: `"foo:300s"`, `"timeout"` (no colon), `":300s"`.
- Invalid duration payload: `""`, `"true"`, `"false"`, `"timeout:"`, `"timeout:0"`, `"timeout:0s"`, `"timeout:-1s"`, `"timeout:500ms"`, `"timeout:abc"`, `"timeout:5"`, `"timeout:Never"`, `"timeout:NEVER"`, `"timeout: 300s"`, `"timeout:300s\n"`.
- Boundary: `"timeout:1s"` accepted, `"timeout:999ms"` rejected.
- Round-trip: `"timeout:300s"` → Policy with Duration=300s and Form=duration; `"timeout:never"` → Policy with Form=never.

### Manager wake (`pkg/sandbox-manager/wake_test.go`)

- WakeSandbox(paused, duration, autoPause=false) writes ShutdownTime = now + max(duration, 5min); no PauseTime.
- WakeSandbox(paused, duration, autoPause=true) writes PauseTime = now + max(duration, 5min); ShutdownTime = now + maxTimeout.
- WakeSandbox(paused, never) clears both timeouts.
- WakeSandbox(paused, duration < 5min) clamps to 5min.
- WakeSandbox(paused, duration >= 5min) preserves duration.
- WakeSandbox(running) returns AlreadyRunning, asserts no Update was issued.
- WakeSandbox(annotation absent) returns AutoResumeDisabled.
- WakeSandbox(annotation invalid) returns InvalidAutoResumePolicy.
- WakeSandbox(deletionTimestamp set) returns Gone.
- WakeSandbox(Spec.Paused && cond Paused == False) returns Pausing — both via state and via `IsSandboxPausable` returning false.
- WakeSandbox(state=creating) returns BadState.
- WakeSandbox(state=resuming, IsSandboxPausable=false) returns BadState.
- WakeSandbox(not found) returns NotFound.
- WakeSandbox does NOT mutate the wake-on-traffic annotation.
- Concurrent wake + wake: second caller skips post-resume init, both succeed; longest timeout survives.
- Cross-form race (driven by SaveTimeoutWithPolicy ordering, asserted in two sub-cases):
  - Connect (finite 300s) writes first, then wake (never) writes second → spec keeps the 300s deadline; wake's no-op is observed via `TimeoutUpdateResult.Updated == false`.
  - Wake (never) writes first, then connect (finite 300s) writes second → spec keeps the cleared (zero) deadline; connect's no-op is observed via `TimeoutUpdateResult.Updated == false`.
  Both sub-cases verify the "first writer wins, second is a no-op" property of `BaselineAware` + `ExtendOnly` against zero-time semantics.

### Shared `ConnectOrWake` core (`pkg/sandbox-manager/connect_core_test.go`)

- Paused input: triggers Resume; SaveTimeoutWithPolicy with BaselineAware.
- Running input: skips Resume; SaveTimeoutWithPolicy with ExtendOnly.
- Never-timeout sandbox + zero NewEndAt + empty SetAnnotations → no write.
- Never-timeout sandbox + zero NewEndAt + non-empty SetAnnotations → annotation update still flows through.
- AutoPause=true + finite NewEndAt → PauseTime + ShutdownTime=now+maxTimeout.
- AutoPause=false + finite NewEndAt → only ShutdownTime.
- Empty NewEndAt + non-empty SetAnnotations → annotation written, timeouts cleared by `setTimeout(opts)` (the empty-time path).

### `SaveTimeoutWithPolicy.SetAnnotations` (`pkg/sandbox-manager/infra/sandboxcr/sandbox_test.go`)

- SetAnnotations with non-empty values writes them on the same `retryUpdate` round.
- SetAnnotations with empty-string value deletes the annotation key.
- shouldUpdate=false (policy degrade / no-op): SetAnnotations are NOT applied.
- Conflict-retry: a 409 in the middle of `retryUpdate` re-runs the modifier, applying SetAnnotations against the refreshed object.

### E2B handler annotation sync (`pkg/servers/e2b/create_test.go`, `pause_resume_test.go`, `timeout_test.go`)

- Create with `autoResume.enabled=true` + finite Timeout → annotation written as `"timeout:<seconds>s"`.
- Create with `autoResume.enabled=true` + `never-timeout=true` extension → annotation = `"timeout:never"`.
- Create with `autoResume=nil` → no annotation.
- Create with `autoResume.enabled=false` → no annotation.
- Connect on sandbox without annotation → no annotation added; `opts.SetAnnotations` empty.
- Connect on sandbox with annotation, paused→resumed branch, request timeout=600 → annotation = `"timeout:600s"` written via SetAnnotations.
- Connect on sandbox with annotation, running branch, request timeout extends → annotation updated.
- Connect on sandbox with annotation, running branch, request timeout shorter (extend-only no-op) → annotation unchanged.
- Connect on never-timeout sandbox (skip per ConnectOrWake guard) → annotation unchanged.
- SetTimeout on running sandbox without annotation → no annotation added.
- SetTimeout on running sandbox with annotation → annotation updated.
- SetTimeout on never-timeout sandbox → annotation unchanged.

### Gateway filter (`pkg/sandbox-gateway/filter/filter_test.go`, `wake/wake_test.go`)

- Wake gate matrix:
  - paused + Pausable=true + WakeOnTraffic="" → 502 (existing).
  - paused + Pausable=false + WakeOnTraffic non-empty → 502 (existing; never call manager).
  - non-paused + Pausable=true + WakeOnTraffic non-empty → 502 (existing).
  - paused + Pausable=true + WakeOnTraffic non-empty → wake.
- Paused + all three conditions met: returns `api.Running`; goroutine calls manager; on 200 + registry converges, calls Continue with new dynamic metadata.
- Manager 422 InvalidPolicy: SendLocalReply 503.
- Manager 409 Pausing: SendLocalReply 503 with `Retry-After: 5`.
- Manager 410 Gone: SendLocalReply 502 sandbox_not_found.
- Manager transport error: SendLocalReply 503.
- Concurrent in-replica wake (singleflight): two parallel requests to same paused sandbox produce one manager call; both receive Continue.
- OnDestroy after returning Running: filter context cancelled; in-flight wake goroutine sees ctx.Err() and exits; Continue/SendLocalReply not invoked on the destroyed stream.
- panic in goroutine: caught, mapped to SendLocalReply 500.
- Registry update arrives before manager response: still continues.
- No gateway-side wake timeout: assert that the goroutine does not impose its own deadline; only ctx cancellation (from OnDestroy) terminates it.

### Refresh handler (`pkg/sandbox-gateway/server/server_test.go`)

- /refresh paused: registry retains entry, State="paused".
- /refresh dead: registry removes entry.
- /refresh terminating: registry removes entry.

### Route mapping (`pkg/utils/sandbox-manager/proxyutils/default_test.go`)

- Sandbox with annotation: Route.WakeOnTraffic populated; Route.Pausable reflects `IsSandboxPausable(sbx)`.
- Sandbox without annotation: Route.WakeOnTraffic empty string.
- Sandbox in resuming/pending/terminating phase: Route.Pausable=false.
- JSON round-trip Route old→new: missing fields decode to zero values.
- JSON round-trip Route new→old: extra fields ignored.

### Integration

A single happy-path integration test under `test/e2e/` (or the project's existing integration suite):

- Bootstrap envtest with manager + gateway controller + sandbox controller stubs.
- Create sandbox with `autoResume.enabled=true`, `timeout=60s`.
- Pause via E2B handler.
- Send HTTP request through gateway filter.
- Assert: filter returns `api.Running`, eventually calls Continue; Sandbox CR has `Spec.Paused=false`; `Spec.ShutdownTime ≈ now + 5min` (clamped from 60s).
- Annotation remains `"timeout:60s"` (the original create value).

End-to-end SDK tests (real E2B SDK against a running cluster) are deferred to a follow-up.

## Open Decisions

- **Exact NetworkPolicy label selector** for sandbox-gateway pods. To be confirmed against the gateway Deployment's `metadata.labels` at implementation time. Whether a manager-side NetworkPolicy already exists (and we extend it) or whether a new NetworkPolicy is created is a deployment-shape detail to verify in `config/network-policy/`.
- **Manager internal Service shape**. Whether `:7789` is exposed by extending the existing `sandbox-manager` Service with a new port, or by creating a new dedicated Service, is a kustomize/Helm preference to be confirmed during implementation.
- **Wake metrics naming.** Manager-side wake metrics should align with the existing `sandbox{Resume,Pause,Delete}Responses` / `Duration` histograms in `pkg/sandbox-manager/metrics.go`. V1 includes a counter of wake responses by action code and a histogram of wake total duration. Gateway-side metrics for singleflight-coalesced calls and `wakeAndWait` duration are deferred to a follow-up because the sandbox-gateway currently exposes Envoy admin stats on `:9090`, while controller-runtime Go metrics are disabled to avoid port conflicts.
- **Future request-level auth on `/wake`.** Out of scope for v1 (network policy is sufficient given the trust model). If hostile co-tenancy or stricter compliance forces this later, the cleanest hook is an SA-token + TokenReview check on the manager handler; placement is at the entry of `pkg/proxy/wake_handler.go`. This spec does not pre-add the hook to keep the path symmetric with `/refresh`.
- **Future JWT/access-token owner check.** Tracking a separate workstream that maps an envd `X-Access-Token` to the sandbox owner. Not in this scope; no placeholder header is added to the wake path because such a header would only be meaningful once the verification machinery exists.
