# API Key Sandbox Quota — Design Spec (Redis-backed, live-set model)

- Date: 2026-06-18 (rev3)
- Branch: `feature/e2b-api-quota-260617`
- Supersedes:
  - rev0: the per-shard Lease leader-election design (deleted).
  - rev1: the Redis **committed-counter + reservation-overlay + version-guarded reconcile** design (the previous content
    of this file). rev2 replaces that with a **Redis live-set** model: Redis holds the *literal set* of sandboxes each
    limited key currently holds (one entry per sandbox, keyed by its lockstring, carrying its resource footprint),
    maintained **incrementally** (add at create, remove at delete). The cluster is no longer a frequently-relisted source
    that *overwrites a counter*; it is only a **backstop** that a leader-elected controller diffs against to self-heal
    drift. This deletes the entire version-guard machinery (`q:committed`/`q:cver`, RV decimal compare, `mayCreate`,
    fast/slow reconcile cadences, fold/free, the `resv` ZSET overlay, lazy `NEED_SEED`).
  - rev2 → rev3 (this revision): anti-drift **driver** moved from `pkg/cache` to the quota layer and enumerates limited
    keys from the **key store** (Redis-independent → survives total loss); **owner label + backfill dropped** (reads
    reuse the existing `IndexUser` informer index); **APIReader banned** (all reads informer-only, gated on cache sync);
    added an atomic `resyncSums`; **clone** now stamps a lockstring; referenced templates are **immutable for quota**;
    `unlimited → limited` is forbidden forever.
- Scope: `pkg/servers/e2b/` (create, api_key, models, keys, routes), `pkg/servers/e2b/quota/` (new: `QuotaManager`,
  backends, Lua, and the leader-gated anti-drift **driver**), `pkg/sandbox-manager/` (api/infra wiring, a generic
  `IsPrimary()` leadership capability), `pkg/sandbox-manager/config` + `clients` (Redis config/client), `pkg/cache/`
  (additive live-CR read primitive `ListLiveSandboxEntriesByOwner` over the existing `IndexUser` field index, plus event
  handler registration for event-driven release), `cmd/sandbox-manager/`, `config/`/Helm chart (RBAC for
  `coordination.k8s.io/leases`, Redis config), `go.mod`/`vendor` (add a Redis client).

## 1. Background

sandbox-manager is a stateless, multi-replica backend exposing E2B/MCP APIs to manage sandboxes. Each request
authenticates via `X-API-KEY`, resolved to a `*models.CreatedTeamAPIKey` (with `ID uuid.UUID` and `Team`). That `ID` is
the sandbox **owner**: every create path stamps it onto the Sandbox CR as `agentsv1alpha1.AnnotationOwner` (claim and
create-on-no-stock via `utils.LockSandbox`, clone via direct set), and it surfaces as `route.Owner`.

We need to cap how much a single API key may hold — both **count** of sandboxes and their **resource footprint** (CPU,
memory) — enforced across replicas without materially slowing down create. Peak create throughput is ~2500/sec
(150k/min) aggregate; a single cluster may hold ~**500k** sandboxes; the apiserver may lag by up to ~**1 minute**. The
worst case for a single limited key is "small limit + high churn" (constantly creating and deleting against a tiny
limit), which rules out any per-operation external IO on the **unlimited** hot path and demands a cheap, contention-free
check on the **limited** hot path, plus fast slot reclamation.

### Why rev2 exists (relative to rev1)

rev1 stored only an **aggregate counter** in Redis and had to periodically recompute it from a cluster `List` and
overwrite it. That non-atomic, concurrent "read apiserver → overwrite a shared counter" is the *sole* reason it needed
the version-guard machinery (enumerated in *Supersedes* above); the periodic relist is also the dominant cost at 500k
sandboxes and makes the count laggy and imprecise.

rev2 removes the recompute entirely. Redis holds the **live set itself** (one entry per live sandbox), so:

- `live` is the *literal membership* — exact, no recompute, no lag.
- Every mutation is a **targeted, per-lockstring, idempotent** op (add this sandbox / remove this sandbox), never a
  read-modify-write of a shared counter. **Idempotency keyed by a stable per-sandbox identity replaces the version
  guard.** Concurrent / retried / leadership-handoff / double-fired writes all converge.
- A full cluster `List` is needed only by a **rare** backstop rebuild, not on every reconcile tick. Steady state is
  incremental + an infrequent leader-side diff against the warm informer.
- A per-entry **resource footprint** makes multi-dimensional quota (CPU/memory) natural: each dimension is just a sum
  maintained alongside membership.

### Key facts established from the codebase

- `pkg/utils/utils.go:201` `LockSandbox(sbx, lock, owner)` stamps `AnnotationLock = lock` (a UUID from
  `NewLockString()` / `uuid.NewString()`, `utils.go:211`) **and** `AnnotationOwner = owner` on the **same** CR write,
  used by claim/create (`performLockSandbox`, `claim.go:661`). **The lockstring is an existing per-sandbox UUID identity
  persisted on the CR** — rev2 reuses it as the Redis entry key; no new `opID` annotation is introduced. **Gap fixed in
  rev3:** the **clone** path (`newSandboxFromTemplate`, `clone.go:303`) previously stamped only `AnnotationOwner`, *not* a
  lockstring; rev3 makes clone stamp one too (via its `Modifier`, `create.go:200`, using `NewLockString()`) so **every**
  owner-stamped CR carries a lockstring (§5).
- Sandbox is Pod-backed: resources live in the inlined `EmbeddedSandboxTemplate` PodTemplateSpec
  (`sandbox_types.go:99-113`), as container `Resources`. The create request's **target** resources are known at admit
  time (claim may `InplaceUpdate` a pooled sandbox to the requested spec — `claim.go:213`, `create.go`), so the footprint
  to charge is computable before committing to a specific instance.
- `pkg/cache` is the sandbox-manager-only, informer-backed cache. It already runs informers, exposes
  `CacheSandboxCustomReconciler` + `AddReconcileHandlers()` for external event handlers, and maintains an `IndexUser`
  field index over `AnnotationOwner` (`index.go:83`). rev3 adds only a live-CR read primitive
  `ListLiveSandboxEntriesByOwner` here and lets the quota layer register an event handler; the anti-drift **driver**
  itself lives in `pkg/servers/e2b/quota` (§6.5.2). Anti-drift never uses `GetAPIReader` — all its **Sandbox CR** reads
  are informer reads, gated on cache sync (subjects come from the key store, not the apiserver; §6.5.2).
- `CountActiveSandboxes` **excludes** `Dead` (`cache.go:306`) and is relied on by the SandboxClaim controller. **It must
  not be modified.** Quota uses additive owner reads instead (§6).
- `AnnotationOwner` is an **annotation**. `pkg/cache` already registers an `IndexUser` field index over it
  (`index.go:83`), so the anti-drift driver lists a key's live CRs with `cache.List(MatchingFields{user: K})` straight
  off the **informer** — no owner label and no apiserver read are needed (§5).
- `k8s.io/client-go/tools/leaderelection` (Lease-backed) is already vendored — reused for a single generic
  `IsPrimary()` capability (§8).
- No Redis client is vendored yet; rev2 adds one (e.g. `github.com/redis/go-redis/v9`) behind a backend interface.

## 2. Goals & Non-Goals

### Goals

- Enforce per-API-key limits on **count**, **cpu** (requests, millicores), and **memory** (requests, MB) of sandboxes
  held, strongly consistent across replicas while Redis is healthy. The model is **multi-dimensional and extensible**
  (future: `limits.*`, per-template scope) without a schema change.
- Make `live` **exact and lag-free in steady state** by storing the literal live set in Redis and maintaining it
  incrementally — no periodic counter recompute, no frequent full `List`.
