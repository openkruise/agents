# E2B Code Path Analysis: Create Sandbox

## Metadata
- Scenario: Create sandbox (claim path + clone path, all 6 brainstorm dimensions, with special focus on CSI mount failures)
- E2B Python API (verified via context7): `Sandbox.create(template=None, timeout=None, metadata=None, envs=None, secure=True, allow_internet_access=True, mcp=None, network=None, lifecycle=None, volume_mounts=None, **opts)`
- HTTP endpoint / route: `POST /sandboxes` (dual-registered: native + `<CustomPrefix>/api/sandboxes`), middleware `CheckApiKey` only (no `{sandboxID}`, so owner-route check is skipped)
- Handler / implementation file: `pkg/servers/e2b/create.go` → `CreateSandbox` → `createSandboxWithClaim` / `createSandboxWithClone`
- sandbox-manager commit: `8e1fad2b`
- Analyst / date: code-path analysis, 2026-05-29

---

## 1. E2B Contract (verified)

- Python call example:
  ```python
  from e2b import Sandbox
  sbx = Sandbox.create(template="base", timeout=300, metadata={"k": "v"}, secure=True)
  ```
- Parameters (defaults / required):
  - `template` — template name/ID; defaults to `base`. Maps to HTTP body `templateID`.
  - `timeout` — seconds, **default 300**; max 86400 (Pro) / 3600 (Hobby).
  - `metadata`, `envs` — maps; optional.
  - `secure` — **default True** (envd guarded by access token).
  - `lifecycle` — `{on_timeout: "kill"|"pause", auto_resume}`, default `on_timeout="kill"`.
- Expected behavior & return: creates a sandbox and returns a `Sandbox` handle; HTTP `POST /sandboxes` returns the created sandbox descriptor (this server returns **201 Created** with the `models.Sandbox` body).
- Documented errors: bad request (invalid template/params), auth errors. SDK assumes 4xx for client errors, 5xx for server errors (relevant to status-code paths below).
- Source (context7 library id): `/e2b-dev/e2b` (High reputation). HTTP body shape is authoritatively defined by this server's `models.NewSandboxRequest`.

> **Contract-drift note**: the current E2B SDK expresses pause/auto-resume through `lifecycle.on_timeout`, while this server's `NewSandboxRequest` reads a top-level `autoPause bool`. The mapping between the SDK's `lifecycle` and this server's `autoPause` is an implicit compatibility contract and is a standing High-risk item.

---

## 2. Implementation Overview

### Request flow (middleware → handler)

```
POST /sandboxes
  └─ CheckApiKey               (auth only; create has no {sandboxID} → no owner-route check)
       └─ CreateSandbox
            ├─ user == nil                                 → 401
            ├─ parseCreateSandboxRequest
            │     ├─ json.Decode(body)        decode err   → ApiError{Code:0} → HTTP 500
            │     ├─ ParseExtensions()        bad ext       → 400
            │     ├─ metadata key validation  bad key       → 400
            │     └─ timeout default(300)/bounds[30,maxTimeout]  → 400
            ├─ getNamespaceOfUser(user)       (admin → "", else team name = namespace)
            ├─ HasTemplate(ns, templateID)?   → createSandboxWithClaim  → ClaimSandbox
            ├─ else HasCheckpoint(ns, id)?    → createSandboxWithClone  → CloneSandbox
            └─ else                                          → 400 "Template or Checkpoint not found"
```

### Claim internals (`TryClaimSandbox`, wrapped by `infra.ClaimSandbox` retry loop over `ClaimTimeout`)

