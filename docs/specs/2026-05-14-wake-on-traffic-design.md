# Wake-on-Traffic Auto-Resume

## Context

Today, a paused sandbox returns to running only when an SDK explicitly issues
`/sandboxes/{id}/connect` or `/sandboxes/{id}/resume`. Data-plane traffic to a paused
sandbox is rejected by the sandbox-gateway filter with a 502 `sandbox_not_running`.
The gateway refresh handler (`pkg/sandbox-gateway/server/server.go:166-176`) removes
any non-Running route from the registry, so a paused entry can disappear from a
gateway replica between the manager push and the local informer write-back.

E2B's `POST /sandboxes` create API exposes `autoResume: SandboxAutoResumeConfig { enabled: bool }`,
and the [auto-resume documentation](https://e2b.dev/docs/sandbox/auto-resume) defines
the wake semantics: any HTTP traffic or SDK operation against a paused sandbox
transparently resumes it; on resume, the timeout restarts with a 5-minute minimum, or
the original duration if it was longer; the lifecycle configuration persists across
pause/resume cycles.

The project does not implement these semantics. Adding them requires producing a wake
trigger from the gateway data-plane filter, executing the resume in sandbox-manager,
restoring an appropriate timeout, and forwarding the original request to the
now-running pod.

The connect/resume rewrite (`4ec17604`) already provides every manager-side primitive
the wake path needs: `ConnectSandbox` resumes a paused sandbox atomically, floors a
too-short timeout to `--e2b-min-resume-timeout` (default `300s` = 5 minutes), and
applies the timeout with `ExtendOnly` so concurrent writers converge on the longest
deadline. The wake feature therefore reduces to: (a) let the gateway call
`ConnectSandbox` cross-owner under a restricted credential, (b) make paused routes
visible to the gateway, and (c) drive the call asynchronously from the data-plane
filter, keeping the original request in flight until the sandbox is Running.

## Goals

- Auto-resume any paused sandbox whose CR carries `agents.kruise.io/wake-on-traffic`
  when data-plane traffic arrives at the sandbox-gateway.
- Mirror E2B `autoResume` semantics: enabled at create time via the top-level
  `autoResume.enabled` wire field; resume restores the configured timeout from create
  or the last explicit set-timeout call. Connect does not rewrite that persisted
  auto-resume configuration. Finite resume timeouts are floored to
  `--e2b-min-resume-timeout` (default 5 minutes) by the existing `ConnectSandbox` path.
- Support a K8s-direct workflow: a user can patch the annotation on a Sandbox CR plus
  `spec.paused=true` and obtain wake-on-traffic without going through the E2B SDK.
- Reuse `ConnectSandbox` verbatim for the wake execution — no parallel resume/timeout
  code path in the manager.
- Introduce a narrowly-scoped, cross-owner **system credential** so the gateway can
  call `ConnectSandbox` without owner identity, while keeping the blast radius minimal
  (connect-only; no delete/pause/create).
- Cross-replica correct on the gateway side (multi-replica deployment); manager-side
  concurrency is handled entirely by the existing `ConnectSandbox` semantics.

## Non-goals

- **No CRD changes.** The wake policy is annotation-only.
- **No manager-side wake orchestration.** There is no `/wake` endpoint, no
  `WakeSandbox`, no `ConnectOrWake`, and no manager-side annotation parser. The gateway
  calls the existing `ConnectSandbox`.
- **No broadening of the admin key.** The admin key keeps its current behavior; a
  separate, connect-scoped system credential is introduced instead. The credential
  design (catalog, scopes, Secret population, rotation stance) lives in the
  [multi-system-key design](2026-06-02-multi-system-key-design.md).
- **The manager's built-in ext_proc (`pkg/proxy/ext_proc.go`) does not get
  wake-on-traffic.** Only the standalone sandbox-gateway supports wake. Deployments
  that route exclusively through the manager's native ext_proc proxy mode will not
  auto-resume paused sandboxes; they continue to receive the existing 502. This is an
  accepted limitation (no current demand).
- **No changes to sandbox-controller.** Auto-pause (`spec.PauseTime`) and auto-shutdown
  (`spec.ShutdownTime`) continue unchanged.
- **No idle-detection or activity-tracking.** "Wake on traffic" is wake-from-paused,
  not refresh-on-traffic; running sandboxes are forwarded without timeout side-effects.
- **No changes to normal API-key `ConnectSandbox` / `SetSandboxTimeout`
  resume/timeout semantics or HTTP status codes** other than (a) an additive
  wake-on-traffic annotation sync on `SetSandboxTimeout` only and (b) honoring a
  system caller's cross-owner access. The only status-code change is scoped to
  system-key `/connect`: retryable gateway wake errors return `409 Conflict`.
- **No async upgrade for non-wake filter paths.** The async pattern is added only on
  the wake branch of the gateway filter.

## Design

### Annotation protocol

A single annotation `agents.kruise.io/wake-on-traffic` carries the wake policy. Values
use a `<kind>:<payload>` form so future kinds can be added without breaking the parser.
Only one kind, `timeout`, is defined now.

```go
// api/v1alpha1/annotations.go
const AnnotationWakeOnTraffic = InternalPrefix + "wake-on-traffic"
```

The payload is **integer seconds**, not a `time.Duration` string, because the E2B
connect/set-timeout request model is `timeout` in integer seconds
(`models.SetTimeoutRequest.TimeoutSeconds int`). Storing seconds keeps the annotation,
the create/set-timeout writers, and the shared `pkg/utils/timeout` codec all on the same
integer type with no rounding ambiguity.

Value grammar:

| Value | Meaning |
|---|---|
| absent | wake-on-traffic disabled; gateway returns the existing 502 on paused traffic |
| `"timeout:never"` | enabled; the sandbox is never-timeout, so resume sets no deadline |
| `"timeout:<positive integer>"` (seconds, e.g. `"timeout:300"`, `"timeout:9000"`) | enabled; on wake the gateway asks `ConnectSandbox` to restore this many seconds (floored to `--e2b-min-resume-timeout`) |
| anything else | invalid; gateway treats it as not-enabled and falls back to 502 |

Parser rules (the grammar is encoded/decoded by a single shared codec in
`pkg/utils/timeout`, used by both the manager-side writers and the gateway-side reader;
see Code Impact):

- `never` is matched literally (case-sensitive); no surrounding whitespace tolerated.
- Otherwise `strconv.Atoi` the payload and require `> 0`. Reject non-numeric, `0`,
  negatives, and anything with a unit suffix (`"300s"`, `"5m"`, `"1500ms"`) or
  fractional value.
- Because every manager-side writer emits `timeout:<int>`, only hand-patched
  annotations can be malformed, and those simply disable wake (502).

`InternalPrefix = "agents.kruise.io/"` is on `BlackListPrefix`
(`pkg/servers/e2b/metadata.go:24`), so E2B users cannot inject this key through the
public `metadata` field; only the manager itself or a cluster admin via `kubectl` can
write it.

### E2B create wire mapping

`models.NewSandboxRequest` (`pkg/servers/e2b/models/sandbox.go`) gains the field
defined by E2B:

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

A pointer keeps "user did not send `autoResume` at all" distinguishable from
`{enabled: false}` and matches E2B's nullable shape. Both stock E2B Python and JS SDKs
flatten the SDK-side `lifecycle` wrapper to this wire shape before sending.

`basicSandboxCreateModifier` (`pkg/servers/e2b/create.go:277`) writes the annotation:

```go
if request.AutoResume != nil && request.AutoResume.Enabled {
    // FormatWakeOnTraffic encodes "timeout:never" when NeverTimeout, else
    // "timeout:<seconds>" — the only writer of the annotation grammar.
    sbx.SetAnnotation(v1alpha1.AnnotationWakeOnTraffic,
        timeout.FormatWakeOnTraffic(request.Extensions.NeverTimeout, request.Timeout))
}
```

`autoResume.enabled=true` requires `autoPause=true`. With `autoPause=false`, timeout
uses the auto-shutdown path and deletes the sandbox instead of preserving a paused
sandbox for later traffic wake, so the create request is rejected with `400 Bad Request`
when `autoResume.enabled=true` is sent without `autoPause=true`.

### Annotation duration sync on set-timeout

The annotation stores the sandbox's **configured auto-resume timeout in seconds**. It
is initialized by create and then updated only by an explicit set-timeout call. Connect
is deliberately excluded: this matches E2B's behavior, where connect may restore or
extend the live deadline for the current running session, but it does not rewrite the
persisted lifecycle / auto-resume configuration used by a later wake.

That distinction matters because pause rewrites the absolute deadline to `now + 1000y`.
The annotation is the durable configured duration that survives the pause. It should
change only when the user changes that configured duration through set-timeout, not on
ordinary connect/wake traffic.

`timeout.Options` gains a `SetAnnotations` field:

```go
// pkg/utils/timeout/types.go
type Options struct {
    ShutdownTime time.Time
    PauseTime    time.Time
    // SetAnnotations is applied to metadata.annotations in the same retryUpdate
    // round that writes ShutdownTime / PauseTime. Empty-string values delete the key.
    SetAnnotations map[string]string `json:"-"`
}
```

Inside `Sandbox.SaveTimeoutWithPolicy`'s modifier (`pkg/sandbox-manager/infra/sandboxcr/sandbox.go:236`),
the modifier computes two independent change decisions:

- `timeoutChanged` from the existing timeout policy (`Always` / `ExtendOnly`).
- `annotationsChanged` by comparing `opts.SetAnnotations` with the current annotation
  map, where an empty value means delete.

The modifier issues one Kubernetes `Update` when either decision is true. It calls
`setTimeout` only when `timeoutChanged`, and applies `opts.SetAnnotations` only when
`annotationsChanged`, in that same `retryUpdate` round. This keeps set-timeout and
annotation sync atomic while still allowing an explicit set-timeout request to repair a
stale wake annotation even if the absolute timeout fields already match.

`SetSandboxTimeout` is the only v1 handler that populates `SetAnnotations`. If the
sandbox already carries `wake-on-traffic`, it sets
`opts.SetAnnotations[v1alpha1.AnnotationWakeOnTraffic] = "timeout:<request.TimeoutSeconds>"`
and passes it through `SaveTimeoutWithPolicy`. The stored value is the **requested**
configured seconds, not a floored effective value. Flooring applies only to future
connect/wake resume deadlines.

Handlers never populate `SetAnnotations` when the sandbox carries no
`wake-on-traffic` annotation — they never silently enable wake-on-traffic.

| Handler / path | timeout policy | annotation outcome |
|---|---|---|
| `SetSandboxTimeout` (running) | `UpdatePolicyAlways` | annotation set to the requested seconds **exactly, including a shorter value** — set-timeout is an absolute set, so the configured duration (and the annotation) shrink with it |
| `ConnectSandbox` running (not paused) | `ExtendOnly` via `updateConnectTimeout` | annotation untouched, even if the live deadline extends |
| `ConnectSandbox` paused→resume (timed) | atomic Resume write + post-resume `ExtendOnly` | annotation untouched; the stored create/set-timeout value remains the source of truth for the next wake |
| `ResumeSandbox` (deprecated old-SDK path), timed | atomic Resume write + post-resume `ExtendOnly` | annotation untouched |
| any path, never-timeout sandbox | no timeout write (`buildResumeOpts` returns empty; `updateConnectTimeout` short-circuits on `currentEndAt.IsZero()`) | annotation untouched; it stays `"timeout:never"` |

`Pause` does not touch the annotation. Connect, resume, and gateway-driven wake do not
touch it either. A wake call goes through `ConnectSandbox` with the annotation's stored
seconds as the request timeout; that restores the live deadline for this resume cycle
but leaves the stored lifecycle configuration unchanged, matching E2B's behavior.


### System credential and system-caller handling

The gateway must call `ConnectSandbox` on sandboxes it does not own, without an
end-user identity. This is served by a **cross-owner, connect-scoped system
credential**. The credential design itself — the code-defined key catalog, per-key
scopes, the dedicated Secret and its population, and the `CheckApiKey` /
`AllowSystemKey` authorization flow — lives in
[Multiple System Keys With Per-Key Scopes](2026-06-02-multi-system-key-design.md). The
wake path relies on these properties:

- A connect-scoped system key exists, backed by a dedicated Secret
  `e2b-connect-system-key-store`, carrying `SystemAuthConnect` and marked cross-owner.
- The connect route opts in with `AllowSystemKey(SystemAuthConnect)`:

  ```go
  RegisterE2BRoute(sc.mux, http.MethodPost, "/sandboxes/{sandboxID}/connect",
      sc.ConnectSandbox, sc.AllowSystemKey(SystemAuthConnect), sc.CheckApiKey)
  ```

  A system key presented on any other route is rejected (default deny), so a
  compromised gateway can only trigger connect/resume — never delete, pause, create,
  or enumerate.

The two behaviors below are part of the wake execution path and are documented here;
the credential/recognition mechanism is in the design linked above.

**Cross-owner threading.** `GetClaimedSandboxOptions` carries `AllowAnyOwner bool`. The
connect handler sets it for a system caller, and `GetClaimedSandbox`
(`pkg/sandbox-manager/api.go:119`) skips its `sbx.GetRoute().Owner != user` check when it
is set. The system principal resolves to cluster scope (`getNamespaceOfUser` returns
`""`) like the admin team.

**No access token to system callers.** When the caller is the system principal,
`ConnectSandbox` returns the sandbox **without the envd access token** — it calls
`convertToE2BSandbox(sbx, "")` instead of `convertToE2BSandbox(sbx, utils.GetAccessToken(sbx))`.
The gateway only needs the sandbox resumed and routed; the per-sandbox access token is
never exposed to it, narrowing what a compromised gateway can learn.

**Gateway delivery.** The gateway reads the same `e2b-connect-system-key-store` Secret
**read-only** — it issues only `Get` and never creates or populates it; only the manager
populates it. It waits/retries until it observes a non-empty key before enabling wake
calls, then sends the value verbatim as `X-API-KEY` on its `ConnectSandbox` request.

### Gateway wake via authenticated `ConnectSandbox`

The sandbox-gateway gains a thin client that issues
`POST <managerE2B>/sandboxes/{id}/connect` with header `X-API-KEY: <system-key>` and a
body carrying the timeout parsed from the annotation:

- `timeout:<seconds>` → `timeoutSeconds = <seconds>`. `ConnectSandbox` floors it to
  `--e2b-min-resume-timeout` for the paused-with-deadline case and applies it
  `ExtendOnly`.
- `timeout:never` → the gateway sends the constant **`1`**. The sandbox is never-timeout
  (`currentEndAt.IsZero()`), so `ConnectSandbox` ignores the request timeout entirely
  (`buildResumeOpts` returns empty, `updateConnectTimeout` short-circuits). `1` is used
  rather than `0` because `ParseSetTimeoutRequest` rejects `<= 0`
  (`pkg/servers/e2b/timeout.go:96`); even under annotation/spec drift, `1s` floored and
  applied `ExtendOnly` can never shrink an existing later deadline.

`ConnectSandbox` returns 200 (already running) or 201 (resumed) on success. The gateway
treats both as success, then polls its local registry until the route is observed
`Running` before forwarding the original request.

**Manager-side connectivity.** The gateway needs the manager E2B API base URL (cluster
DNS / Service) and the system key (from `e2b-connect-system-key-store`). The gateway already
runs a controller-runtime manager with cluster access, so reading the Secret is
straightforward. These are new gateway config inputs (the gateway currently makes no
HTTP calls to the manager).

### Gateway Route extension

`proxyutils.Route` (`pkg/utils/proxyutils/route.go`) gains a single field:

- `WakeOnTraffic string` — the raw annotation value verbatim; the gateway decodes it via
  the shared `pkg/utils/timeout` codec on the wake path.

No `Pausable`/`Resumable` pre-filter field is added. Whether a paused-looking sandbox
is actually resumable depends on `Status.Phase` + conditions that change rapidly and
that the Route (a snapshot) cannot represent authoritatively; the gateway relies on
`ConnectSandbox`'s response (success vs. retryable conflict) instead of pre-filtering.

JSON encoding is forward-compatible: old peers ignore the unknown field on inbound; new
peers see a zero value when receiving from old peers.

`proxyutils.GetRouteFromSandbox` (`pkg/utils/proxyutils/route.go`) reads
`sandbox.Annotations[v1alpha1.AnnotationWakeOnTraffic]` and copies it to the Route, so
the field flows from the CR to every replica's local registry through both the local
informer (`gateway_controller.go`) and the manager's `SyncRouteWithPeers` push.

### Refresh handler relaxation

`pkg/sandbox-gateway/server/server.go:166-176` currently deletes any non-Running route.
With wake-on-traffic, paused entries must remain visible so the filter can find them.
The handler is relaxed to keep non-Dead routes:

```go
switch route.State {
case v1alpha1.SandboxStateDead, "":
    registry.GetRegistry().Delete(route.ID)
default:
    registry.GetRegistry().Update(route.ID, route)
}
```

This matches the local controller's path (`gateway_controller.go`), which already keeps
non-Running routes via `Update`.

### Gateway async filter

`pkg/sandbox-gateway/filter/filter.go` extends `DecodeHeaders` with an async branch on
the paused + wake-on-traffic path. The non-wake paths remain synchronous and unchanged.

```
DecodeHeaders:
  parse / lookup registry / extract sandboxID, port (existing)

  if route.State == Running:
    set dynamic metadata (existing)
    return api.Continue

  // Wake gate: paused AND wake-on-traffic configured. Any other combination
  // falls through to the existing 502 path.
  if !(route.State == Paused && route.WakeOnTraffic != ""):
    SendLocalReply 502 (existing)
    return api.LocalReply

  spawn goroutine (parented to filter context — cancelled on OnDestroy):
    defer recover() — turn panics into SendLocalReply 500
    err := wakeAndWait(filterCtx, sandboxID, route.WakeOnTraffic)
    f.completeOnce.Do(func() {
      if err != nil:
        SendLocalReply with mapped status
      else:
        re-read registry, set extraHeaders + dynamic metadata, Continue
    })
  return api.Running
```

Invariants:

- `f.completeOnce sync.Once` guards `Continue` and `SendLocalReply`. Calling either
  twice on the same stream panics inside Envoy.
- `OnDestroy` cancels the filter context and disables the `completeOnce` block,
  preventing callbacks on a destroyed stream.
- **No gateway-side wake deadline.** The wake retries as long as the client stays
  connected. The terminating signals are: (a) the client connection going away
  (OnDestroy → `filterCtx` cancellation), and (b) Envoy's listener-level
  `stream_idle_timeout` / route `timeout` in `config/sandbox-gateway/configmap.yaml`.
  Either cancels `filterCtx` and ends the retry loop.

**Per-replica coalescing without cross-request coupling.** The shared unit is a single
`ConnectSandbox` call, coalesced with `singleflight.DoChan` keyed by sandbox ID. The
coalesced function runs under a **detached context** (gateway-lifetime-scoped, with a
generous safety cap comfortably above the manager's per-attempt Resume wait — see
below), **not** any request's `filterCtx`. Each request goroutine then:

```
wakeAndWait(filterCtx, id, wakePolicy):
  timeoutSeconds = parse(wakePolicy)   // never → 1
  for {
    if filterCtx.Err() != nil: return filterCtx.Err()
    ch := sf.DoChan(id, func() (any, error) {
        return connectClient.Connect(detachedCtx, id, timeoutSeconds) // X-API-KEY: system key
    })
    select {
    case <-filterCtx.Done():
        return filterCtx.Err()                  // this request's client went away; others unaffected
    case res := <-ch:
        switch classify(res):
        case success (200/201):
            goto poll
        case retryable (409 conflict):
            backoff(filterCtx)                  // 50ms → 100ms → … → 1s cap, abort on ctx
            continue
        case terminal (any non-409 HTTP status or transport error):
            return mappedError
        }
    }
  }
poll:
  for !filterCtx.Done():
    if registry.Get(id).State == Running: return nil
    sleep backoff (50ms → 500ms cap)
  return filterCtx.Err()
```

Decoupling rationale: because the coalesced `Connect` uses a detached context, a leader
request disconnecting cannot cancel the shared call or poison still-connected followers;
because each request selects on its **own** `filterCtx`, a follower exits promptly on
its own `OnDestroy` without waiting on the shared result. The detached safety cap exists
only so the shared goroutine cannot leak if the manager hangs; it is sized comfortably
above the manager's per-attempt Resume bound (`resumeWaitMaxTimeout = 10 * time.Minute`,
`pkg/sandbox-manager/infra/sandboxcr/sandbox.go:340`, applied at `resumeTask.Wait`) so a
normal slow resume is never aborted by the cap.