- Keep enforcement correct under concurrency/retry/leadership-handoff using **per-lockstring idempotent, per-op atomic**
  Redis writes (Lua) — no version guard.
- Unlimited keys perform **zero** Redis IO on the hot path. Limited keys perform **at most one** Redis round-trip per
  acquire.
- The cluster remains the **ground truth**; the Redis live set is reconstructible from it and self-heals via a
  **bidirectional** anti-drift diff (add entries for live CRs missing from Redis; remove entries whose CR is gone).
- Reclaim slots **fast** for small-limit high-churn keys: eager release on create failure, pre-emptive release on
  delete, plus event-driven removal — never solely the periodic backstop.
- Backward compatible: keys without a quota field default to **unlimited**.

### Non-Goals (Phase 1)

- **`limits.*` dimensions, per-template scope, per-team scope.** The quota subject is always the API key. Only model
  extension points are reserved; not enforced.
- **Post-create resize / 变配 admission as a separate API.** sandbox-manager exposes no post-create resource-change
  capability. If a create chooses an in-place update of a pooled sandbox, the footprint charged is the **declared target
  resources at create** (§6.3); since resources never change after create, **no informer update-event footprint tracking
  is needed** and the entry footprint is fixed for the sandbox's lifetime.
- **Strict no-oversell across Redis unavailability.** Phase 1 is **fail-open** by default on Redis trouble (§9): during a
  Redis outage limited keys are temporarily unenforced (treated as unlimited), accepting a bounded oversell that the
  leader's anti-drift rebuild self-heals once Redis returns. The **fail-closed** posture (and its `q:warm`/settle/`WAIT`
  machinery) is deferred to a later PR (§9, §16-future).
- Reclaiming/evicting existing sandboxes; quota only blocks new creates.
- Quota mutability: settable only at **key create**, immutable thereafter — **no `PATCH`** (§6.8 eliminates the
  `unlimited → limited` transition oversell by construction).
- Billing/usage reporting; cross-cluster/multi-region quota; a per-op consensus log or apiserver-side fencing webhook.

## 3. Quota Data Model (static)

Addressed by `(apiKeyID, dimension, scope)`. Phase 1 enforces three dimensions at `scope = {}` (per-api-key).

```go
type QuotaDimension string
const (
    DimSandboxCount QuotaDimension = "sandbox.count" // unit: sandboxes
    DimCPU          QuotaDimension = "cpu"           // unit: millicores (sum of container requests.cpu)
    DimMemory       QuotaDimension = "memory"        // unit: MB        (sum of container requests.memory)
    // future: limits.cpu, limits.memory, ...
)

// Scope narrows where a limit applies. Subject is always the API key (never a team).
// Phase 1: empty == per-api-key (the only enforced scope). Template is a forward extension point only.
type QuotaScope struct {
    Template string `json:"template,omitempty"` // future per-template scope (not enforced in Phase 1)
}

type QuotaLimit struct {
    Dimension QuotaDimension `json:"dimension"`
    Scope     QuotaScope     `json:"scope,omitempty"`
    Limit     *int64         `json:"limit,omitempty"` // nil == unlimited; normalized unit per dimension
}

type QuotaSpec struct {
    Limits []QuotaLimit `json:"limits,omitempty"` // empty/absent == fully unlimited
}
```

- **Units (internal, integer):** cpu in **millicores** (`resource.Quantity.MilliValue()` summed over containers);
  memory in **MB** = 10^6 bytes (summed bytes, **ceil**-rounded, to bias conservative). All Lua math is integer.
- **External JSON** is nested and extensible, e.g.
  `"quota": { "sandbox": { "count": 50 }, "cpu": "16", "memory": "32Gi" }`. The handler normalizes resource values
  (quantity strings) into the internal millicores/MB and into `QuotaSpec.Limits`. The exact external shape is an
  implementation detail provided it stays nested.
- `QuotaSpec` is loaded at auth time (`CheckApiKey` puts `user` in context), so the hot path never re-reads the key
  store.

### 3.1 Validation (only at key create — Phase 1 has no quota `PATCH`)

A quota is **never silently ignored**:

- **Absent / `null` quota** (or empty `Limits`) → **unlimited**.
- **`Limit == nil`** for a dimension → that dimension **unlimited**.
- **All present limits `nil`** (a non-empty `Limits` that nonetheless constrains nothing) → **normalized to unlimited /
  absent at write time**, so a "limited key" is defined uniformly across the system as *having ≥1 non-nil limit*. This
  removes the ambiguity where the hot path would treat such a key as unlimited while the anti-drift driver (§6.5.2) would
  otherwise enumerate it as limited.
- **`Limit == 0`** → **valid** hard-zero (every create in that dimension returns 429).
- **`Limit < 0`** → **rejected**.
- **Duplicate `(dimension, scope)`** → **rejected**.
- **Unknown dimension** (anything other than `sandbox.count` / `cpu` / `memory` in Phase 1, e.g. `limits.cpu`) →
  **rejected**, so a not-yet-enforced future dimension is never silently dropped.
- **Non-empty `scope`** → **rejected in Phase 1**.
- **No Redis configured** + a non-empty quota → **rejected** (§6.1).

## 4. Static Config Storage

Both backends store **only** the static `QuotaSpec`, alongside the key, written once at **key create** (immutable
thereafter, §6.8). **No dynamic usage is ever written to the key store** — that lives in Redis.

- **Secret backend:** add `Quota *QuotaSpec json:"quota,omitempty"` to `models.CreatedTeamAPIKey`; serializes into the
  per-key JSON inside `e2b-key-store`; old payloads decode to `nil` == unlimited; writes reuse `retryUpdateSecret` CAS.
- **MySQL backend:** add a nullable `quota JSON` column to `team_api_keys` (`NULL` == unlimited); `AutoMigrate` adds it,
  gated by `DisableAutoMigrate` with a documented manual DDL alternative.

## 5. Identity, Owner Index, and Counting Primitive

- **Sandbox identity in Redis = the lockstring** (`AnnotationLock`, a UUID stamped by `LockSandbox`). It is globally
  unique and persisted on the CR, so it survives `GenerateName` (the create path need not know the final object name).
  Re-keying every Redis op off it gives idempotency for free. **Every owner-stamped create path must stamp a
  lockstring:** claim/create already do (`LockSandbox`); rev3 adds it to **clone** (via the `Modifier`, `create.go:200`,
  using `NewLockString()`), closing the one path (`clone.go:303`) that previously set only `AnnotationOwner`. This is a
  hard precondition (§7).
- **Owner read uses the existing `IndexUser` field index — no owner label, no backfill.** `pkg/cache` already indexes
  `AnnotationOwner` (`index.go:83`), so the anti-drift driver lists a key's live CRs with
  `cache.List(MatchingFields{user: K})` directly off the informer. Because all anti-drift reads are informer reads (never
  APIReader, §6.5.2), no server-side label selector is required, so rev3 adds **no** owner label and needs **no** one-time
  backfill. The index covers every CR carrying `AnnotationOwner`, including clones once they are owner-stamped.