```
acquire claim worker (claimLockChannel, bounded by MaxClaimWorkers; ctx-cancel → fail)
 └─ pickAnAvailableSandbox(pickCache, cache, createLimiter)
       ├─ pool empty + CreateOnNoStock → newSandboxFromSandboxSet (createLimiter.Allow(); deny → NoAvailableError[retriable])
       ├─ pool empty + !CreateOnNoStock → NoAvailableError[retriable]
       └─ pick available (needs PodIP) or speculate-creating
 └─ modifyPickedSandbox (Modifier: timeout/labels/annotations + InitRuntime annotation + CSI annotation; inplace image/resources)
 └─ performLockSandbox (CR update; apierrors.IsConflict → retriableError + RV expectation)
 └─ if create/speculate/inplace → waitForSandboxReady (scheduler + kubelet)
 └─ if InitRuntime != nil → runtime.InitRuntime (envd via connectrpc; fail → retriable)
 └─ if SecurityIdentityProvider gate → issue + record + propagate token (fail → retriable)
 └─ if CSIMount != nil → ProcessCSIMounts  ── FAILURE IS NOT WRAPPED → NOT RETRIED → immediate fail
 └─ defer clearFailedSandbox(err): reserve via timeout (>0 / forever) OR Kill (==0)
on success: metrics + syncRoute(peers)  (gossip; failure logged & ignored)
```

### Clone internals (`CloneSandbox`, wrapped by `infra.CloneSandbox` retry loop over `CloneTimeout`)

```
findCheckpointAndTemplateById (cache→APIReader fallback; CacheBackoff retry)   fail → 500
 └─ waitCloneCreateLimiter (createLimiter.Wait(ctx) — BLOCKING, unlike claim's Allow())
 └─ createSandboxFromCheckpoint (GenerateName) ── retriable; ORPHAN-AMPLIFICATION risk on client-timeout-after-persist
 └─ cloneWaitSandboxReady       (retriable)
 └─ cloneReInitRuntime          (retriable)
 └─ CSI: if opts.CSIMount==nil → ResolveCSIMountFromAnnotation (fail → NOT retried)
         ProcessCSIMounts                                       (fail → NOT retried)
 └─ defer clearFailedSandbox(err)
on success: syncRoute(peers)
```

### External components crossed (outside trust boundary)
- **API Server / informer cache**: `ListSandboxesInPool`, `PickSandboxSet`, `GetCheckpoint`, template `Get` (cache, can be stale); CR create/update/patch (conflict / latency / 5xx); Namespace `Get` (key-creation only).
- **Scheduler + kubelet**: `waitForSandboxReady` / `cloneWaitSandboxReady` (pod scheduling + readiness).
- **envd runtime (in pod, connectrpc)**: `InitRuntime` / `cloneReInitRuntime`, `ProcessCSIMounts`.
- **Identity provider** (feature-gated): security token issuance/propagation.
- **peers / memberlist**: `syncRoute` gossip (failure swallowed).
- **CSI driver / cloud storage**: `ProcessCSIMounts`, PV/storage resolution.

### Shared mutable state
- **In-process (per replica)**: `pickCache` (sync.Map), `claimLockChannel` (bounded worker pool), `createLimiter` (per-replica rate limiter, shared by claim+clone), expectations (`ResourceVersionExpectation`).
- **Cross-replica (only K8s + gossip are shared)**: SandboxSet pool, CR optimistic lock (resourceVersion), per-replica route table reconciled via gossip + `reconcileRoute`.

### Status-code mechanics (`pkg/servers/web/framework.go`)
`writeJson`: when `ApiError.Code == 0`, it falls back to the handler `defaultCode = http.StatusInternalServerError`. Therefore **every `&web.ApiError{Message: ...}` with no explicit `Code` returns HTTP 500**, including: JSON decode errors, all `ClaimSandbox` failures, all `CloneSandbox` failures. Handler panics are caught by a deferred `recover` and return a generic 500.

---

## 3. Confirmed Scenario Set