**Why aggressive retry.** When `/connect` is authenticated by the system key,
retryable wake errors return a fast `409 Conflict`. This is intentionally scoped to
system callers: normal API-key `/connect` keeps the existing E2B-compatible status
mapping (for example, today's pause-in-flight conflict remains `400`). The gateway is
the only system-key `/connect` caller in v1, so it can treat `409` as the retry signal
without changing SDK-visible behavior. The common transient window is pause-in-flight /
not-yet-resumable: the controller completes the pause within seconds, after which the
sandbox is resumable. Retrying (rather than returning 503 to the client) makes the wake
transparent, which is the E2B contract. The retry is bounded by the client connection,
so a sandbox that is permanently non-resumable resolves when the client gives up or
Envoy times out the stream.

The gateway's retry rule is deliberately narrow: **only an HTTP `409` response from
the system-key `/connect` call is retried**. The manager is responsible for mapping
every gateway-retryable wake condition to `409`. Other HTTP statuses, including `5xx`,
are not treated as wake conflicts by the gateway; they are terminal for the current
request and surface through the existing 502 local-reply path. Transport failures do
not carry a `/connect` status code, so they are also terminal in v1 rather than a
second retry signal. If a later release wants infrastructure-level retries for manager
connectivity, that should be designed separately from wake-conflict retry semantics.

Error-to-behavior mapping at the gateway:

| `ConnectSandbox` result | gateway behavior |
|---|---|
| 200 / 201 | poll registry → Continue |
| 409 (system-key retryable wake conflict: resume in flight / not resumable yet / wait-task conflict) | retry with backoff (bounded by client connection) |
| 400 (invalid request, including malformed/too-large timeout if it ever reaches the manager) | stop → SendLocalReply 502 sandbox_not_running |
| 5xx | stop → SendLocalReply 502 sandbox_not_running |
| transport error / no HTTP response | stop → SendLocalReply 502 sandbox_not_running |
| 404 (sandbox not found / not claimed) | stop → SendLocalReply 502 sandbox_not_found |
| 401 / 403 (credential / scope error) | stop → SendLocalReply 502 sandbox_not_running |
| `filterCtx` cancelled (client disconnect / OnDestroy) | no reply emitted — Envoy already cleaning up |

The manager-side `/connect` handler distinguishes caller class before mapping resume
errors:

- Normal API-key caller: existing behavior is preserved, including the current
  `ErrorConflict → 400` mapping used by SDK-visible `/connect`.
- System-key caller: gateway-retryable wake errors map to `409 Conflict`. This includes
  `managererrors.ErrorConflict` returned by `Resume` for not-yet-resumable states and
  `cacheutils.ErrWaitTaskConflict` returned by the resume wait-task registration path.

The gateway sends a valid system key plus a timeout parsed from the annotation, so its
HTTP retry decision is exactly `status == 409`. `401/403` should not occur in steady
state (the system key carries the connect scope); mapping them to the existing 502
keeps the no-wake response shape stable for clients if a misconfiguration races.

### Network and config plumbing

- The gateway → manager E2B API call needs network reachability to the manager's E2B
  port. If existing NetworkPolicies do not already allow gateway → manager on that
  port, add an ingress allow rule (gateway label selector confirmed against
  `config/sandbox-gateway/deployment.yaml` at implementation).
- The gateway mounts / reads the `e2b-connect-system-key-store` Secret to obtain the system key.
- No new manager `:7789` route, no `/wake` NetworkPolicy, and no dedicated internal
  Service are introduced.

## State machine and concurrency

All resume + timeout + concurrency behavior is delegated to the existing
`ConnectSandbox` path; the gateway only decides whether to call it, retries on
transient conflicts, and waits for `Running`.

### Wake outcomes by sandbox state

The gateway's wake gate is `State == paused && WakeOnTraffic != ""`. `GetSandboxState`
collapses several CR shapes into the `paused` gateway state, and `ConnectSandbox`'s
outcome differs by the underlying `IsSandboxResumable` result.

| Observed CR shape | gateway state | ConnectSandbox outcome | gateway action |
|---|---|---|---|
| Phase=Running, Spec.Paused=false, Ready=true | running | n/a | forward directly (no wake) |
| Phase=Running, Spec.Paused=true (pause in flight) | paused | system-key `/connect`: `IsSandboxResumable=false "SandboxIsPausing"` → Resume `ErrorConflict` → **409** | retry until pause completes, then resumes |
| Phase=Paused, cond[SandboxPaused]=True | paused | resumable → Resume + ExtendOnly timeout → **201** | poll → Continue |
| Phase=Paused, cond[SandboxPaused]=False (pod terminating) | paused | system-key `/connect`: `IsSandboxResumable=false "SandboxIsPausing"` → **409** | retry until cond flips True, then resumes |
| Phase=Resuming | paused | resumable; resume idempotent / converges → 200/201 | poll → Continue |
| Phase=Pending / state=creating | creating | n/a (not paused) | existing 502 |
| state=dead / DeletionTimestamp / Failed/Succeeded/Terminating | dead | route deleted by refresh | existing 502 |
| not found / not claimed | n/a | **404** | SendLocalReply 502 sandbox_not_found |

The pause-in-flight rows are the reason for aggressive retry: the gateway sees `paused`,
system-key `ConnectSandbox` returns a fast `409`, and the window closes within seconds
as the controller finishes the pause (Phase=Paused, cond=True → resumable).

### Concurrent wake / connect on the same sandbox

- Two gateway replicas (or two requests across replicas) both call `ConnectSandbox`.
  `Resume` is atomic and first-writer-wins: only the update that flips `Spec.Paused`
  wins; losing callers short-circuit on the Ready condition and do not re-init. All
  callers then run the post-resume `ExtendOnly` write; the longest deadline survives.
- Within one replica, `singleflight.DoChan` coalesces concurrent connect calls (and
  each retry round) for the same sandbox into one manager call.
- Wake concurrent with an SDK connect: identical — both go through `ConnectSandbox`,
  both `ExtendOnly`, longest deadline wins.
- Never-timeout sandbox: `currentEndAt.IsZero()` short-circuits the timeout write for
  every caller; resume still proceeds. No deadline is ever written.

There is no `BaselineAware` reasoning and no cross-form race table any more — the
atomic-resume placeholder plus `ExtendOnly` is the single, already-tested mechanism.

### Cross-replica correctness

- Per-replica `singleflight` deduplicates within one replica. Across replicas, each
  independently calls `ConnectSandbox`; the manager serializes via atomic Resume.
- Informer staleness on the gateway can produce a transient `paused` Route after the
  manager has resumed. `ConnectSandbox` then returns 200 (already running); the gateway
  keeps polling its registry and converges once its informer catches up. Worst case is
  a ~1s delay, never an incorrect routing decision.

## Behavior under existing flows

### Auto-pause (controller-driven via `Spec.PauseTime`)

The controller flips `Spec.Paused = true` without writing any timeout. The wake
annotation, set at create / set-timeout, is preserved. When traffic later
arrives, the gateway calls `ConnectSandbox`, which resumes and applies the restored
timeout. Identical to manual-pause behavior from the wake perspective.

### Manual pause (E2B `/pause`)

Manual pause sets `Spec.Paused = true` and rewrites both `ShutdownTime` and `PauseTime`
to `now + 1000y`. The annotation is untouched. Wake calls `ConnectSandbox`: `Resume`
lifts `Spec.Paused` and writes the floored placeholder timeout atomically; the
post-resume `ExtendOnly` write applies `now + effective`. Because the placeholder
already replaced the `1000y` deadline during the atomic Resume, the post-resume write
is a normal extend.

### Never-timeout sandbox (`Extensions.NeverTimeout=true` at create)

`Spec.PauseTime` / `Spec.ShutdownTime` are nil at all times; `ParseTimeout` returns
`currentEndAt.IsZero() == true`. The gateway sends `timeout=1`, which `ConnectSandbox`
ignores via the never-timeout short-circuit; it resumes without writing a deadline. The
annotation, if present, is `"timeout:never"`.

### Wake on running sandbox (defensive)

The gateway only triggers wake when `route.State != Running`. If a race routes a wake
to an already-running sandbox, `ConnectSandbox` returns 200 with `ExtendOnly` (no
shrink); the gateway forwards normally.

## Code Impact

### New files

- The system-key module under `pkg/servers/e2b/keys/` that backs the connect credential
  is designed in the [multi-system-key design](2026-06-02-multi-system-key-design.md);
  the wake feature does not own it.
- `pkg/utils/timeout/wakeontraffic.go` — the shared wake-on-traffic annotation codec
  (`WakeOnTrafficConfig`, `FormatWakeOnTraffic`, `ParseWakeOnTraffic`): the single source of
  truth for the `timeout:<seconds>` / `timeout:never` grammar, used by the manager
  create / set-timeout writers and the gateway reader. Lives next to the timeout utilities
  to avoid a manager↔gateway package dependency and to keep the payload grammar with the
  timeout types.
- `pkg/sandbox-gateway/wake/wake.go` — `wakeAndWait`, the `singleflight.Group`, and the
  manager E2B HTTP client (system-key header). The annotation grammar is decoded via the
  shared `pkg/utils/timeout` codec, not a gateway-local parser; `timeout:never` maps to a
  named placeholder (`neverWakeConnectTimeoutSeconds = 1`).
- `pkg/sandbox-gateway/filter/async.go` — async `DecodeHeaders` glue, `completeOnce`,
  `OnDestroy` integration.

### Modified files

- `api/v1alpha1/annotations.go` — `AnnotationWakeOnTraffic` constant.
- `pkg/servers/e2b/models/sandbox.go` — `AutoResume *AutoResumeConfig` + `AutoResumeConfig` type.
- `pkg/servers/e2b/create.go` — `basicSandboxCreateModifier` writes the annotation
  (`timeout:<seconds>` / `timeout:never`).
- `pkg/utils/timeout/types.go` — `Options.SetAnnotations map[string]string` field.
- `pkg/sandbox-manager/infra/sandboxcr/sandbox.go` — `SaveTimeoutWithPolicy` modifier
  applies `opts.SetAnnotations` in the same `retryUpdate` round as `setTimeout`; the
  modifier writes when either timeout fields or requested annotations differ, so
  set-timeout syncs the annotation atomically and can repair stale annotation state.
- `pkg/servers/e2b/pause_resume.go` — `ConnectSandbox` / `ResumeSandbox` do **not** build
  or pass `SetAnnotations`; connect/resume leaves the wake-on-traffic annotation at the
  create or last set-timeout value. `ConnectSandbox` honors a system caller's
  `AllowAnyOwner` and returns the sandbox without the envd access token for system callers
  (`convertToE2BSandbox(sbx, "")`). Normal API-key callers keep the existing `/connect`
  error mapping. System-key callers map gateway-retryable wake errors
  (`managererrors.ErrorConflict` from `Resume` and `cacheutils.ErrWaitTaskConflict`) to
  `409 Conflict`, giving the gateway a clean retry signal without changing SDK-visible
  behavior.
- `pkg/servers/e2b/timeout.go` — `SetSandboxTimeout` builds the `SetAnnotations` map
  (`UpdatePolicyAlways`, so a shorter value also rewrites the annotation) when the
  annotation is present.
- `pkg/servers/e2b/sandbox.go` — `getSandboxOfUser` threads `AllowAnyOwner` for system
  callers.
- `pkg/sandbox-manager/infra/interface.go` — `GetClaimedSandboxOptions.AllowAnyOwner bool`.
- `pkg/sandbox-manager/api.go` — `GetClaimedSandbox` skips the owner check when
  `AllowAnyOwner` is set.
- `pkg/servers/e2b/routes.go` — the connect route registers
  `AllowSystemKey(SystemAuthConnect)`. The `AllowSystemKey` middleware and `CheckApiKey`
  system-key recognition / scope enforcement are designed in the
  [multi-system-key design](2026-06-02-multi-system-key-design.md).
- `pkg/servers/e2b/core.go` (+ `cmd/sandbox-manager/main.go`) — wire the system-key
  module so `CheckApiKey` can recognize the connect credential; see the
  [multi-system-key design](2026-06-02-multi-system-key-design.md).
- `pkg/servers/e2b/models/api_key.go` — `SystemAuth` scope type, `SystemAuthConnect`,
  `SystemKeyID`, and the system principal synthesis helper; see the
  [multi-system-key design](2026-06-02-multi-system-key-design.md).
- `pkg/utils/proxyutils/route.go` — `Route.WakeOnTraffic`; `GetRouteFromSandbox`
  populates it.
- `pkg/sandbox-gateway/server/server.go` — `handleRefresh` keeps non-Dead routes.
- `pkg/sandbox-gateway/filter/filter.go` — async wake branch with the wake gate.
- `pkg/sandbox-gateway/filter/config.go` — manager E2B base URL + system-key source.
- `cmd/sandbox-gateway/main.go` — init the wake module / config.
- `config/sandbox-manager/secret.yaml`, `config/sandbox-manager/rbac.yaml` — the
  pre-created connect-credential Secret, its population, and the manager's `get`/`update`
  RBAC on it are covered by the
  [multi-system-key design](2026-06-02-multi-system-key-design.md).
- `config/sandbox-gateway/rbac.yaml` — add a namespaced Role + RoleBinding (in
  `sandbox-system`) granting the gateway ServiceAccount `get` on the
  `e2b-connect-system-key-store` Secret (`resourceNames`-restricted; v1 reads it via a direct Get
  at startup). The gateway's existing ClusterRole keeps only Sandbox/Pod read access.
- `config/sandbox-gateway/deployment.yaml` — manager E2B base URL config (and the system
  key is read from the Secret via the API, no mount required for v1).
- `config/network-policy/...` — allow gateway → manager E2B port if not already allowed.

### Removed relative to the original design

- `/wake/{sandboxID}` endpoint and `pkg/proxy/wake_handler.go`.
- `pkg/sandbox-manager/wake.go` (`WakeSandbox`) and `pkg/sandbox-manager/wakeontraffic.go`
  (manager-side `ParseWakeOnTrafficPolicy`).
- `pkg/sandbox-manager/connect_core.go` (`ConnectOrWake` / `ConnectOrWakeInput`).
- The `wakeMinimumTimeout` manager constant (reuse `--e2b-min-resume-timeout`).
- The manager `maxTimeout` field addition, the `Route.Pausable` field, the dedicated
  `/wake` NetworkPolicy, and the `:7789` internal Service.

### Not modified

- `api/v1alpha1/sandbox_types.go`, `config/crd/...` — no CRD changes.
- `pkg/controller/sandbox/...` — no controller behavior changes.
- `pkg/proxy/ext_proc.go` — the manager's built-in ext_proc does not get wake (non-goal).
- Normal API-key `ConnectSandbox` / `SetSandboxTimeout` HTTP status codes — unchanged
  (E2B `/connect` documents 200/201/400/401/404/500; normal conflict stays `400`).
  The system-key `/connect` retry signal is the only scoped exception.

## Test Plan

### Annotation codec (`pkg/utils/timeout/wakeontraffic_test.go`)

- Disabled: annotation absent.
- Never: exact `"timeout:never"`.
- Seconds form: `"timeout:1"`, `"timeout:30"`, `"timeout:300"`, `"timeout:9000"`.
- Invalid (treated as not-enabled): `""`, `"true"`, `"timeout:"`, `"timeout:0"`,
  `"timeout:-1"`, `"timeout:300s"`, `"timeout:5m"`, `"timeout:1500ms"`, `"timeout:1.5"`,
  `"timeout:abc"`, `"timeout:Never"`, `"timeout: 300"`, `"foo:300"`, `"timeout"` (no colon).

### System credential and system-key module

The system-key authorization tests (per-key scope acceptance, the two 403 paths, system
principal synthesis, blank-key rejection) and the system-key module lifecycle tests
(Secret populate / adopt / fail-closed / concurrent-replica convergence) live with the
[multi-system-key design](2026-06-02-multi-system-key-design.md). The wake-specific
`/connect` behaviors — system-key caller cross-owner access (below), the `409` retry
mapping, and no-access-token-to-system-callers (in the handler tests) — are covered here.

### `GetClaimedSandbox` AllowAnyOwner (`pkg/sandbox-manager/api_test.go`)

- `AllowAnyOwner=false`, owner mismatch → ErrorNotAllowed (unchanged).
- `AllowAnyOwner=true`, owner mismatch → returns the sandbox.
- `AllowAnyOwner=true`, sandbox unhealthy/unclaimed → still rejected for the right reason.

### `SaveTimeoutWithPolicy.SetAnnotations` (`pkg/sandbox-manager/infra/sandboxcr/sandbox_test.go`)

- Non-empty values written on the same `retryUpdate` round.
- `Always` policy, requested timeout shorter than current: timeout AND annotation both
  rewritten to the shorter value.
- Timeout policy no-op but `SetAnnotations` differs: annotation updated, timeout fields
  left untouched.
- Empty-string value deletes the annotation key.
- Timeout policy no-op and `SetAnnotations` already matches: no Update issued.
- Conflict-retry: a 409 mid-`retryUpdate` re-runs the modifier and applies
  SetAnnotations against the refreshed object.

### E2B handler annotation sync (`create_test.go`, `pause_resume_test.go`, `timeout_test.go`)

- Create `autoPause=true` + `autoResume.enabled=true` + finite Timeout → annotation `"timeout:<seconds>"`.
- Create `autoPause=true` + `autoResume.enabled=true` + `never-timeout` extension → `"timeout:never"`.
- Create `autoResume.enabled=true` without `autoPause=true` → `400 Bad Request`.
- Create `autoResume=nil` / `enabled=false` → no annotation.
- Connect path never populates `SetAnnotations` (with or without the wake annotation).
- Connect with annotation, paused→resumed, timeout=600 → timeout is applied/restored,
  annotation remains at the create or last set-timeout value.
- Connect with annotation, paused→resumed, timeout=60 (below `--e2b-min-resume-timeout`)
  → deadline floored to 300s, annotation unchanged.
- Connect with a normal API key and a not-yet-resumable paused sandbox keeps the existing
  `400` status; the system-key caller for the same retryable wake condition gets `409`.
- Connect with a system key and `cacheutils.ErrWaitTaskConflict` during resume wait-task
  registration gets `409`.
- Connect with annotation, running, timeout extends → annotation unchanged.
- Connect with annotation, running, timeout shorter (ExtendOnly no-op) → unchanged.
- Connect on never-timeout sandbox → annotation unchanged.
- SetTimeout with annotation, longer → annotation set to requested seconds; **shorter →
  annotation rewritten shorter** (`UpdatePolicyAlways`); without annotation → not added;
  never-timeout → unchanged.

### Gateway filter (`pkg/sandbox-gateway/filter/filter_test.go`, `wake/wake_test.go`)

- Wake gate: paused + WakeOnTraffic="" → 502; non-paused + WakeOnTraffic set → 502;
  paused + WakeOnTraffic set → wake.
- Paused + gate met: returns `api.Running`; goroutine calls connect; on 200/201 +
  registry converges → Continue with new dynamic metadata.
- Connect 409 (system-key retryable wake conflict): retries; succeeds once a later
  attempt returns 200/201.
- Connect 400: treated as terminal and mapped to 502 sandbox_not_running.
- Connect 5xx: treated as terminal and mapped to 502 sandbox_not_running.
- Connect transport error / no HTTP response: treated as terminal and mapped to 502
  sandbox_not_running.
- Connect 404: SendLocalReply 502 sandbox_not_found (no retry).
- Connect 401/403: SendLocalReply 502 sandbox_not_running (no retry).
- Concurrent in-replica wake (DoChan): two parallel requests → one connect call per
  round; both Continue.
- Leader disconnect mid-call does not cancel the shared connect nor poison a
  still-connected follower (the follower's own ctx governs it).
- OnDestroy after returning Running: filter ctx cancelled; goroutine sees ctx.Err()
  and exits; no Continue/SendLocalReply on the destroyed stream.
- Panic in goroutine: caught, mapped to SendLocalReply 500.
- No gateway-side wake deadline: the goroutine imposes no deadline of its own; only
  `filterCtx` cancellation terminates it.

### Refresh handler (`pkg/sandbox-gateway/server/server_test.go`)

- /refresh paused: registry retains entry, State="paused".
- /refresh dead / terminating: registry removes entry.

### Route mapping (`pkg/utils/proxyutils/route_test.go`)

- Sandbox with annotation: `Route.WakeOnTraffic` populated.
- Sandbox without annotation: `Route.WakeOnTraffic` empty.
- JSON round-trip old→new (missing field → zero) and new→old (extra field ignored).

### Integration

A single happy-path integration test under the existing suite:

- Bootstrap manager + gateway controller + sandbox controller stubs.
- Create sandbox with `autoPause=true`, `autoResume.enabled=true`, `timeout=60`.
- Pause via E2B handler.
- Send a request through the gateway filter.
- Assert: filter returns `api.Running`, eventually Continue; Sandbox CR has
  `Spec.Paused=false`; `Spec.ShutdownTime ≈ now + 5min` (floored from 60s by
  `--e2b-min-resume-timeout`); annotation remains `"timeout:60"`.

End-to-end SDK tests (real E2B SDK against a running cluster) are deferred to a
follow-up.

## Open Decisions

- **Manager E2B base URL delivery to the gateway** — env var vs. config field vs.
  service discovery. Confirm against `config/sandbox-gateway/deployment.yaml`.
- **`e2b-connect-system-key-store` Secret shape** — single plaintext key entry in
  `sandbox-system`; v1 settles on a direct name-restricted API read from the gateway (no
  mount). Confirm the exact data key name at implementation.
- **NetworkPolicy** — whether gateway → manager E2B port is already allowed or needs a
  new rule; confirm the gateway label selector at implementation.
- **Detached safety cap value** — sized comfortably above the manager's per-attempt
  Resume bound (`resumeWaitMaxTimeout = 10 * time.Minute`); confirm the exact value
  (e.g. ~11–12 min).
- **Wake metrics** — gateway-side counters for coalesced calls and retry rounds are
  deferred (the gateway exposes Envoy admin stats on `:9090` while controller-runtime
  Go metrics are disabled to avoid a port conflict).
- **System-key scope narrowing.** The connect credential resolves to cluster scope
  (connect-only, no access token, so the blast radius is "wake any sandbox"). Any future
  narrowing of the principal is tracked in the
  [multi-system-key design](2026-06-02-multi-system-key-design.md). Out of scope for wake v1.