- **Quota "live" predicate — the single source of truth for charging, used identically by the count/sum reads and by
  both anti-drift directions (§6.5.2):**

  ```go
  // isLiveForQuota reports whether a Sandbox still occupies its owner's quota.
  // Freed (NOT live) iff deletion has been requested or is in progress; a merely
  // Failed/Succeeded-but-not-yet-deleted sandbox still occupies quota until it is.
  func isLiveForQuota(sbx *agentsv1alpha1.Sandbox) bool {
      return sbx.DeletionTimestamp == nil &&
          sbx.Status.Phase != agentsv1alpha1.SandboxTerminating
  }
  ```

  This predicate is **deliberately narrower** than `CountActiveSandboxes`'s "not `Dead`" filter (which *also* excludes
  Failed/Succeeded via `GetSandboxState`): quota frees a slot only when deletion is requested/terminating, **not** the
  moment a pod fails. That difference — plus the need for per-entry `{lockstring, footprint}` data — is why quota uses its
  own additive read (over `IndexUser`) rather than `CountActiveSandboxes` (left untouched, §1). The exact field/enum
  (`Status.Phase == SandboxTerminating`, `sandbox_types.go:262`) is confirmed at implementation; the **semantics** above
  are fixed. We do **not** wait for the CR to become invisible.

  ```go
  // ListLiveSandboxEntriesByOwner returns, for owner K, every Sandbox CR with
  // isLiveForQuota == true, each as {lockstring, footprint}. Reads the warm informer
  // via MatchingFields{user: K} (IndexUser). Never APIReader; the caller invokes it only
  // after the cache has synced (§6.5.2), so an unsynced cache can never look "empty".
  func (c *Cache) ListLiveSandboxEntriesByOwner(ctx, opts) ([]LiveEntry, error)
  ```

## 6. Counting Model (Redis live-set)

All enforcement lives in a `QuotaManager` (package `pkg/servers/e2b/quota`) behind a `QuotaBackend` interface, with a
`redisBackend` and a `noopBackend`.

```go
type QuotaManager interface {
    // Acquire charges one sandbox of the given footprint, or returns ErrQuotaExceeded (429).
    // Unlimited keys are a no-op (zero Redis IO). Idempotent on the lockstring.
    Acquire(ctx, req AcquireRequest) (Reservation, error)
    // Release returns a charged sandbox. Idempotent. Issued with context.WithoutCancel.
    Release(ctx, req ReleaseRequest) error
    // Describe reports current per-dimension usage/limit for read APIs.
    Describe(ctx, apiKeyID string) (QuotaStatus, error)
}
// AcquireRequest carries apiKeyID, namespace, lockstring, footprint{cpu,mem}, and the loaded QuotaSpec.
```

The **anti-drift / removal driver** (leader-gated, §6.5) is not part of the request-serving interface; it lives in the
quota layer (`pkg/servers/e2b/quota`, §6.5.2) and reuses the same Lua helpers. `pkg/cache` only exposes the owner-indexed
live-CR read (`ListLiveSandboxEntriesByOwner`) and accepts the registered event handler — it never drives reconcile.

### 6.1 Backend selection (pluggable, optional Redis)

- **Redis configured** → `redisBackend`: full enforcement.
- **Redis not configured** → `noopBackend`: `Acquire` always allows, `Release`/anti-drift are no-ops, `Describe` reports
  "unlimited". Setting a **non-empty** quota at key create is **rejected** (§3.1) so a quota is never silently ignored.

### 6.2 Redis keys (per limited key K; all hash-tagged `{K}`)

| Redis key | Type | Writer(s) | Meaning |
|---|---|---|---|
| `q:live:{K}` | HASH | Acquire / anti-drift **add** (HSET+INCRBY); Release / event / anti-drift **remove** (HDEL+DECRBY) | field = **lockstring**, value = JSON `{"cpu":<m>,"mem":<MB>,"ts":<redis-unix>}`. Membership = the live set; `cpu`/`mem` are the authoritative per-sandbox charge decremented on removal; `ts` (Redis server clock at acquire) is the age source for the anti-drift remove gate (§6.5.2) |
| `q:sum:cpu:{K}` | int | same ops (INCRBY/DECRBY) | Σ cpu footprint over `q:live:{K}` |
| `q:sum:mem:{K}` | int | same ops (INCRBY/DECRBY) | Σ mem footprint over `q:live:{K}` |

`live(K)` per dimension: **count = `HLEN q:live:{K}`**, **cpu = `GET q:sum:cpu:{K}`**, **mem = `GET q:sum:mem:{K}`**.

> **No global keys in Phase 1.** Because Phase 1 is fail-open (§9) there is **no** `q:warm`/`q:warmAt`/settle barrier.
> Every Lua touches only per-`{K}` keys, so with the `{K}` hash-tag they co-locate in one slot — **Redis Cluster causes
> no `CROSSSLOT`** here (standalone / Sentinel / Cluster are all structurally compatible in Phase 1; officially
> testing/supporting Cluster is a separate call). The global cold/warm barrier returns only with the deferred
> fail-closed posture, at which point the Cluster question reopens.

**Sum/membership consistency** is guaranteed because **every** mutation changes the HASH field **and** the affected sums
in the **same** Lua script — they are never written separately. This is a hard invariant (§16) and the only thing the
sums depend on.

### 6.3 Footprint

The footprint charged for a sandbox is the sum over its **regular containers** of `requests.cpu` (millicores) and
`requests.memory` (MB, ceil), computed from the **declared target spec at create** (the resources the sandbox will have,
including any create-time `InplaceUpdate` to a pooled instance). **Phase 1 counts regular containers only** — init
containers and native sidecars (`restartPolicy: Always` init containers) are **not** charged; a Kubernetes
effective-pod-request accounting is a possible later refinement.

**Footprint invariant (load-bearing):** every sandbox is created with a **positive** cpu *and* memory request — upheld by
the platform's templates/defaulting (a defaulting/validating webhook can enforce it if a deployment cannot otherwise
guarantee it). This keeps `footprintOf > 0`, which two correctness properties rely on: (a) a `limit == 0` dimension
rejects **every** create — the Acquire Lua treats `lim == 0` as a hard-zero regardless of footprint, so even a (degenerate)
zero-request create is rejected; and (b) any single limited dimension **bounds the live-set** — because each entry
consumes a positive amount of cpu/memory, a cpu/memory limit caps the entry count just as a `sandbox.count` limit would,
so a cpu/mem-only-limited key cannot accumulate unbounded zero-footprint entries (§6.5.2 cost bound). If the invariant
were ever violated, only (b) weakens (unbounded set under a resource-only limit); (a) holds unconditionally via the Lua.

The footprint is read from the spec — **not** written to a CR annotation. It is
stored in the `q:live:{K}` entry value and is **fixed for the sandbox's lifetime** (no post-create 变配 exists, §2), so
removal decrements exactly what was charged with no drift and no update-event tracking. The anti-drift **add** path
recomputes the identical footprint from the existing CR's spec (§5 primitive).

Resource resolution: for an inline `Template`, read container `Resources` directly; for a `TemplateRef`, resolve the
referenced `SandboxTemplate`. A referenced `SandboxTemplate` is **treated as immutable for quota** — so the anti-drift
**add** path, resolving the same ref later, recomputes the **identical** footprint it charged at create; all footprint
inputs are thus recoverable from the CR (inline) or the immutable referenced template, with no drift. A shared
`footprintOf(podSpec)` helper is used by both Acquire and anti-drift add; a resolution failure **fails safe** (defer the
add — never charge zero).

This immutability is a **load-bearing assumption** for `TemplateRef` sandboxes (a mutated template would make
anti-drift add/rebuild charge a *different* footprint than create). Phase 1 treats it as a deployment convention, but
because it is correctness-critical the robust enforcement — a validating webhook rejecting quota-relevant resource edits
to a referenced `SandboxTemplate` — **SHOULD ship alongside Phase 1 whenever referenced templates are user-mutable**. The
equally-robust alternative is to **snapshot** the charged footprint onto the Sandbox CR at create (§17) so the recompute
reads the CR, not the ref — removing the dependency entirely (and the simplest path if a deployment needs mutable
templates). Inline-`Template` sandboxes (e.g. clones, §1) already carry their resolved spec, so they are unaffected
regardless.

### 6.4 Acquire (hot path) — atomic, multi-dimensional, idempotent

Single Lua, run on **every** replica (Redis is the serialization point; no leadership on the hot path):