| # | Scenario | Dimension | Notes |
|---|----------|-----------|-------|
| 1 | claim/clone success & all branch errors | single-request | full branch walk |
| 2 | single-endpoint high concurrency (same/empty pool, no-stock storm, clone storm) | single-endpoint concurrency | pickCache / workers / limiter |
| 3 | multi-endpoint races (template/checkpoint delete, scale-down, connect-after-create) | multi-endpoint concurrency | TOCTOU + route lag |
| 4 | hostile/boundary params (bad JSON, timeout bounds, metadata, extensions, secure/autoPause) | parameter combinations | distrust all input |
| 5 | degraded cluster (cache stale, CR conflict, 5xx, slow scheduler, envd down, identity down, **CSI high failure rate**) | cluster/env variation | every external dep perturbed |
| 6 | other-endpoint side effects (TTL expiry, clearFailedSandbox vs reconcile, cross-tenant limiter coupling) | side effects | shared-state mutation |
| 7 | delayed/timing (create→connect gossip lag, clone orphan on client timeout, limiter token replenishment) | delayed/timing | propagation + timeout gaps |

Confirmed by user: cover all dimensions; include clone path; **CSI mount failure rate is high → special focus.**

---

## 4. Execution Paths

### 4.1 Single-request paths

| Path ID | Trigger condition | Deps crossed | Result / side effect | Recoverability |
|---------|-------------------|--------------|----------------------|----------------|
| **P1** | `user == nil` in ctx | none | 401 | client/auth fix |
| **P2** | malformed/empty/oversized JSON body | none | `ApiError{Code:0}` → **HTTP 500** (should be 400) | client fix; 5xx may trigger SDK retries on a bad request |
| **P3** | bad extension value (claim-timeout non-int, negative reserve-for, bad image, bad CPU qty, bad CSI mountPath, single-CSI volume/mountpoint mismatch, bad label) | none | 400 "Bad extension param" | client fix |
| **P4** | metadata key fails `IsQualifiedName` | none | 400 | client fix |
| **P5** | metadata key has blacklisted prefix | none | 400 | client fix |
| **P6** | `timeout < 30` or `> maxTimeout` (0 → default 300) | none | 400 "timeout should between…" | client fix |
| **P7** | `HasTemplate=false && HasCheckpoint=false` | API Server (cache reads) | 400 "Template or Checkpoint not found" | **false 400 possible on stale cache** |
| **P8** | claim: `CSIMountOptionsConfig` resolution fails (driver/PV/storage) | API Server, storage registry | 400 | client/env fix · **CSI** |
| **P9** | claim success | API Server, scheduler/kubelet, envd, identity, CSI | **201** + body + access token; CR locked/claimed; route synced | success |
| **P10a** | no-stock + `!CreateOnNoStock`, retried until `ClaimTimeout` | cache | **500** | self-heals if pool scales within window |
| **P10b** | no-stock + `CreateOnNoStock` + `createLimiter` sustained denial until `ClaimTimeout` | cache, limiter | **500** | retry; throttle-driven |
| **P10c** | `performLockSandbox` resourceVersion conflict, retries exhausted | API Server (CR update) | **500** | retryable; cross-replica contention |
| **P10d** | `waitForSandboxReady` timeout (create/speculate/inplace) | scheduler, kubelet | **500** + `clearFailedSandbox` | env-dependent |
| **P10e** | `InitRuntime` fails (envd unreachable), retries exhausted | envd runtime | **500** + `clearFailedSandbox` | runtime-dependent |
| **P10f** | security token issue/record/propagate fails (gate on), exhausted | identity provider | **500** + `clearFailedSandbox` | provider-dependent |
| **P10g** | **claim CSI `ProcessCSIMounts` fails** (post-lock, post-ready, post-init) — NOT retried | CSI driver, envd | **immediate 500** + `clearFailedSandbox`; a fully-prepared sandbox is reserved/killed | **CSI · high churn given high failure rate** |
| **P10h** | manager `ClaimSandbox` template recheck `HasTemplate=false` (template deleted between handler check & recheck) | cache | **500** (semantically not-found) | TOCTOU |
| **P10i** | `ClaimTimeout` / client disconnect mid-claim | ctx | **500**, possible partial/locked sandbox → `clearFailedSandbox` | cleanup may itself fail |
| **P11** | clone: `InplaceUpdate.Image != ""` | none | 400 "InplaceUpdate is not supported for clone" | client fix |
| **P12** | clone: CSI driver resolution fails (pre-clone) | API Server, storage registry | 400 | **CSI** |
| **P13** | clone success | cache+APIReader, limiter (blocking), scheduler/kubelet, envd, CSI | **201** + body + token from sandbox | success |
| **P14a** | clone: checkpoint/template not found after CacheBackoff | cache, APIReader | **500** | stale cache / deleted checkpoint |
| **P14b** | clone: `createLimiter.Wait(ctx)` interrupted by `CloneTimeout` | limiter, ctx | **500** | retry |
| **P14c** | clone: `createSandboxFromCheckpoint` fails — retriable; **client-timeout-after-persist** | API Server (GenerateName CR) | **500**; **orphan CR (no IsAlreadyExists signal) → leak** | **needs out-of-band janitor; not self-healing otherwise** |
| **P14d** | clone: `cloneWaitSandboxReady` timeout | scheduler, kubelet | **500** + `clearFailedSandbox` | env-dependent |
| **P14e** | clone: `cloneReInitRuntime` fails | envd runtime | **500** + `clearFailedSandbox` | runtime-dependent |
| **P14f** | clone: `ResolveCSIMountFromAnnotation` fails (when `opts.CSIMount==nil`) — NOT retried | API Server, storage registry | **immediate 500** | **CSI** |
| **P14g** | clone: `ProcessCSIMounts` fails — NOT retried | CSI driver, envd | **immediate 500** + `clearFailedSandbox` | **CSI · high churn** |
| **P15** | clone silently drops `secure`, `InplaceUpdate.Resources`, `skip-init-runtime` (clone uses `GetAccessToken(sbx)`, no fresh-token-on-secure) | none | **201** but client-requested `secure` may yield no/empty token (insecure) | silent semantic gap |
| **P16** | any panic (nil-deref, etc.) | web recover | **500** "Internal Server Error" (generic, masks cause) | replica survives |

### 4.2 Same-replica concurrent paths

| Path ID | Shared state / TOCTOU | Concern | Result |
|---------|----------------------|---------|--------|
| **C1** | `pickCache` (sync.Map) | two concurrent claims, same template — TOCTOU between pick and pickCache mark | ideally distinct picks; worst case same pick → one lock conflict → retry |
| **C2** | `claimLockChannel` (MaxClaimWorkers) | saturation under burst; excess claims block; ctx-cancel while waiting | latency / 500 (P10i) |
| **C3** | `createLimiter` | claim `Allow()` (fail-fast retry) vs clone `Wait()` (blocking) compete on the **same** limiter | starvation/unfairness between claim and clone under no-stock load |
| **C4** | expectations (RV) | concurrent locks set RV expectations; stale cache entries skipped | extra retries; eventual fresh read |
| **C5** | `ProcessCSIMounts` semaphore | N mounts run concurrently; **partial failure → `errors.Join` fails whole** while some mounts already applied | partial mount state on a sandbox that is then reserved/killed · **CSI** |

### 4.3 Cross-replica concurrent paths

| Path ID | Mechanism | Concern | Result |
|---------|-----------|---------|--------|
| **X1** | per-replica `pickCache` independent; **K8s optimistic lock is the only true serializer** | two replicas pick the same available sandbox → `performLockSandbox` conflict; one wins, others 409 → retriable | **must serialize**; if exclusivity ever breaks → two clients share one sandbox (cross-tenant) |
| **X2** | `createLimiter` is **per replica** | global pod-create QPS = N_replicas × MaxCreateQPS; no-stock storm across replicas | API Server / scheduler overload; multiplies on scale-up (ops) |
| **X3** | `syncRoute` gossip lag after create | follow-up connect/describe/pause on a replica without the route → `GetOwnerOfSandbox` miss | **404 "Sandbox route not found" after a 201** (read-after-write across replicas) |
| **X4** | `syncRoute` failure swallowed (logged only) | peer partition → other replicas only learn route via `reconcileRoute` backfill | extended 404 window cross-replica |
| **X5** | two replicas both `CreateOnNoStock` from same SandboxSet | both create new sandboxes (GenerateName) | pool over-provision; cost; reconciled later |