```
-- KEYS[1]=q:live:{K}  KEYS[2]=q:sum:cpu:{K}  KEYS[3]=q:sum:mem:{K}
-- ARGV: lockstring, footCpu, footMem, limCount, limCpu, limMem   (-1 == unlimited for a dimension)
if redis.call('HEXISTS', KEYS[1], lockstring) == 1 then return 'OK' end        -- idempotent re-entry: already charged
local cnt = redis.call('HLEN', KEYS[1])
local sc  = tonumber(redis.call('GET', KEYS[2]) or '0')
local sm  = tonumber(redis.call('GET', KEYS[3]) or '0')
if limCount >= 0 and cnt + 1      > limCount then return 'REJECTED' end         -- multi-dim: ALL must fit; count==0 rejects (cnt+1>0)
if limCpu   == 0 or (limCpu > 0 and sc + footCpu > limCpu) then return 'REJECTED' end  -- cpu: lim==0 is hard-zero (rejects even a 0-footprint create), else threshold
if limMem   == 0 or (limMem > 0 and sm + footMem > limMem) then return 'REJECTED' end  -- mem: lim==0 is hard-zero, else threshold
local now = redis.call('TIME')[1]                                              -- Redis server clock (skew-free) for entry age
redis.call('HSET',   KEYS[1], lockstring, cjson.encode({cpu=footCpu, mem=footMem, ts=now}))
redis.call('INCRBY', KEYS[2], footCpu)
redis.call('INCRBY', KEYS[3], footMem)
return 'OK'
```

- **Unlimited key** (no `QuotaSpec`, or every limit nil) → `QuotaManager.Acquire` short-circuits before any Redis call
  (zero IO) — the majority of the 2500/sec.
- `OK` → proceed with create; the lockstring is the one already stamped on the CR (`LockSandbox`), so no extra CR write.
- `REJECTED` → HTTP **429**, returned **immediately with no retry** (the create path must not loop on a quota miss; a
  pooled sandbox tentatively picked before the charge must be returned to the pool — see §10).
- **Idempotent re-entry:** a retry with the same lockstring finds the field present → `OK` without double-charging. Sums
  only move on the first commit. (Lockstrings are fresh per-create UUIDs, never reused with a different footprint, so the
  `HEXISTS` short-circuit cannot under-charge; a defensive footprint-mismatch check is unnecessary.)
- **Redis transport error / unreachable** → **fail-open** (treat the key as unlimited for this request; allow). Phase 1
  default; a config knob to fail-closed instead is deferred (§9).
- **Quantization:** values are integers; an unlimited dimension passes `-1`.

### 6.5 Removal — eager, pre-emptive, event-driven, and a leader backstop

Release means an atomic, idempotent Lua that reads the entry's stored footprint and removes it:

```
-- KEYS as above; ARGV: lockstring
local v = redis.call('HGET', KEYS[1], lockstring)
if not v then return 0 end                                                     -- already gone → no-op (idempotent)
local f = cjson.decode(v)
redis.call('HDEL',   KEYS[1], lockstring)
redis.call('DECRBY', KEYS[2], f.cpu)
redis.call('DECRBY', KEYS[3], f.mem)
return 1
```

A sandbox's slot is freed as soon as its deletion is **requested or proven** — we do **not** wait for the CR to become
invisible. Paths, fastest first (all idempotent, safe to overlap):

1. **Create failure → eager Release** (replica handling the create). Unlike rev1's conservative "release only if provably
   pre-CR", rev2 releases on **any** create failure rather than depend on the backstop. If the CR was in fact committed,
   the bidirectional anti-drift **re-adds** it within grace (§6.5.2) — so an over-eager release self-heals; the
   trade is a transient over-admission, bounded by grace.
2. **Manager `DELETE /sandboxes/{id}` → pre-emptive Release** (replica handling the delete). The slot returns to the key
   immediately, before the CR is gone. If the delete then fails/stalls, anti-drift behaviour is governed by the
   live-CR rule below.
3. **Leader informer event** (covers non-manager deletions — TTL, `kubectl`, controller-driven): on a Sandbox owner-CR
   event where the CR transitions to **not** `isLiveForQuota` (deletion-requested / terminating) or is truly deleted →
   Release that lockstring. Runs **only on the leader** (`IsPrimary()`); idempotent w.r.t. paths 1–2.
4. **Leader anti-drift backstop** (§6.5.2): removes entries with no live CR after grace.

#### 6.5.1 Why "deletion-requested = freed" is safe and bounded

A pre-emptively-released sandbox that is stuck `Terminating` still physically exists until GC, so for the window between
delete-request and true deletion a key can have `actual > live` (e.g. a stuck-terminating pod plus a freshly created
replacement). This is the deliberate trade for low-latency reclamation (§16). It is **bounded** by the number of
concurrently terminating sandboxes and resolves when they truly disappear. The anti-drift **add** path does **not**
resurrect such entries (it only adds entries for `isLiveForQuota` CRs), so it never fights an intended deletion.

#### 6.5.2 Bidirectional anti-drift (leader-gated driver in `pkg/servers/e2b/quota`)

This is the **single** correction primitive; it makes drift in **either** direction self-heal. Critical: in an
incremental live-set model the backstop must be **bidirectional** — a "remove-only" GC would let a lost entry (Redis
restart / async-failover / rollback that drops an entry whose CR exists) undercount **forever**, since nothing re-adds
it → permanent oversell.

**Placement & layering.** The reconcile **driver** lives in the quota layer (it owns the key store and the Redis
backend); `pkg/cache` only exposes the live-CR read primitive `ListLiveSandboxEntriesByOwner` (§5, over `IndexUser`). The
driver imports nothing from cache beyond that read; the event-driven release (path 3) is a closure owning the
`QuotaManager` **registered into** the cache via `AddReconcileHandlers()` (cache never imports quota — no cycle). Both
the periodic diff and the event handler run only while `IsPrimary()` (§8).

**Enumerating the subjects.** The driver enumerates the **limited keys** (those whose stored `QuotaSpec` has ≥1 non-nil
limit — the same definition the hot path uses, §3.1) from the **key store** — the durable, Redis-independent source, which is exactly why it survives a total Redis loss (an
empty Redis cannot tell you which keys *should* exist). Redis is never `SCAN`-ed to discover subjects (that would break
on total loss and on Cluster). Enumeration cost is backend-specific: the **Secret** backend reads the single
`e2b-key-store` object once and filters `QuotaSpec != nil` in memory (it necessarily parses all stored keys, but it is
one read); the **MySQL** backend filters server-side (`WHERE quota IS NOT NULL`). Full-rebuild duration after a Redis
loss is therefore bounded by `#limited-keys × per-key work`; the implementation paginates/budgets per cycle, jitters,
and exports diff-lag / rebuild-duration / divergence metrics (§16). For each enumerated key K:

1. `live := cache.List(MatchingFields{user: K})` filtered by `isLiveForQuota` → the authoritative set of
   `{lockstring, footprint}` (informer only).