### 4.4 Side effects from other endpoints

| Path ID | Other endpoint / actor | Effect on create |
|---------|------------------------|------------------|
| **O1** | `DeleteTemplate` / SandboxSet scale-down during claim | `HasTemplate` true at handler, gone at lock/recheck → P10h 500, or pool drained → P10a 500 (TOCTOU) |
| **O2** | `DeleteCheckpoint` (or its template) during clone | `findCheckpointAndTemplate` ok, then deleted → create-from-missing-template → P14a/P14c |
| **O3** | TTL/timeout controller during create | `basicSandboxCreateModifier` sets ShutdownTime; a short/stale timeout could shut down right after claim |
| **O4** | controller reconcile vs `clearFailedSandbox` | on failure, Kill/reserve races SandboxSet replenishment → transient inconsistency |
| **O5** | any other team's create traffic | shared per-replica `createLimiter` + `claimLockChannel` → **one tenant's no-stock storm throttles another tenant's creates** (noisy neighbor) |

### 4.5 Delayed / timing paths

| Path ID | Timing gate | Concern |
|---------|-------------|---------|
| **D1** | create → immediate connect | gossip not yet propagated → 404 window (== X3, timing-gated) |
| **D2** | post-201 state change | 201 is returned only after waitReady+init+CSI succeed (same-replica read-after-write is consistent); cross-replica gap is X3 |
| **D3** | `timeout = 30` (min) then delayed connect | sandbox pauses/kills before first use (autoPause behavior) |
| **D4** | client timeout < `ClaimTimeout` | ctx cancel → `clearFailedSandbox`; locked sandbox reserved/killed; orphan if cleanup fails |
| **D5** | **clone client-timeout-after-persist** | retry creates a 2nd CR (GenerateName) → **orphan leak**; needs janitor (== P14c) |
| **D6** | `createLimiter` token replenishment | burst succeeds, then throttled to rate; if `ClaimTimeout` < time-to-token → 500 |

---

## 5. Scoring & Risk

Importance adjusted for recoverability (self-healing path dropped one level). Importance and Risk are orthogonal.