2. `have := HGETALL q:live:{K}` → the current Redis entries.
3. Diff:
   - **Add (missing entry → charge):** lockstring in `live`, absent from `have`, CR `CreationTimestamp` age `> grace` →
     add via the Acquire-add Lua **without** the limit checks (anti-drift reflects reality; it cannot reject an existing
     sandbox). Heals lost entries / rebuilds after Redis loss.
   - **Remove (leaked entry → free):** lockstring in `have`, **not** `isLiveForQuota` in `live` (gone or
     deletion-requested/terminating), entry age (`now − entry.ts`, Redis clock) `> grace` → Release. Frees
     failed-create leftovers and deletes the event handler missed.
   - **Resync sums:** run the atomic `resyncSums` Lua (`HVALS q:live:{K}` → Σ → `SET q:sum:cpu/mem:{K}`) so the sums are
     recomputed from authoritative membership — repairing any sum-only divergence (e.g. a partial loss that dropped a sum
     key but not the hash) that the membership-guarded add/remove cannot see. Cost is bounded by the key's own live-set
     size, which its **own quota** caps (the footprint invariant, §6.3: a `sandbox.count` limit directly, or — since
     every sandbox has a positive cpu/memory footprint — a cpu/memory limit equally bounds the entry count), so the hash
     never grows unboundedly. `resyncSums` runs on a **slow cadence** (much less frequent than the membership diff — it
     only repairs the rare *external* sum-only loss), so even for a key with a very large quota its single-threaded
     `HVALS` cost is infrequent and amortized; resync latency is a monitored metric and the implementation documents a
     max-supported per-key live-set envelope.

Add/Remove are **per-lockstring, idempotent** (HEXISTS / HGET guards) and `resyncSums` is a single atomic Lua, so the
whole pass is safe to run concurrently with the hot path and with a flapping leader, with **no version guard**.

- **Cache-sync / health gate (never APIReader).** All **Sandbox CR** reads are informer reads (subjects come from the
  key store). The gate is **not one-time**: **every remove-capable pass** — both the periodic diff and the leader's
  event-driven release (path 3) — first checks the Sandbox informer is **healthy**, defined as: it has completed ≥1 full
  list, `HasSynced` is true, and no watch error / outstanding relist since the last successful sync — and **skips the
  remove direction** otherwise. So a freshly-(re)elected leader, a mid-run relist, or a watch-bookmark gap can never make
  a partial/cold cache look "empty" and wrongly free a live slot. The **add** direction is always safe (it only ever
  charges existing CRs). And even if a spurious remove ever slipped through, it **self-heals** — the add direction
  re-charges the still-live CR within `grace` (a bounded transient, §7.1); correctness does not *rest* on the gate, the
  gate just shrinks the transient. A lagging-but-synced informer is also safe — a create not yet in cache defers the add;
  a delete still in cache defers the remove — both converge on the next pass, within `grace`. A **key-store enumeration
  error** likewise skips that anti-drift cycle (metric + log) and is **never** interpreted as an empty subject set.
  Skipped passes (cache-unhealthy or enumeration-error) are counted as metrics.
- **Grace = 10 minutes** (§16), comfortably above the ~1-minute worst-case apiserver lag, so a just-created CR is never
  mistaken for "absent" and a just-released slot is never mistaken for "missing".
- **Cadence / cost.** The *remove* direction is driven primarily by the leader's informer **events** (path 3) at event
  speed; the periodic full **bidirectional diff** is an **infrequent** backstop (minutes). Every read is informer-served
  — there is **no apiserver `List`** anywhere — and the per-key work (`cache.List` by index + `HGETALL`/`resyncSums`) is
  bounded by that key's own live-set size (capped by its own quota — see *Resync sums* above), not by the 500k cluster
  total. This is what removes the rev1 relist cost.

### 6.6 New keys need no seed

A brand-new limited key provably owns **zero** sandboxes, so an absent `q:live:{K}` legitimately means count 0 / sums 0
— `Acquire` simply starts charging from empty. There is **no** `NEED_SEED` round-trip and no per-key consistent read on
the cold path (rev1's lazy seed is deleted). The only case where an absent/short hash does **not** mean "truly zero" is a
Redis data loss for an already-active key; that is handled by fail-open + the leader rebuild (§9), the same bounded
self-healing envelope, not by a per-acquire seed.

### 6.7 Key-deletion cleanup

On API-key delete (§11): `DEL q:live:{K} q:sum:cpu:{K} q:sum:mem:{K}` — all three share the `{K}` hash-tag, so this is a
**single one-slot command** (Cluster-safe, no `CROSSSLOT`). This is the **sole** cleanup for a deleted key's entries:
since the driver enumerates subjects from the key store (§6.5.2), a deleted key is no longer reconciled, so its entries
are never revisited — therefore there is **no `SCAN` sweep**. To shrink the leak window the `DEL` is retried a bounded,
**non-blocking** number of times off the hot path; if it still fails the residue is harmless dead memory (key IDs are
fresh, never-reused UUIDs) that is never read again — monitor Redis memory for it. Failure is **non-fatal**.

### 6.8 Quota lifecycle: immutable after create

A key's `QuotaSpec` is **immutable after creation — there is no quota `PATCH`** (§11). This eliminates by construction
the only normal-operation oversell this design would otherwise have, the **`unlimited → limited` transition race**
(rev1's most likely oversell): while unlimited the hot path bypasses Redis, so a later re-limit would leave CRs created
by a stale-`unlimited` replica uncharged while a fresh-`limited` replica admits against a short `live`. By fixing the
mode at birth:

- A key created **limited** is enforced from its first `Acquire`; it owns zero sandboxes at birth (§6.6), so there is no
  prior unlimited interval leaving uncharged CRs.
- A key created **unlimited** stays unlimited; the hot path always bypasses Redis for it.
- A replica that has not yet observed a newly-created key resolves it as **unknown** (auth failure), never as
  "unlimited" — so no replica ever holds a stale `unlimited` view of a limited key.

To change a key's quota, create a **new** key. Any future `PATCH` may only change the limits of a key that was **born
limited** — promoting an `unlimited` key to `limited` is **forbidden forever** (so the hot path's "unlimited ⇒ bypass
Redis" assumption can never be invalidated) — and MUST ship a safe-activation scheme (activation window, or forced
cross-replica `QuotaSpec` cache invalidation). Deferred.

## 7. Correctness

Per dimension, admission grants iff `live + charge <= limit`, computed and committed **atomically** in the Acquire Lua.
While Redis is healthy, at the instant of every admission `live` equals the exact charged set, so a grant cannot exceed
the limit — **strict enforcement at admit**.

The whole-system invariant is **convergence**: the Redis live set converges to the cluster's set of **live** owner-K CRs
(by lockstring), with `q:sum:*` equal to the Σ of their footprints. This rests on:

1. **Per-lockstring idempotent, per-op atomic mutation.** Every Redis write is one Lua script (or one idempotent
   command) keyed by a stable lockstring and guarded by `HEXISTS`/`HGET`. Aggregates (`HLEN`, sums) move **only** through
   these guarded ops. Therefore concurrent acquires, retries, leadership handoff, and double-fired releases (pre-emptive
   + event) all converge — **no version guard, no stop-the-world.**
2. **Bidirectional anti-drift (§6.5.2).** Every divergence is corrected after `grace` (plus the diff cadence): a CR with
   no entry is added; an entry with no live CR is removed; sums are recomputed from membership (`resyncSums`). This is
   what keeps lost entries (Redis restart / failover / rollback) — and sum-only losses — from drifting permanently, the
   property a remove-only GC would lack.
3. **Sum/membership consistency (§6.2 + §6.5.2).** Incrementally, sums move only in the same script as membership; for an
   external loss that breaks that coupling, the periodic atomic `resyncSums` recomputes sums from authoritative
   membership.
4. **Deletion-requested = freed (§6.5.1).** A CR that is not `isLiveForQuota` (deletion-requested/terminating) does not
   count; anti-drift add only considers `isLiveForQuota` CRs, so it never fights a deletion.

### 7.1 Honest no-oversell statement

- **Redis healthy:** enforcement is **strict at admit** — a grant never exceeds the limit at the moment it is made.
- **Transients bounded by `grace` (10 min) + the diff cadence, self-healing:**
  - *Eager/pre-emptive release of a CR that actually exists* → transient over-admission until anti-drift re-adds. Bounded
    by in-flight create-failures / stuck-terminating sandboxes.
  - *Leaked entry (failed create that did charge)* → transient over-rejection (under-sell) until anti-drift removes.
  - *Lost Redis entries (restart/failover/rollback) while the key stays active* → transient over-admission until the
    leader rebuild/diff re-adds; **fail-open** means the gap is "unenforced", not "rejected".
- **Redis unavailable (Phase 1 default = fail-open):** limited keys are **temporarily unenforced** (treated as
  unlimited). This is an explicit availability-over-enforcement choice; oversell during the outage is bounded by the
  outage's create volume and self-heals once Redis returns and anti-drift rebuilds. A fail-closed posture (reject during
  outage) is the deferred alternative (§9).

In short: **strict at admit while Redis is healthy; bounded, self-healing, documented transients otherwise; fail-open by
default.** This matches the product intent (prefer availability; never *unbounded* oversell) given the chosen
architecture (cluster = ground truth, Redis = exact live cache, no per-op fencing).

Implicit precondition: **every path that stamps `owner=K` onto a CR goes through `Acquire`** (so the charge precedes the
CR). Phase 1 stamps owner only on the E2B create paths (claim/clone), which all go through `Acquire`.

## 8. Generic "primary manager" leadership

The hot path (`Acquire`/`Release` paths 1–2) runs on **all** replicas. Only the leader-side removal/anti-drift
(§6.5 paths 3–4) benefits from running once.

- `SandboxManager` gains a **generic** `IsPrimary() bool`, backed by a single `coordination.k8s.io/Lease`
  (`sandbox-manager-primary`) via the vendored `client-go/tools/leaderelection` — intentionally not coupled to quota, so
  any future singleton task can gate on it.
- The quota anti-drift **driver** (in `pkg/servers/e2b/quota`) — both its event-driven release (registered into the
  cache via `CacheSandboxCustomReconciler` + `AddReconcileHandlers`) and its periodic diff — runs only while
  `IsPrimary()`. The hot path and per-request release are **not** gated.
- **Leadership carries no correctness weight.** Correctness rests on per-lockstring idempotency (§7). If leadership
  flaps/splits, the worst case is anti-drift running on several replicas at once — idempotent, hence safe.
- RBAC: `get/list/watch/create/update` on `coordination.k8s.io/leases` in the manager namespace (one lease).

## 9. Degradation & Redis Data Loss (Phase 1: fail-open)

Phase 1 posture: **fail-open** on any Redis trouble; rely on the leader's bidirectional anti-drift to rebuild.

- **No Redis configured** → `noopBackend`: all keys unlimited; non-empty quota at key create rejected (§6.1).
- **Redis transiently unreachable** (restart, network error, failover unreachable phase): limited-key `Acquire`
  **allows** (treated as unlimited for that request); unlimited keys unaffected. Bounded oversell, self-healing.
- **Redis data loss** (cold restart / flush / first boot): hashes are empty, so `Acquire` reads `live = 0` and allows —
  which **is** the fail-open behaviour; no special detection needed. The leader's anti-drift **add** pass repopulates
  `q:live:*`/`q:sum:*` by enumerating the limited keys from the **key store** (Redis-independent, §6.5.2) and reading
  each key's live CRs off the **informer** (`IndexUser`, never APIReader). Enforcement resumes per key as its entries are
  rebuilt. The oversell window is the rebuild duration; bounded and self-healing.
- **Partial rollback** (some entries or a sum key lost, others survive): the **add** direction re-charges missing live
  CRs and `resyncSums` recomputes the sums from membership (§6.5.2) within `grace`. No detection key is needed in the
  fail-open posture.
- **Redis config removed after keys exist** (operational): treated exactly as a Redis **outage** — limited keys fall to
  fail-open and new non-empty-quota key creates are rejected (`noopBackend`, §6.1). No special cross-check is added.

> **Deferred (later PR): fail-closed posture.** A config knob `onRedisUnavailable: allow | reject` (default `allow`).
> `reject` reintroduces the rev1 machinery only where it is actually needed for strictness across loss: a global
> `q:warm` total-loss detector, a Redis-server-clock `q:warmAt` settle window (so in-flight creates commit before the
> rebuild seed), `COLD → 503` retryable on the hot path, and optionally synchronous reservation replication
> (`WAIT`/sync topology) for strictness across failover. This is the fiddliest part of the old design and is **not**
> built in Phase 1; its global keys are also what reopen the Redis Cluster `CROSSSLOT` question (§6.2).

Operational recommendation to keep loss rare even under fail-open: Redis AOF `appendfsync everysec` + HA (Sentinel-managed
primary with a replica).

## 10. Create Hot Path (limited key)

1. `CheckApiKey` already put `user` (with `QuotaSpec`) in context.
2. **Unlimited key** → `Acquire` returns a sentinel reservation; no Redis, no leadership lookup; zero cost.
3. **Limited key** → resolve the **target footprint** from the requested spec/template (§6.3) **before** committing to a
   specific pooled instance, then one Lua `Acquire(lockstring, footprint, limits)`:
   - `OK` → proceed.
   - `REJECTED` → **HTTP 429 immediately, no retry**; if a pooled sandbox was tentatively picked, return it to the pool
     (do not lock it). `TryClaimSandbox` must surface the quota miss as terminal, not loop.
   - transport error → **fail-open** (allow) in Phase 1.
4. The `lockstring` is the one `LockSandbox` already stamps on the CR — no extra CR write, no infra interface change.
5. Run `ClaimSandbox` / `CloneSandbox`. On **any** failure → `Release(lockstring)` with `context.WithoutCancel` (§6.5
   path 1). On success → nothing; the entry already reflects the live sandbox.

`DELETE /sandboxes/{id}` issues a **pre-emptive** `Release(lockstring)` (§6.5 path 2) for low-latency slot return; the
leader's event handler (path 3) is the backstop for all non-manager deletions.

## 11. API Surface

- **Create** (`POST /sandboxes`): unchanged shape; quota enforced internally; exceeded → **429** with the E2B error body
  (no retry).
- **Key create** (`POST /api-keys`): optional nested `quota`; **admin-only** to set; validated (§3.1); rejected if no
  Redis (§6.1).
- **No quota `PATCH`** (§6.8): immutable after create; change quota by creating a new key.
- **Describe** (optional): per-dimension `live`/`limit` for a key, via `QuotaManager.Describe`. When Redis is
  unreachable or a key is mid-rebuild, `Describe` does **not** fabricate `live = 0`; it returns the limits with usage
  marked **stale/unknown** (or, if the caller requires it, an error) — consistent with the fail-open posture that
  enforcement (not reporting) degrades gracefully.
- **Key delete** (`DELETE /api-keys/{id}`): drop the quota config; **keep** existing sandboxes; single one-slot `DEL` of
  the key's three `{K}` Redis keys (§6.7), bounded non-blocking retry, non-fatal — this is the sole cleanup (no `SCAN`).

Authorization reuses `CheckCreateAPIKeyPermission`; quota set chains `CheckApiKey` + admin check.

## 12. Compatibility

- Old keys without `quota` → unlimited; new JSON field is `omitempty`.
- `CountActiveSandboxes` untouched; SandboxClaim self-healing preserved.
- No owner label and **no backfill**: anti-drift reads live CRs off the existing `IndexUser` informer index (§5).
- New RBAC: one generic lease (§8).
- New dependency: a Redis client (dormant unless configured). Phase 1 has **no global Redis keys**, so standalone /
  Sentinel / Cluster are all structurally compatible (§6.2).
- No change to E2B lifecycle semantics beyond create-time admission and the removal paths of §6.5.

## 13. Alternatives Considered

| Option | Mechanism | Hot-path IO | Multi-dim (cpu/mem) | 500k `List` cost | No oversell | Verdict |
|---|---|---|---|---|---|---|
| **Redis live-set, incremental, bidirectional anti-drift (rev2, chosen)** | per-lockstring idempotent Lua add/remove; leader diffs vs cluster as backstop | 1 Lua for limited keys; none for unlimited | natural (per-entry footprint + sums) | rare (only initial rebuild / after loss) | strict at admit (Redis healthy); bounded self-healing transients; fail-open default | Recommended — exact `live`, simplest correctness (idempotency replaces version guard), cheap at scale |
| Redis committed-counter + `resv` overlay + version-guarded reconcile (rev1) | atomic Lua acquire; periodic recompute of `committed` from `List` under an RV version guard | 1 Lua for limited keys | poor (committed is a count; cpu/mem needs a summed relist) | high (frequent relist is the source of `committed`) | strict normal-op; bounded residuals | Rejected — laggy/imprecise `live`, large version-guard surface, relist cost dominates at 500k |
| Per-shard Lease leader election + in-memory cell (rev0) | client-go election per shard; sharding; forwarding; settle | none | poor | n/a | yes | Rejected — large self-built distributed surface |
| MySQL atomic counter | row-lock conditional UPDATE per op | every op | poor | n/a | yes | Rejected — hot-row serialization; backend-specific |
| Informer-only (no shared store) | per-replica cache | none | — | — | no (cross-replica lag) | Rejected for enforcement |

## 14. Risks

- **Redis memory for the live set.** Storing every live sandbox (~200 B/entry → ~100 MB at 500k) vs rev1's aggregate
  counters. Acceptable for Redis; the same storage is what enables exact `live` and cheap multi-dim. Monitor memory and
  per-key `HLEN`.
- **Steady-state anti-drift still reads a (warm) view.** A periodic bidirectional diff must eventually compare Redis to
  truth to catch missed events; rev3 makes it **infrequent** and **entirely informer-served** (no apiserver `List`
  anywhere, not even for rebuilds — subjects come from the key store, live CRs from `IndexUser`). Monitor diff lag and
  divergence counts.
- **Large-key resync cost.** `HGETALL` and the atomic `HVALS`-based `resyncSums` are single-threaded Redis ops whose cost
  grows with a key's live-set (quota-bounded, but a very large quota makes them heavy and competes with hot-path
  `Acquire`). Mitigation: run `resyncSums` on a slow cadence (it only repairs rare external sum-loss), document a
  max-supported per-key live-set envelope, and monitor resync/`HGETALL` latency (§6.5.2).