| Path ID | Importance | Risk | Consequence | E2E priority | Rationale |
|---------|-----------|------|-------------|--------------|-----------|
| **X1** | **P0** | High | If optimistic lock ever fails to serialize, two tenants share one sandbox = traffic/data cross-contamination | **Must cover first** | Core cross-replica correctness; multi-replica is default |
| **P14c / D5** | **P0** | High | Orphan sandbox CR leak on clone client-timeout-after-persist; not self-healing without a janitor | **Must cover first** | GenerateName + no IsAlreadyExists; resource/cost leak |
| **P10g** | **P1** | High | Fully-prepared sandbox killed/reserved on CSI failure; high churn at high CSI failure rate; non-retriable | **Must cover first** | **CSI focus**; external/infra + ops sensitive |
| **P14g** | **P1** | High | Same as P10g on clone path; non-retriable | **Must cover first** | **CSI focus** |
| **C5** | **P1** | High | Partial CSI mount state on a sandbox that is then discarded | **Must cover first** | **CSI focus**; concurrency + partial-failure |
| **X3 / X4** | **P1** | High | 404 "route not found" after a 201; cross-replica read-after-write gap, widens on scale events | **Must cover first** | gossip timing + ops; E2B clients connect right after create |
| **P15** | **P1** | High | Clone with `secure=true` may return empty token → insecure sandbox; silent | Cover | implicit contract; security-relevant |
| **X2** | **P1** | High | Global create QPS = N×limiter → API Server/scheduler overload under scale | Cover | per-replica limiter; ops scale multiplies |
| **O5** | **P1** | High | Noisy-neighbor: one tenant's no-stock storm starves another's creates | Cover | shared limiter/workers; multi-tenant SLA |
| **P10c** | **P1** | High | Transient create failure (500) under cross-replica lock contention | Cover | recoverable via retry; env-sensitive |
| **P10d / P14d** | **P1** | High | Create 500 on slow/failed scheduling | Cover | scheduler/kubelet/cloud |
| **P10e / P14e** | **P1** | High | Create 500 when envd unreachable | Cover | runtime dependency |
| **P10f** | **P1** | High | Create 500 when identity provider down (gate on); sandbox unusable | Cover (gate on) | provider + feature-gate ops |
| **P10a / P10b** | **P1** | Medium | Create 500 after `ClaimTimeout` with no stock / throttled | Cover | self-heals if pool scales in window |
| **P14b / D6** | **P1** | Medium | Create 500 when limiter wait outlasts timeout | Cover | throttle/timeout interplay |
| **P2** | **P2** | High | Bad JSON → 500 (should be 400); 5xx may cause SDK to retry a non-retryable request | Cover | E2B-compat status contract |
| **P7 / P10h / O1 / O2 / P14a** | **P2** | Medium | Stale cache / TOCTOU template/checkpoint deletion → false 400 or 500-instead-of-404 | Cover | cache staleness + races |
| **P16** | **P1** | Medium | Panic → generic 500, masked cause; replica survives via recover | Cover | hot-path panic could repeat |
| **C2** | **P2** | Medium | Worker saturation → latency / ctx-cancel 500 | Nice to have | backpressure |
| **C3** | **P2** | Medium | Claim vs clone limiter unfairness | Nice to have | shared limiter |
| **D4** | **P2** | Medium | Client-timeout cleanup; orphan if cleanup fails | Nice to have | timeout interplay |
| **P8 / P12** | **P2** | Medium | Clean 400 on CSI driver resolution failure (pre-claim) | Cover | **CSI** but cheap & well-bounded |
| **P3 / P4 / P5 / P6 / P11** | **P3** | Low | Clean 4xx on input validation | Defer (unit-covered) | deterministic; mostly unit-tested |
| **P1** | **P3** | Low | 401 on missing user | Defer | defensive |
| **X5** | **P3** | Medium | Pool over-provision; self-heals via reconcile | Defer | cost only |
| **D3** | **P3** | Low | Premature pause/kill on tiny timeout (expected) | Defer | user misconfig |

---

## 6. E2E Coverage Recommendations

### High-priority cases to write (P0/P1 × High first)
1. **X1 cross-replica double-claim**: two sandbox-manager replicas claim the same template with a single available sandbox in the pool; assert exactly one 201, the other retries onto a different sandbox or fails cleanly — never the same sandbox ID to two callers.
2. **P14c / D5 clone orphan**: induce a client/proxy timeout after the apiserver persists the cloned CR; assert no leaked sandbox CR survives (and confirm whether a janitor reaps it — see Open Questions).
3. **CSI failure suite (P10g, P14g, C5, P14f)** — given the high real-world CSI failure rate:
   - single CSI mount fails after lock/ready/init → assert immediate 500, no retry, and the prepared sandbox is reserved/killed per `ReserveFailedSandboxFor`;
   - one of N concurrent mounts fails → assert whole-request failure and verify cleanup of partially-mounted state;
   - clone CSI failure (both `ProcessCSIMounts` and `ResolveCSIMountFromAnnotation`).