- **Deletion-requested = freed transient over-actual** (§6.5.1). A stuck-terminating sandbox plus a replacement can
  briefly exceed `limit` in physical pods. Deliberate latency trade; bounded by concurrent terminations; not resurrected
  by anti-drift (live-CR-only add).
- **Eager release biases to transient over-admission.** Releasing on any create failure (incl. ambiguous post-CR-write
  failures) can drop a live CR's charge until anti-drift re-adds (≤ grace). Chosen over depending on the backstop;
  bounded and self-healing.
- **Fail-open during Redis outage = temporary no enforcement.** Bounded oversell for the outage duration, self-healed by
  rebuild. The deferred fail-closed knob trades availability for strictness (§9).
- **Clone must stamp a lockstring.** Quota keys every entry off the lockstring; the clone path historically stamped only
  `AnnotationOwner` (`clone.go:303`). rev3 fixes clone to stamp one (§1/§5); any future owner-stamping path must do the
  same, or its sandboxes are untracked. No owner label / backfill is needed (reads go through `IndexUser`).
- **Footprint resolution for `TemplateRef`.** Referenced templates are **treated as immutable for quota** (§6.3), so
  anti-drift add recomputes the identical footprint; a resolution failure must fail safe (defer the add; never charge
  zero). If a deployment mutates a referenced template in place, the recompute could diverge — out of scope by the
  immutability rule; a future footprint-on-CR annotation (§17) would remove the dependency entirely.
- **Deleted-key `DEL` failure.** If the key-delete `DEL` (§6.7) and its bounded retry both fail, the key's entries leak
  as never-read dead memory (no `SCAN` reclaims them). It is correctness-harmless (deleted keys are never re-enumerated
  and IDs never reused), but **not** availability-harmless: the leak is bounded by Σ (live-set size of each deleted
  limited key whose `DEL` failed), and unbounded *growth* could pressure Redis memory and so widen the fail-open window.
  Mitigation: alert on Redis memory and on `DEL`-failure count; an optional tombstone-retry queue (no `SCAN`) is a
  future hardening.
- **Lockstring uniqueness/stamping.** Correctness keys off the lockstring being globally unique and present on every
  owner-stamped CR (it is, via `LockSandbox`). Any future create path stamping `owner` must also go through `Acquire`
  with that lockstring (§7 precondition).
- **`unlimited → limited` transition oversell** — eliminated by the no-`PATCH` scope decision (§6.8).

## 15. Acceptance Criteria

- Concurrent creates for one limited key across replicas never exceed `count`/`cpu`/`memory` limits while Redis is
  healthy; multi-dimensional admission is all-or-nothing (a create that fits cpu but not memory is rejected and charges
  nothing).
- Idempotent acquire: a retried `Acquire` with the same lockstring does not double-charge any dimension.
- Eager + pre-emptive release: a create failure releases the charge; `DELETE` returns the slot pre-emptively; both are
  idempotent with the leader's event-driven release (no double-decrement below zero).
- Deletion-requested = freed: a `DeletionTimestamp`-set / Terminating owner CR no longer counts; anti-drift does **not**
  re-add it.
- Bidirectional anti-drift: subjects enumerated from the key store; (a) an entry whose CR is gone is removed after grace;
  (b) **a live CR missing its entry is re-added after grace**; (c) `resyncSums` recomputes sums from membership so a
  simulated entry **or sum-key** loss converges to **exact live-set membership and sums** (note: usage may legitimately
  remain `> limit` after a fail-open over-admission — anti-drift converges to *truth*, and further creates are rejected
  until usage falls below limit; quota never drains existing sandboxes). Every remove-capable pass is gated on informer
  health (a cold/unsynced/relisting cache never removes a live slot); a key-store enumeration error skips the cycle
  without treating the subject set as empty. Both directions run only on the leader and are safe under a flapping leader
  (idempotent, no version guard).
- New key needs no seed: first `Acquire` on an absent `q:live:{K}` charges from zero (no `NEED_SEED`, no per-acquire
  consistent read).
- Fail-open: with Redis unreachable, limited keys are allowed (unenforced) and unlimited keys provably do zero Redis IO;
  after Redis returns, the leader rebuild restores exact `live`/sums and enforcement resumes.
- No quota `PATCH`; a newly-created limited key enforces from its first `Acquire`; no replica observes a limited key as
  "unlimited" (unknown key → auth failure).
- 429 with no retry: a quota miss returns immediately and a tentatively-picked pooled sandbox is returned to the pool.
- Footprint: cpu in millicores and memory in MB (ceil) summed from **regular**-container requests (init/sidecars
  excluded, §6.3); `TemplateRef` is resolved; a
  resolution failure fails safe.
- Sum/membership consistency: every add/remove changes the HASH field and the affected sums in one Lua; sums never drift
  from `HLEN`/membership under churn.
- `Describe` reports correct per-dimension `live`/`limit`.
- Validation (§3.1): `limit = 0` blocks all creates in that dimension **including a zero-footprint create** (the Lua
  treats `lim == 0` as hard-zero); an all-nil-limits quota is normalized to unlimited; negative / duplicate /
  unknown-dimension / non-empty-scope / non-empty-quota-without-Redis are rejected at key create.
- Footprint invariant (§6.3): every sandbox carries a positive cpu and memory request, so a cpu/memory-only-limited key's
  live-set is bounded and cannot accumulate unbounded zero-footprint entries.
- `CountActiveSandboxes` and SandboxClaim self-healing unchanged (regression).
- `IsPrimary()` gates only the anti-drift sweep + event-driven release; correctness holds with the sweep forced on all
  replicas (idempotency regression test).
- Lockstring stamped on **every** create path (claim/create **and clone**); the anti-drift driver lists a key's live set
  via `IndexUser` (informer, never APIReader) and returns the correct set; no owner label, no backfill.
- Redis topology: Phase 1 Lua touches only `{K}`-tagged keys (no `CROSSSLOT`); standalone/Sentinel verified, Cluster
  structurally compatible.
- Table-driven unit tests for `QuotaManager` (acquire/release/describe, multi-dim, idempotency, fail-open) and the
  anti-drift driver (both directions, `resyncSums`, grace, cache-sync gate, key-store enumeration, leader-gating), plus
  create-path integration.

## 16. Resolved Decisions & Implementation Discretion

### Resolved (product / architecture)

- **Model:** Redis holds the **live set** (one entry per live sandbox, keyed by lockstring, carrying footprint),
  maintained incrementally; cluster is a backstop. `live` = exact membership/sums; **no committed-counter recompute, no
  version guard** (replaced by per-lockstring idempotency).
- **Identity:** the existing **lockstring** (`AnnotationLock`, UUID from `LockSandbox`), stamped on **every**
  owner-stamping path — claim/create already do; **clone** is fixed to stamp one too (§1/§5). No new `opID` annotation.
- **Dimensions (Phase 1, all enforced):** `sandbox.count`, `cpu` (millicores), `memory` (MB), from **regular**-container
  **requests** (init/sidecars excluded, §6.3), computed from the declared target spec at create. Model extensible to
  `limits.*` / per-template scope (reserved, not enforced; unknown dimension/non-empty scope **rejected**). **Footprint
  invariant:** every sandbox has a positive cpu+memory request (§6.3), keeping any limited dimension's live-set bounded;
  a `limit == 0` dimension is a hard-zero that the Lua rejects unconditionally (even a zero-footprint create). An
  all-nil-limits quota is normalized to unlimited; a "limited key" has ≥1 non-nil limit.
- **No post-create 变配 in the manager:** footprint is fixed at create; **no informer update-event footprint tracking**.
  If a create chooses an in-place update of a pooled sandbox, charge the **declared** target resources. Referenced
  `SandboxTemplate`s are **immutable for quota**, so anti-drift recomputes the identical footprint (§6.3).
- **Removal:** eager release on **any** create failure; **pre-emptive** release on manager `DELETE`; leader event-driven
  release for non-manager deletions; leader bidirectional anti-drift backstop. **Deletion-requested = freed** (do not
  wait for CR invisibility). The single **quota-live predicate** `isLiveForQuota` frees a slot iff `DeletionTimestamp !=
  nil` or phase `Terminating`; Failed/Succeeded-but-not-deleted **still occupy** — deliberately **narrower** than
  `CountActiveSandboxes`'s Dead-exclusion, so quota uses its own owner read (§5).
- **Anti-drift is bidirectional** (add missing live-CR entries; remove leaked entries; `resyncSums` recomputes sums from
  membership) — required so lost entries/sums self-heal rather than drift forever. Add considers `isLiveForQuota` CRs
  only; the remove age-gate uses the entry `ts` (Redis clock). **Grace = 10 minutes.** The **driver lives in the quota
  layer** and enumerates limited keys from the **key store** (Redis-independent → survives total loss); the **live-CR
  read primitive** is in `pkg/cache` over `IndexUser`. All **Sandbox CR** reads are **informer-only, never APIReader**
  (key-store enumeration uses the durable backend); **every remove-capable pass is gated on informer health**. **No owner
  label, no backfill.**
- **No seed:** absent `q:live:{K}` for a new key == zero; `NEED_SEED` deleted.
- **Redis unavailable (Phase 1): fail-open** (treat as unlimited; rely on rebuild). Fail-closed posture + its
  `q:warm`/settle/`WAIT` machinery is **deferred** to a later PR (config knob `onRedisUnavailable`, default `allow`).
- **Quota immutable** after key create (no `PATCH`); admin-only to set; subject is always the API key. **`unlimited →
  limited` is forbidden forever** — even a future `PATCH` may only change a *limited* key's limits, never promote an
  unlimited key — so the hot path's "unlimited ⇒ bypass Redis" assumption can never be invalidated (§6.8).
- **Backend:** Redis, pluggable/optional; no Redis ⇒ unlimited and non-empty quota rejected.
- **Reconciler placement:** leader-gated **driver in `pkg/servers/e2b/quota`** (event handler registered into
  `pkg/cache`); subjects enumerated from the key store; generic `IsPrimary()` lease; correctness via idempotency, not
  leadership.
- **All Redis writes are single atomic Lua (or one idempotent command) and per-lockstring idempotent**, guarded by
  `HEXISTS`/`HGET`; sums and membership are always co-mutated in the same script.
- **Error codes:** quota exceeded **429** (no retry).
- **Topology:** Phase 1 has no global Redis keys → standalone/Sentinel/Cluster structurally compatible.

### Left to the implementing agent

- Redis key prefixes, hash-tag form (`{K}`), Lua script encoding, and the footprint JSON shape stored in the entry.
- The `footprintOf(podSpec)` helper (regular-container iteration — init/sidecars excluded per §6.3 — MB ceil rounding, `TemplateRef`
  resolution and its fail-safe behaviour).
- Redis client choice and config wiring (`pkg/sandbox-manager/config` / `clients`, `cmd/sandbox-manager`): pooling,
  retry/back-off, acquire timeout, and the fail-open error classification.
- The generic leadership lease name and `leaderelection` parameters; `IsPrimary()` exposure on `SandboxManager`.
- Anti-drift cadence values, the key-store enumeration of limited keys, and how missed-event divergence is monitored.
- External nested JSON shape for `quota`.
- Where exactly clone stamps its lockstring (the `Modifier` at `create.go:200` vs `newSandboxFromTemplate`), reusing
  `NewLockString()`.
- Where exactly `Acquire`/`Release` hook into `TryClaimSandbox` / clone / `DELETE`, and how a tentatively-picked pooled
  sandbox is returned to the pool on a 429.
- The `ListLiveSandboxEntriesByOwner` signature in `pkg/cache` (over `IndexUser`) and the `resyncSums` Lua encoding.

## 17. Deferred / Future Work

- **Fail-closed posture** (`onRedisUnavailable: reject`): `q:warm` total-loss detector, Redis-clock `q:warmAt` settle
  window, `COLD → 503`, and synchronous reservation replication (`WAIT` / sync topology) for strictness across failover.
  Reintroduces global Redis keys (reopens the Cluster `CROSSSLOT` question).
- **`limits.cpu` / `limits.memory` dimensions** and **per-template scope** (model already reserves them).
- **Post-create 变配** (if the manager ever exposes it): an informer update-event handler that adjusts the entry
  footprint and sums by the delta, plus optional admission gating of the resize against quota.
- **Quota `PATCH`** for *already-limited* keys (limit changes only — never `unlimited → limited`, §6.8/§16) with a
  safe-activation scheme (activation window or all-replica `QuotaSpec` cache invalidation).
- **Footprint annotation on the CR** — pure optimization to skip template resolution on the anti-drift add path (no
  longer a correctness item, since referenced templates are immutable for quota, §6.3).
- **Apiserver-side fencing** (a validating webhook rejecting a Sandbox create whose reservation is unknown/expired) for
  absolute 0-oversell under failure intersections — out of scope by product decision.
- **Official Redis Cluster support** (testing + the deferred-posture hash-tag redesign).