4. **X3/X4 route gossip lag**: create on replica A, immediately connect/describe via replica B before gossip → assert the 404 window behavior and convergence after `reconcileRoute`.
5. **P15 clone `secure` semantics**: clone with `secure=true` where the checkpoint sandbox has no token → assert returned token is non-empty/secure (or document the gap).
6. **O5 noisy-neighbor**: team A no-stock storm; assert team B creates are not starved beyond SLA.
7. **P2 bad-JSON status code**: malformed body → confirm 500 vs the E2B-expected 400 and decide whether to fix.

### Failure-injection points needed (which external dep to perturb)
- **CSI driver / cloud storage**: force mount RPC failures and slow mounts (primary focus).
- **envd runtime**: make `InitRuntime`/`ProcessCSIMounts` connect fail/timeout.
- **API Server**: inject `resourceVersion` conflicts on lock (P10c), 5xx/latency, and stale informer reads (P7/P10h).
- **Scheduler/kubelet**: delay/fail pod readiness (P10d/P14d).
- **Identity provider**: down/slow with `SecurityIdentityProviderGate` enabled (P10f).
- **peers/memberlist**: partition gossip to exercise X3/X4.
- **createLimiter**: low `MaxCreateQPS` + multi-replica to exercise X2/C3/D6/O5.

### Existing coverage (handler `*_test.go` — read as evidence, not run)
- `create_test.go`: `TestCreateSandboxWithClaim_CSIMount`, `TestCreateSandboxWithClone_CSIMount`, `TestCreateSandboxWithClone_InplaceUpdateRejected` (P11), `TestParseCreateSandboxRequest` (P3/P6 family), `TestCsiMountOptionsConfigRecord`.
- `routes_test.go`: claim happy path + admin cross-namespace (P9, partial getNamespaceOfUser).
- infra/sandboxcr: `claim_test.go` (lock conflict, security token, namespace, pick failures — C/X primitives), `clone_test.go` / `infra_test.go` (`TestInfra_CloneSandboxDoesNotRetryCSIMountFailure` confirms P14g non-retry; clone retry-on-create/wait-ready), `csi_test.go` (`TestProcessCSIMounts_*` concurrency/partial-failure — C5 primitives).

### Coverage gaps (no E2E today)
- HTTP-level decode-error status code (P2) — currently 500.
- No-stock / limiter-exhaustion end-to-end (P10a/P10b/X2/D6).
- Cross-replica double-claim (X1) and route gossip lag (X3/X4) — needs a multi-replica harness.
- Clone orphan amplification (P14c/D5) — needs client-timeout-after-persist injection.
- Identity-provider failure path (P10f).
- Clone silent param drop / `secure` semantics (P15).
- Noisy-neighbor / cross-tenant limiter coupling (O5).

---

## 7. Open Questions / Follow-ups
1. **Janitor for clone orphans (P14c/D5)**: is there a reconciler that reaps orphaned cloned Sandbox CRs created by a retry after a client-timeout-after-persist? If not, this is an unbounded leak and should be P0-tracked.
2. **Decode error → 500 (P2)**: intentional E2B-compat choice or a bug? E2B clients typically treat 5xx as retryable, so a malformed body could trigger SDK retry storms. Should `parseCreateSandboxRequest` decode errors return 400?
3. **Clone `secure` / `InplaceUpdate.Resources` / `skip-init-runtime` (P15)**: confirm intended behavior; clone ignores `request.Secure` and reuses `GetAccessToken(sbx)`. Document or fix.
4. **`autoPause` vs E2B `lifecycle` (contract drift)**: verify what the current E2B SDK actually serializes into the HTTP body and whether `autoPause` is still sent, or whether a `lifecycle` mapping is required for compatibility.
5. **Per-replica `createLimiter` (X2)**: is the QPS cap intended to be per-replica or global? If global is desired, a shared/distributed limiter is required.
