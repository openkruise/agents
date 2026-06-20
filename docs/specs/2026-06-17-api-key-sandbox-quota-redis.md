# API Key Sandbox Quota — Design Spec (Redis-backed, count-only live-set)

- Date: 2026-06-19 (rev5)
- Branch: `feature/e2b-api-quota-260617`
- Supersedes:
  - rev0–rev2: the per-shard Lease leader-election design and the Redis **committed-counter +
    reservation-overlay + version-guarded reconcile** design (deleted).
  - rev3: the Redis **live-set** design that enforced **count + cpu + memory** with a per-entry resource
    **footprint**, `q:sum:cpu/mem:{K}` keys, an atomic `resyncSums`, and a `TemplateRef`-immutability
    requirement; defaulted to **fail-open**; and reclaimed slots with **eager** release on any create failure
    and **pre-emptive** release on delete.
  - rev3 → rev4, driven by a scope/consistency review:
    1. **Phase 1 scope cut to `sandbox.count` only.** The cpu/memory dimensions, the per-entry footprint, the
       `q:sum:*` keys, `resyncSums`, and the `TemplateRef`-immutability requirement are **removed from Phase 1**
       and reserved as future extension points (§16). The Redis live set is now a **count-only membership map**
       (lockstring → acquire-timestamp); `count = HLEN`. No footprint is computed at admit, so the create hot
       path never resolves container resources or templates.
    2. **Redis is the sole quota backend.** No MySQL-native counter and no pluggable alternative store; the
       "Alternatives Considered" section is dropped.
    3. **Quota is settable even without Redis.** A non-empty quota is **no longer rejected** when Redis is
       absent — it is simply **unenforced** (fail-open), exactly as during a Redis outage. The stored limit is
       still persisted and surfaced by `Describe` (usage marked stale/unknown).
    4. **Fail-open is an explicit, confirmed product decision** — not an unstated default. The guarantee is
       restated as **strict-at-admit while Redis is healthy**; the fail-closed posture stays deferred (§16).
    5. **Strict, no-over-admission-while-healthy release.** Eager-release-on-any-failure and
       pre-emptive-release-before-deletion are **removed**. An **ambiguous** create failure **keeps** the charge
       (the bidirectional anti-drift removes a truly-leaked entry after grace); manager `DELETE` releases **only
       after the deletion is accepted** (the CR has a `DeletionTimestamp` / is Terminating) or an informer event
       confirms it.
  - rev4 → rev5 (this revision), driven by an implementation-planning review:
    **Create-failure release widened from provably-pre-CR to also cover cleanup-deleted.** §6.4 path 1, the
    acquire hook (step 5), and the §15 classification now release the charge not only when the failure is
    provably **before any CR write**, but also when the attempt **did create a CR and the failed-sandbox cleanup
    successfully requested its deletion** (so it is no longer `isLiveForQuota`, matching path 2). **Ambiguous**
    failures (a CR may have been committed and deletion is unproven — e.g. a post-write timeout, or a reserved
    failed Sandbox that still counts) **keep** the charge for anti-drift. Because this only releases
    provably-gone slots, it tightens the bounded under-sell window without weakening the
    no-over-sell-while-healthy guarantee.
- Scope: `pkg/servers/e2b/` (create, api_key, models, keys, routes), `pkg/servers/e2b/quota/` (new:
  `QuotaManager`, the Redis backend, the count-only Lua, and the leader-gated anti-drift **driver**),
  `pkg/sandbox-manager/` (api/infra wiring, a generic `IsPrimary()` leadership capability),
  `pkg/sandbox-manager/config` + `clients` (Redis config/client), `pkg/cache/` (an additive live-CR read
  primitive `ListLiveLockstringsByOwner` over the existing `IndexUser` field index, plus event-handler
  registration for event-driven release), `cmd/sandbox-manager/`, `config/`/Helm chart (RBAC for
  `coordination.k8s.io/leases`, Redis config), `go.mod`/`vendor` (add a Redis client).

## 1. Background

sandbox-manager is a stateless, multi-replica backend exposing E2B/MCP APIs to manage sandboxes. Each request
authenticates via `X-API-KEY`, resolved to a `*models.CreatedTeamAPIKey` (with `ID uuid.UUID` and `Team`). That
`ID` is the sandbox **owner**: every create path stamps it onto the Sandbox CR as
`agentsv1alpha1.AnnotationOwner` (claim and create-on-no-stock via `utils.LockSandbox`, clone via the create
`Modifier`), and it surfaces as `route.Owner`.

We need to cap **how many sandboxes a single API key may hold** (Phase 1: count only), enforced across replicas
without materially slowing down create. Peak create throughput is ~2500/sec (150k/min) aggregate; a single
cluster may hold ~**500k** sandboxes; the apiserver may lag by up to ~**1 minute**. The worst case for a single
limited key is "small limit + high churn" (constantly creating and deleting against a tiny limit), which rules
out any per-operation external IO on the **unlimited** hot path and demands a cheap, contention-free check on
the **limited** hot path, plus timely slot reclamation.

### Why a Redis live set (count-only)

Redis holds the **live set itself** — one entry per live sandbox, keyed by its **lockstring** — so:

- `live` is the *literal membership*: `count = HLEN q:live:{K}`. No counter recompute, no relist, no lag.
- Every mutation is a **targeted, per-lockstring, idempotent** op (add this sandbox / remove this sandbox),
  never a read-modify-write of a shared counter. **Idempotency keyed by a stable per-sandbox identity replaces a
  version guard.** Concurrent / retried / leadership-handoff / double-fired writes all converge.
- A full cluster `List` is needed only by a **rare** backstop rebuild, not on every reconcile tick. Steady state
  is incremental plus an infrequent leader-side diff against the warm informer.

### Key facts established from the codebase

- `pkg/utils/utils.go:201` `LockSandbox(sbx, lock, owner)` stamps `AnnotationLock = lock` (a UUID from
  `NewLockString()` / `uuid.NewString()`, `utils.go:211`) **and** `AnnotationOwner = owner` on the **same** CR
  write, used by claim/create (`performLockSandbox`, `claim.go:661`). **The lockstring is an existing
  per-sandbox UUID persisted on the CR** — quota reuses it as the Redis entry key; no new annotation is
  introduced. **Gap to fix:** the **clone** path (`newSandboxFromTemplate`, `clone.go:303`) currently stamps
  only `AnnotationOwner`, *not* a lockstring; rev4 makes clone stamp one too (via its `Modifier`,
  `create.go:200`, using `NewLockString()`) so **every** owner-stamped CR carries a lockstring (§5).
- `pkg/cache` is the sandbox-manager-only, informer-backed cache. It already runs informers, exposes
  `CacheSandboxCustomReconciler` + `AddReconcileHandlers()` for external event handlers, and maintains an
  `IndexUser` field index over `AnnotationOwner` (`index.go:83`, for both Sandbox and Checkpoint). rev4 adds
  only a live-CR read primitive `ListLiveLockstringsByOwner` here and lets the quota layer register an event
  handler; the anti-drift **driver** itself lives in `pkg/servers/e2b/quota` (§6.4.2). Anti-drift never uses
  `GetAPIReader` — all its **Sandbox CR** reads are informer reads, gated on cache health (subjects come from
  the key store, not the apiserver; §6.4.2).
- `CountActiveSandboxes` **excludes** `Dead` (`cache.go:306`) and is relied on by the SandboxClaim controller.
  **It must not be modified.** Quota uses its own additive owner read instead (§5).
- `k8s.io/client-go/tools/leaderelection` (Lease-backed) is already vendored — reused for a single generic
  `IsPrimary()` capability (§8).
- No Redis client is vendored yet; rev4 adds one (e.g. `github.com/redis/go-redis/v9`) behind a backend
  interface.
- The create hot path is `createSandboxWithClaim` → `ClaimSandbox` and `createSandboxWithClone` →
  `CloneSandbox` (`create.go`); the `Modifier` closures (`create.go:119` / `:200`) are the existing stamping
  hook where `Acquire`/`Release` wrap the call.

## 2. Goals & Non-Goals

### Goals

- Enforce a per-API-key limit on the **count** of sandboxes held, **strict at admit while Redis is healthy**,
  across replicas.
- Make `live` **exact and lag-free in steady state** by storing the literal live set (lockstrings) in Redis and
  maintaining it incrementally — no periodic counter recompute, no frequent full `List`.
- Keep enforcement correct under concurrency/retry/leadership-handoff using **per-lockstring idempotent, per-op
  atomic** Redis writes (Lua) — no version guard.
- Unlimited keys perform **zero** Redis IO on the hot path. Limited keys perform **at most one** Redis
  round-trip per acquire.
- The cluster remains the **ground truth**; the Redis live set is reconstructible from it and self-heals via a
  **bidirectional** anti-drift diff (add entries for live CRs missing from Redis; remove entries whose CR is
  gone).
- **No over-admission while Redis is healthy:** release is conservative (keep the charge on an ambiguous create
  failure; free a slot only once deletion is accepted), so a healthy-state transient can only be a bounded
  *under-sell*, never an over-sell.
- Backward compatible: keys without a quota field default to **unlimited**.
- The quota **data model is extensible** (cpu/memory dimensions, per-template scope) without a schema change,
  but Phase 1 enforces only `sandbox.count`.

### Non-Goals (Phase 1)

- **cpu / memory dimensions, per-template scope, per-team scope.** The quota subject is always the API key.
  These are reserved model extension points and are **rejected at key-create validation** in Phase 1 (§3.1), so
  a not-yet-enforced dimension is never silently accepted.
- **Per-entry resource footprint / Kubernetes effective-pod-request accounting.** Count-only enforcement needs
  no resource math, so Phase 1 reads no container resources and resolves no templates on any path. (Reintroduced
  with the cpu/memory dimensions in §16.)
- **Post-create resize / 变配 admission.** sandbox-manager exposes no post-create resource-change capability;
  count is unaffected by resizes regardless.
- **Strict no-oversell across Redis unavailability.** Phase 1 is **fail-open** on Redis trouble (§9) — a
  **confirmed product decision** trading enforcement for availability: during a Redis outage limited keys are
  temporarily unenforced (treated as unlimited), accepting a bounded oversell that the leader's anti-drift
  rebuild self-heals once Redis returns. The **fail-closed** posture (and its `q:warm`/settle/`WAIT` machinery)
  is deferred (§16).
- Reclaiming/evicting existing sandboxes; quota only blocks new creates.
- Quota mutability: settable only at **key create**, immutable thereafter — **no `PATCH`** (§6.7 eliminates the
  `unlimited → limited` transition oversell by construction). This is a deliberate Phase 1 scope-down.
- Billing/usage reporting; cross-cluster/multi-region quota; a per-op consensus log or apiserver-side fencing
  webhook.

## 3. Quota Data Model (static)

Addressed by `(apiKeyID, dimension, scope)`. Phase 1 enforces exactly one dimension, `sandbox.count`, at
`scope = {}` (per-api-key). The model reserves the other dimensions/scopes for forward compatibility but does
**not** enforce them.

```go
type QuotaDimension string
const (
    DimSandboxCount QuotaDimension = "sandbox.count" // unit: sandboxes — the ONLY Phase 1 dimension
    // reserved (NOT enforced in Phase 1; rejected at validation, §3.1):
    //   "cpu"    — millicores
    //   "memory" — MB
    //   "limits.cpu", "limits.memory", ...
)

// Scope narrows where a limit applies. Subject is always the API key (never a team).
// Phase 1: empty == per-api-key (the only accepted scope). Template is a forward extension point only.
type QuotaScope struct {
    Template string `json:"template,omitempty"` // future per-template scope (rejected in Phase 1)
}

type QuotaLimit struct {
    Dimension QuotaDimension `json:"dimension"`
    Scope     QuotaScope     `json:"scope,omitempty"`
    Limit     *int64         `json:"limit,omitempty"` // nil == unlimited
}

type QuotaSpec struct {
    Limits []QuotaLimit `json:"limits,omitempty"` // empty/absent == fully unlimited
}
```

- **External JSON** is nested and extensible, e.g. `"quota": { "sandbox": { "count": 50 } }`. The handler
  normalizes it into `QuotaSpec.Limits`. The exact external shape is an implementation detail provided it stays
  nested (so cpu/memory can be added later without breaking it).
- `QuotaSpec` is loaded at auth time (`CheckApiKey` puts `user` in context), so the hot path never re-reads the
  key store.

### 3.1 Validation (only at key create — Phase 1 has no quota `PATCH`)

A quota is **never silently ignored or silently accepted**:

- **Absent / `null` quota** (or empty `Limits`) → **unlimited**.
- **`Limit == nil`** for a dimension → that dimension **unlimited**.
- **All present limits `nil`** → **normalized to unlimited / absent at write time**, so a "limited key" is
  defined uniformly as *having ≥1 non-nil limit* (the same definition the hot path and the anti-drift driver
  use, §6.4.2).
- **`sandbox.count` `Limit == 0`** → **valid** hard-zero (every create returns 429).
- **`Limit < 0`** → **rejected**.
- **Duplicate `(dimension, scope)`** → **rejected**.
- **Any dimension other than `sandbox.count`** (cpu, memory, `limits.*`, …) → **rejected in Phase 1** (reserved,
  not yet enforceable), so a future dimension is never silently dropped or silently honored.
- **Non-empty `scope`** → **rejected in Phase 1**.
- **No constraint on Redis presence.** A non-empty quota is **accepted regardless of whether Redis is
  configured**; if Redis is absent (or later unavailable) the limit is simply **unenforced** (fail-open, §6.1),
  and `Describe` reports the stored limit with usage stale/unknown (§11). The quota is persisted either way.

## 4. Static Config Storage

Both key backends store **only** the static `QuotaSpec`, alongside the key, written once at **key create**
(immutable thereafter, §6.7). **No dynamic usage is ever written to the key store** — that lives in Redis.

- **Secret backend:** store the internal normalized `QuotaSpec` in the per-key JSON inside `e2b-key-store`;
  public API request/response structs use a separate nested wire model (`"quota": {"sandbox": {"count": N}}`)
  and convert to/from `QuotaSpec` at the handler/storage boundary. Old payloads without quota decode to
  `nil` == unlimited; writes reuse the existing `retryUpdateSecret` CAS. (The single `e2b-key-store` Secret is
  suited only to **static** config — it cannot host per-create dynamic counters at 2500/sec; that is exactly why
  dynamic state lives in Redis, not the Secret.)
- **MySQL backend:** add a nullable `quota JSON` column to `team_api_keys` (`NULL` == unlimited); `AutoMigrate`
  adds it, gated by `DisableAutoMigrate` with a documented manual DDL alternative.

## 5. Identity, Owner Index, and Counting Primitive

- **Sandbox identity in Redis = the lockstring** (`AnnotationLock`, a UUID stamped by `LockSandbox`). It is
  globally unique and persisted on the CR, so it survives `GenerateName` (the create path need not know the
  final object name). Re-keying every Redis op off it gives idempotency for free. **Every owner-stamped create
  path must stamp a lockstring:** claim/create already do (`LockSandbox`); rev4 adds it to **clone** (via the
  `Modifier`, `create.go:200`, using `NewLockString()`), closing the one path (`clone.go:303`) that previously
  set only `AnnotationOwner`. This is a hard precondition (§7).
- **Owner read uses the existing `IndexUser` field index — no owner label, no backfill.** `pkg/cache` already
  indexes `AnnotationOwner` (`index.go:83`), so the anti-drift driver lists a key's live CRs with
  `cache.List(MatchingFields{user: K})` directly off the informer. Because all anti-drift reads are informer
  reads (never APIReader, §6.4.2), no server-side label selector is required, so rev4 adds **no** owner label
  and needs **no** one-time backfill. The index covers every CR carrying `AnnotationOwner`, including clones
  once they are owner-stamped.
- **Quota "live" predicate — the single source of truth for counting, used identically by the count read and by
  both anti-drift directions (§6.4.2):**

  ```go
  // isLiveForQuota reports whether a Sandbox still occupies its owner's quota.
  // Freed (NOT live) iff deletion has been requested or is in progress; a merely
  // Failed/Succeeded-but-not-yet-deleted sandbox still occupies quota until it is.
  func isLiveForQuota(sbx *agentsv1alpha1.Sandbox) bool {
      return sbx.DeletionTimestamp == nil &&
          sbx.Status.Phase != agentsv1alpha1.SandboxTerminating
  }
  ```

  This predicate is **deliberately narrower** than `CountActiveSandboxes`'s "not `Dead`" filter (which *also*
  excludes Failed/Succeeded): quota frees a slot only when deletion is requested/terminating, **not** the moment
  a pod fails. That difference is why quota uses its own additive read (over `IndexUser`) rather than
  `CountActiveSandboxes` (left untouched, §1). The exact field/enum (`Status.Phase == SandboxTerminating`,
  `sandbox_types.go:262`) is confirmed at implementation; the **semantics** above are fixed. We do **not** wait
  for the CR to become invisible.

  ```go
  // ListLiveLockstringsByOwner returns, for owner K, the lockstring of every Sandbox CR
  // with isLiveForQuota == true. Reads the warm informer via MatchingFields{user: K}
  // (IndexUser). Never APIReader; the caller invokes it only after the cache is healthy
  // (§6.4.2), so an unsynced cache can never look "empty".
  func (c *Cache) ListLiveLockstringsByOwner(ctx, opts) ([]string, error)
  ```

  (Count-only needs just the lockstrings — no per-entry footprint.)

## 6. Counting Model (Redis count-only live-set)

All enforcement lives in a `QuotaManager` (package `pkg/servers/e2b/quota`) behind a `QuotaBackend` interface,
with a `redisBackend` and a `noopBackend`.

```go
type QuotaManager interface {
    // Acquire charges one sandbox, or returns ErrQuotaExceeded (429). Unlimited keys
    // are a no-op (zero Redis IO). Idempotent on the lockstring.
    Acquire(ctx, req AcquireRequest) (Reservation, error)
    // Release returns a charged sandbox. Idempotent. Issued with context.WithoutCancel.
    Release(ctx, req ReleaseRequest) error
    // Describe reports current count usage/limit for read APIs.
    Describe(ctx, apiKeyID string) (QuotaStatus, error)
}
// AcquireRequest carries apiKeyID, lockstring, and the loaded QuotaSpec (count limit).
```

The **anti-drift / removal driver** (leader-gated, §6.4) is not part of the request-serving interface; it lives
in the quota layer (`pkg/servers/e2b/quota`, §6.4.2) and reuses the same Lua helpers. `pkg/cache` only exposes
the owner-indexed live-CR read (`ListLiveLockstringsByOwner`) and accepts the registered event handler — it
never drives reconcile.

### 6.1 Backend selection (pluggable, optional Redis)

- **Redis configured** → `redisBackend`: full enforcement.
- **Redis not configured** → `noopBackend`: `Acquire` always allows (fail-open), `Release` / anti-drift are
  no-ops. Setting a **non-empty** quota at key create is **still accepted** (§3.1) and persisted; it is simply
  unenforced while no Redis exists. `Describe` returns the **stored limits with usage marked stale/unknown** —
  it does **not** fabricate a usage of 0 (which would falsely imply enforcement). This is identical to the
  Redis-configured-but-unavailable case (§9), so "Redis absent" and "Redis down" behave the same.

### 6.2 Redis keys (per limited key K; hash-tagged `{K}`)

| Redis key | Type | Writer(s) | Meaning |
|---|---|---|---|
| `q:live:{K}` | HASH | Acquire / anti-drift **add** (HSET); Release / event / anti-drift **remove** (HDEL) | field = **lockstring**, value = `<redis-unix>` (Redis server clock at acquire). Membership = the live set; `count = HLEN`; the value is **only** the entry age used by the anti-drift remove gate (§6.4.2) — there is no footprint and no sum |

`live(K)` = **`HLEN q:live:{K}`**. There is a **single** Redis key per limited key.

> **No global keys, no sum keys in Phase 1.** Because Phase 1 is fail-open (§9) there is **no**
> `q:warm`/`q:warmAt`/settle barrier, and because it is count-only there is **no** `q:sum:*`. Every Lua touches
> only the one per-`{K}` key, so with the `{K}` hash-tag everything for a key co-locates in one slot — **Redis
> Cluster causes no `CROSSSLOT`** (standalone / Sentinel / Cluster are all structurally compatible; officially
> testing/supporting Cluster is a separate call). The global cold/warm barrier returns only with the deferred
> fail-closed posture (§16), at which point the Cluster question reopens.

### 6.3 Acquire (hot path) — atomic, idempotent

Single Lua, run on **every** replica (Redis is the serialization point; no leadership on the hot path):

```
-- KEYS[1]=q:live:{K}
-- ARGV[1]=lockstring  ARGV[2]=limCount   (limCount: 0 == hard-zero, >0 == cap; unlimited never reaches Lua)
if redis.call('HEXISTS', KEYS[1], ARGV[1]) == 1 then return 'OK' end             -- idempotent re-entry: already charged
local lim = tonumber(ARGV[2])
if lim == 0 then return 'REJECTED' end                                           -- hard-zero: reject every create
if lim > 0 and redis.call('HLEN', KEYS[1]) + 1 > lim then return 'REJECTED' end  -- at capacity
redis.call('HSET', KEYS[1], ARGV[1], redis.call('TIME')[1])                      -- value = acquire ts (Redis clock, skew-free)
return 'OK'
```

- **Unlimited key** (no `QuotaSpec`, or every limit nil) → `QuotaManager.Acquire` short-circuits **before any
  Redis call** (zero IO) — the majority of the 2500/sec. The Lua only ever runs for a limited key, so an
  unlimited dimension never needs to be encoded.
- `OK` → proceed with create; the lockstring is the one already stamped on the CR (`LockSandbox`), so no extra
  CR write.
- `REJECTED` → HTTP **429**, returned **immediately with no retry** (the create path must not loop on a quota
  miss; a pooled sandbox tentatively picked before the charge must be returned to the pool — see §10).
- **Idempotent re-entry:** a retry with the same lockstring finds the field present → `OK` without
  double-charging. Lockstrings are fresh per-create UUIDs, never reused, so the `HEXISTS` short-circuit cannot
  miscount.
- **Redis transport error / unreachable** → **fail-open** (treat the key as unlimited for this request; allow).
  Confirmed Phase 1 product decision; a config knob to fail-closed instead is deferred (§9/§16).

### 6.4 Removal — conservative, event-driven, and a leader backstop

Release is an atomic, idempotent Lua that removes the entry:

```
-- KEYS[1]=q:live:{K}; ARGV[1]=lockstring
return redis.call('HDEL', KEYS[1], ARGV[1])   -- 1 removed / 0 already gone (idempotent)
```

A slot is freed only once its sandbox's deletion is **accepted or proven** — never speculatively. Paths (all
idempotent, safe to overlap):

1. **Create failure → conservative release.** The replica handling the create releases the charge **only when
   it is provable that no live CR remains charged by this attempt**: either the failure occurred strictly
   before any CR write, or the attempt did create a CR and the failed-sandbox cleanup successfully requested
   deletion for that CR (so it is no longer `isLiveForQuota`, matching path 2). On an **ambiguous** failure (a
   CR may have been committed and cleanup did not prove deletion acceptance — e.g. a post-write timeout), it
   **keeps** the charge. If no live CR in fact exists, the bidirectional anti-drift (§6.4.2) removes the leaked
   entry once its age exceeds grace; if a live CR does exist, the charge was correct. The trade is a bounded
   **under-sell** (over-rejection) for up to grace, never an over-sell — the safe direction.
2. **Manager `DELETE /sandboxes/{id}` → release after deletion is accepted.** The handler issues the delete and
   releases the lockstring **only after the apiserver has accepted the deletion** (the CR has a
   `DeletionTimestamp` / has entered Terminating → no longer `isLiveForQuota`). This is **not** pre-emptive:
   the slot is genuinely free at release time, so it cannot over-admit. It is still low-latency — it does not
   wait for physical pod GC.
3. **Leader informer event** (covers non-manager deletions — TTL, `kubectl`, controller-driven): on a Sandbox
   owner-CR event where the CR transitions to **not** `isLiveForQuota` (deletion-requested / terminating) or is
   truly deleted → Release that lockstring. Runs **only on the leader** (`IsPrimary()`); idempotent w.r.t.
   paths 1–2.
4. **Leader anti-drift backstop** (§6.4.2): removes entries with no live CR after grace; adds entries for live
   CRs missing from Redis after grace.

#### 6.4.1 Why "deletion-requested = freed" is safe and bounded

A released sandbox that is stuck `Terminating` still physically exists until GC, so for the window between
deletion-acceptance and true deletion a key can have *physical pods* `> limit` (e.g. a stuck-terminating pod
plus a freshly created replacement). This is the deliberate trade for timely reclamation (§13). The **quota
charge stays correct** throughout — the terminating sandbox is uncharged because its deletion was accepted, and
the replacement is charged — so it is **not** a quota over-admission, only a transient excess of physical pods.
It is bounded by the number of concurrently terminating sandboxes and resolves when they truly disappear. The
anti-drift **add** path does **not** resurrect such entries (it only adds entries for `isLiveForQuota` CRs), so
it never fights an intended deletion.

#### 6.4.2 Bidirectional anti-drift (leader-gated driver in `pkg/servers/e2b/quota`)

This is the **single** correction primitive; it makes drift in **either** direction self-heal. Critical: in an
incremental live-set model the backstop must be **bidirectional** — a "remove-only" GC would let a lost entry
(Redis restart / async-failover / rollback that drops an entry whose CR exists) undercount **forever**, since
nothing re-adds it → permanent oversell.

**Placement & layering.** The reconcile **driver** lives in the quota layer (it owns the key store and the
Redis backend); `pkg/cache` only exposes the live-CR read primitive `ListLiveLockstringsByOwner` (§5, over
`IndexUser`). The driver imports nothing from cache beyond that read; the event-driven release (path 3) is a
closure owning the `QuotaManager` **registered into** the cache via `AddReconcileHandlers()` (cache never
imports quota — no cycle). Both the periodic diff and the event handler run only while `IsPrimary()` (§8).

**Enumerating the subjects.** The driver enumerates the **limited keys** (those whose stored `QuotaSpec` has
≥1 non-nil limit — the same definition the hot path uses, §3.1) from the **key store** — the durable,
Redis-independent source, which is exactly why it survives a total Redis loss (an empty Redis cannot tell you
which keys *should* exist). Redis is never `SCAN`-ed to discover subjects (that would break on total loss and
on Cluster). Enumeration cost is backend-specific: the **Secret** backend reads the single `e2b-key-store`
object once and filters `Quota != nil` in memory; the **MySQL** backend filters server-side
(`WHERE quota IS NOT NULL`). Full-rebuild duration after a Redis loss is bounded by `#limited-keys × per-key
work`; the implementation paginates/budgets per cycle, jitters, and exports diff-lag / rebuild-duration /
divergence metrics (§15). For each enumerated key K:

1. `live := cache.List(MatchingFields{user: K})` filtered by `isLiveForQuota` → the authoritative set of
   lockstrings (informer only).
2. `have := HGETALL q:live:{K}` → the current Redis entries (lockstring → acquire-ts).
3. Diff:
   - **Add (missing entry → charge):** lockstring in `live`, absent from `have`, CR `CreationTimestamp` age
     `> grace` → add via `HSET` **without** the limit check (anti-drift reflects reality; it cannot reject an
     existing sandbox). Heals lost entries / rebuilds after Redis loss.
   - **Remove (leaked entry → free):** lockstring in `have`, **not** `isLiveForQuota` in `live` (gone or
     deletion-requested/terminating), entry age (`now − entry-ts`, Redis clock) `> grace` → `HDEL`. Frees
     failed-create leftovers (path 1's kept charges) and deletions the event handler missed.

There are **no sums** to reconcile, so there is **no `resyncSums`** — count is `HLEN` of authoritative
membership. Add/Remove are **per-lockstring, idempotent** (`HEXISTS` / `HDEL` semantics), so the whole pass is
safe to run concurrently with the hot path and with a flapping leader, with **no version guard**.

- **Cache-health gate (never APIReader).** All **Sandbox CR** reads are informer reads (subjects come from the
  key store). The gate is **not one-time**: **every remove-capable pass** — both the periodic diff and the
  leader's event-driven release (path 3) — first checks the Sandbox informer is **healthy**, defined as: it has
  completed ≥1 full list, `HasSynced` is true, and no watch error / outstanding relist since the last
  successful sync — and **skips the remove direction** otherwise. So a freshly-(re)elected leader, a mid-run
  relist, or a watch-bookmark gap can never make a partial/cold cache look "empty" and wrongly free a live slot.
  The **add** direction is always safe (it only ever charges existing CRs). Even if a spurious remove ever
  slipped through, it **self-heals** — the add direction re-charges the still-live CR within `grace`;
  correctness does not *rest* on the gate, the gate just shrinks the transient. A lagging-but-synced informer is
  also safe — a create not yet in cache defers the add; a delete still in cache defers the remove — both
  converge on the next pass. A **key-store enumeration error** likewise skips that anti-drift cycle (metric +
  log) and is **never** interpreted as an empty subject set. Skipped passes are counted as metrics.
- **Grace = 10 minutes** (§15), comfortably above the ~1-minute worst-case apiserver lag, so a just-created CR
  is never mistaken for "absent" and a just-released slot is never mistaken for "missing".
- **Cadence / cost.** The *remove* direction is driven primarily by the leader's informer **events** (path 3) at
  event speed; the periodic full **bidirectional diff** is an **infrequent** backstop (minutes). Every read is
  informer-served — there is **no apiserver `List`** anywhere — and the per-key work (`cache.List` by index +
  `HGETALL`) is bounded by that key's own live-set size (capped by its own count limit), not by the 500k cluster
  total.

### 6.5 New keys need no seed

A brand-new limited key provably owns **zero** sandboxes, so an absent `q:live:{K}` legitimately means count 0 —
`Acquire` simply starts charging from empty. There is **no** `NEED_SEED` round-trip and no per-key consistent
read on the cold path. The only case where an absent/short hash does **not** mean "truly zero" is a Redis data
loss for an already-active key; that is handled by fail-open + the leader rebuild (§9), the same bounded
self-healing envelope, not by a per-acquire seed.

### 6.6 Key-deletion cleanup

On API-key delete (§11): `DEL q:live:{K}` — a **single key** (count-only has no sum keys), so a trivially
one-slot command (Cluster-safe). This is the **sole** cleanup for a deleted key's entries: since the driver
enumerates subjects from the key store (§6.4.2), a deleted key is no longer reconciled, so its entries are never
revisited — therefore there is **no `SCAN` sweep**. To shrink the leak window the `DEL` is retried a bounded,
**non-blocking** number of times off the hot path; if it still fails the residue is harmless dead memory (key
IDs are fresh, never-reused UUIDs) that is never read again — monitor Redis memory for it. Failure is
**non-fatal**.

### 6.7 Quota lifecycle: immutable after create

A key's `QuotaSpec` is **immutable after creation — there is no quota `PATCH`** (a deliberate Phase 1
scope-down, §11). This eliminates by construction the only normal-operation oversell this design would otherwise
have, the **`unlimited → limited` transition race**: while unlimited the hot path bypasses Redis, so a later
re-limit would leave CRs created by a stale-`unlimited` replica uncharged while a fresh-`limited` replica admits
against a short `live`. By fixing the mode at birth:

- A key created **limited** is enforced from its first `Acquire`; it owns zero sandboxes at birth (§6.5), so
  there is no prior unlimited interval leaving uncharged CRs.
- A key created **unlimited** stays unlimited; the hot path always bypasses Redis for it.
- A replica that has not yet observed a newly-created key resolves it as **unknown** (auth failure), never as
  "unlimited" — so no replica ever holds a stale `unlimited` view of a limited key.

To change a key's quota, create a **new** key. Any future `PATCH` (§16) may only change the limits of a key that
was **born limited** — promoting an `unlimited` key to `limited` is **forbidden forever** (so the hot path's
"unlimited ⇒ bypass Redis" assumption can never be invalidated) — and MUST ship a safe-activation scheme.
Deferred.

## 7. Correctness

Admission grants iff `live + 1 <= limit`, computed and committed **atomically** in the Acquire Lua. **While
Redis is healthy**, at the instant of every admission `live` equals the exact charged set, so a grant cannot
exceed the limit — **strict enforcement at admit**.

The whole-system invariant is **convergence**: the Redis live set converges to the cluster's set of **live**
owner-K CRs (by lockstring), so `HLEN` equals the exact live count. This rests on:

1. **Per-lockstring idempotent, per-op atomic mutation.** Every Redis write is one Lua script (or one
   idempotent command) keyed by a stable lockstring and guarded by `HEXISTS`/`HDEL`. `HLEN` moves **only**
   through these guarded ops. Therefore concurrent acquires, retries, leadership handoff, and double-fired
   releases (delete + event) all converge — **no version guard, no stop-the-world.**
2. **Bidirectional anti-drift (§6.4.2).** Every divergence is corrected after `grace`: a live CR with no entry
   is added; an entry with no live CR is removed. This is what keeps lost entries (Redis restart / failover /
   rollback) from drifting permanently — the property a remove-only GC would lack.
3. **Conservative release (§6.4).** A charge is dropped only when provably pre-CR or after deletion is accepted,
   so a healthy-state release can never free a slot a live CR still holds.
4. **Deletion-requested = freed (§6.4.1).** A CR that is not `isLiveForQuota` does not count; anti-drift add
   only considers `isLiveForQuota` CRs, so it never fights a deletion.

### 7.1 Honest no-oversell statement

- **Redis healthy:** enforcement is **strict at admit** *and* **release is conservative**, so there is **no
  over-admission**. The only healthy-state transient is a bounded **under-sell** (over-rejection): an ambiguous
  failed create that charged but produced no CR holds a leaked entry until anti-drift removes it (≤ grace).
- **Physical-pods transient (not a quota over-admission):** "deletion-requested = freed" (§6.4.1) lets a
  stuck-terminating pod plus a replacement briefly exceed `limit` in *physical pods*, while the quota *charge*
  stays exactly correct. Bounded by concurrent terminations.
- **Redis unavailable (confirmed product decision = fail-open):** limited keys are **temporarily unenforced**
  (treated as unlimited). This is an explicit availability-over-enforcement choice; oversell during the outage
  is bounded by the outage's create volume and self-heals once Redis returns and anti-drift rebuilds.
- **Lost Redis entries while the key stays active:** transient over-admission until the leader rebuild/diff
  re-adds; under fail-open the gap is "unenforced", not "rejected".

In short: **strict at admit and no over-admission while Redis is healthy; the only over-admission is during a
Redis outage or a post-loss rebuild window — both bounded, self-healing, and a confirmed availability-favoring
product decision.** Phase 1 never permits *unbounded* oversell.

Implicit precondition: **every path that stamps `owner=K` onto a CR goes through `Acquire`** (so the charge
precedes the CR). Phase 1 stamps owner only on the E2B create paths (claim/clone), which all go through
`Acquire`.

## 8. Generic "primary manager" leadership

The hot path (`Acquire` / the path-1/2 release) runs on **all** replicas. Only the leader-side
removal/anti-drift (§6.4 paths 3–4) benefits from running once.

- `SandboxManager` gains a **generic** `IsPrimary() bool`, backed by a single `coordination.k8s.io/Lease`
  (`sandbox-manager-primary`) via the vendored `client-go/tools/leaderelection` — intentionally not coupled to
  quota, so any future singleton task can gate on it.
- The quota anti-drift **driver** (in `pkg/servers/e2b/quota`) — both its event-driven release (registered into
  the cache via `CacheSandboxCustomReconciler` + `AddReconcileHandlers`) and its periodic diff — runs only while
  `IsPrimary()`. The hot path and per-request release are **not** gated.
- **Leadership carries no correctness weight.** Correctness rests on per-lockstring idempotency (§7). If
  leadership flaps/splits, the worst case is anti-drift running on several replicas at once — idempotent, hence
  safe.
- RBAC: `get/list/watch/create/update` on `coordination.k8s.io/leases` in the manager namespace (one lease).

## 9. Degradation & Redis Data Loss (Phase 1: fail-open — confirmed product decision)

Phase 1 posture: **fail-open** on any Redis trouble; rely on the leader's bidirectional anti-drift to rebuild.
This is an explicit, confirmed availability-over-enforcement decision, not an unstated default — see §7.1.

- **No Redis configured** → `noopBackend`: all keys unenforced (unlimited); a non-empty quota is still accepted
  and persisted (§6.1) and surfaced by `Describe` as stored-limit-with-unknown-usage.
- **Redis transiently unreachable** (restart, network error, failover unreachable phase): limited-key `Acquire`
  **allows** (treated as unlimited for that request); unlimited keys unaffected. Bounded oversell, self-healing.
- **Redis data loss** (cold restart / flush / first boot): the hash is empty, so `Acquire` reads `HLEN = 0` and
  allows — which **is** the fail-open behaviour; no special detection needed. The leader's anti-drift **add**
  pass repopulates `q:live:*` by enumerating the limited keys from the **key store** (Redis-independent,
  §6.4.2) and reading each key's live CRs off the **informer** (`IndexUser`, never APIReader). Enforcement
  resumes per key as its entries are rebuilt. The oversell window is the rebuild duration; bounded and
  self-healing.
- **Partial rollback** (some entries lost, others survive): the **add** direction re-charges missing live CRs
  (after `grace`). No detection key is needed in the fail-open posture.
- **Redis config removed after keys exist** (operational): treated exactly as a Redis **outage** — limited keys
  fall to fail-open. No special cross-check is added.

> **Deferred (later PR): fail-closed posture.** A config knob `onRedisUnavailable: allow | reject` (default
> `allow`). `reject` reintroduces strictness across loss: a global `q:warm` total-loss detector, a
> Redis-server-clock `q:warmAt` settle window (so in-flight creates commit before the rebuild seed),
> `COLD → 503` retryable on the hot path, and optionally synchronous replication (`WAIT` / sync topology) for
> strictness across failover. Its global keys are also what reopen the Redis Cluster `CROSSSLOT` question
> (§6.2). **Not** built in Phase 1.

Operational recommendation to keep loss rare even under fail-open: Redis AOF `appendfsync everysec` + HA
(Sentinel-managed primary with a replica).

## 10. Create Hot Path (limited key)

1. `CheckApiKey` already put `user` (with `QuotaSpec`) in context.
2. **Unlimited key** → `Acquire` returns a sentinel reservation; no Redis, no leadership lookup; zero cost.
3. **Limited key** → one Lua `Acquire(lockstring, countLimit)` (no resource/footprint resolution — count-only):
   - `OK` → proceed.
   - `REJECTED` → **HTTP 429 immediately, no retry**; if a pooled sandbox was tentatively picked, return it to
     the pool (do not lock it). `TryClaimSandbox` must surface the quota miss as terminal, not loop.
   - transport error → **fail-open** (allow).
4. The `lockstring` is the one `LockSandbox` already stamps on the CR — no extra CR write, no infra interface
   change.
5. Run `ClaimSandbox` / `CloneSandbox`. On failure → **conservative release** (§6.4 path 1): release with
   `context.WithoutCancel` **only** if the failure is provably pre-CR or failed-sandbox cleanup has successfully
   requested deletion of the attempt's CR; otherwise keep the charge for anti-drift. On success → nothing; the
   entry already reflects the live sandbox.

`DELETE /sandboxes/{id}` releases the lockstring (§6.4 path 2) **after** the apiserver accepts the deletion (CR
→ deletion-requested), for low-latency-but-safe slot return; the leader's event handler (path 3) is the backstop
for all non-manager deletions.

## 11. API Surface

- **Create** (`POST /sandboxes`): unchanged shape; quota enforced internally; exceeded → **429** with the
  existing E2B error body (the spec-compliant `{code,message}` shape used by other E2B error paths), no retry.
- **Key create** (`POST /api-keys`): optional nested `quota`; **admin-only** to set; validated (§3.1). The
  internal `{"limits":[...]}` shape is not a documented public API shape and must not cause a nested public
  request to be silently ignored.
  **Accepted regardless of Redis presence** (unenforced if no Redis, §6.1).
- **No quota `PATCH`** (§6.7): immutable after create; change quota by creating a new key. (Deliberate Phase 1
  scope-down — a product decision.)
- **Describe** (optional): count `live` / `limit` for a key, via `QuotaManager.Describe`. When Redis is
  unreachable, absent, or a key is mid-rebuild, `Describe` does **not** fabricate `live = 0`; it returns the
  stored limit with usage marked **stale/unknown** (or, if the caller requires it, an error) — consistent with
  the fail-open posture that *enforcement* (not reporting) degrades gracefully.
- **Key delete** (`DELETE /api-keys/{id}`): drop the quota config; **keep** existing sandboxes; single one-slot
  `DEL q:live:{K}` (§6.6), bounded non-blocking retry, non-fatal — the sole cleanup (no `SCAN`).

Authorization reuses `CheckCreateAPIKeyPermission`; quota set chains `CheckApiKey` + admin check.

## 12. Compatibility

- Old keys without `quota` → unlimited; the new JSON field is `omitempty`.
- `CountActiveSandboxes` untouched; SandboxClaim self-healing preserved.
- No owner label and **no backfill**: anti-drift reads live CRs off the existing `IndexUser` informer index
  (§5).
- New RBAC: one generic lease (§8).
- New dependency: a Redis client (dormant unless configured). Phase 1 has **no global Redis keys**, so
  standalone / Sentinel / Cluster are all structurally compatible (§6.2).
- No change to E2B lifecycle semantics beyond create-time admission and the removal paths of §6.4.

## 13. Risks

- **Redis memory for the live set.** Storing every live sandbox (lockstring + a small ts → on the order of tens
  of MB at 500k) — much smaller than rev3's footprint-carrying entries. Monitor memory and per-key `HLEN`.
- **Steady-state anti-drift still reads a (warm) view.** A periodic bidirectional diff must eventually compare
  Redis to truth to catch missed events; it is **infrequent** and **entirely informer-served** (no apiserver
  `List` anywhere — subjects from the key store, live CRs from `IndexUser`). Monitor diff lag and divergence
  counts.
- **Under-sell from a kept ambiguous-failure charge.** Conservative release (§6.4 path 1) can hold a leaked
  entry for up to grace, transiently over-rejecting a high-churn key. The deliberate trade for never
  over-admitting while healthy; bounded and self-healing.
- **Deletion-requested = freed → transient excess physical pods** (§6.4.1). A stuck-terminating sandbox plus a
  replacement can briefly exceed `limit` in physical pods (the quota charge stays correct). Bounded by
  concurrent terminations; not resurrected by anti-drift (live-CR-only add).
- **Fail-open during Redis outage = temporary no enforcement.** Bounded oversell for the outage duration,
  self-healed by rebuild. The deferred fail-closed knob trades availability for strictness (§9/§16). Confirmed
  product decision.
- **Clone must stamp a lockstring.** Quota keys every entry off the lockstring; the clone path historically
  stamped only `AnnotationOwner` (`clone.go:303`). rev4 fixes clone to stamp one (§1/§5); any future
  owner-stamping path must do the same, or its sandboxes are untracked. No owner label / backfill is needed.
- **Deleted-key `DEL` failure.** If the key-delete `DEL` (§6.6) and its bounded retry both fail, the key's
  entries leak as never-read dead memory (no `SCAN` reclaims them). Correctness-harmless (deleted keys are never
  re-enumerated and IDs never reused), but not availability-harmless: unbounded *growth* could pressure Redis
  memory. Mitigation: alert on Redis memory and on `DEL`-failure count; an optional tombstone-retry queue (no
  `SCAN`) is future hardening.
- **Lockstring uniqueness/stamping.** Correctness keys off the lockstring being globally unique and present on
  every owner-stamped CR (it is, via `LockSandbox`). Any future create path stamping `owner` must also go
  through `Acquire` with that lockstring (§7 precondition).

## 14. Acceptance Criteria

- Concurrent creates for one limited key across replicas never exceed `count` **while Redis is healthy**.
- Idempotent acquire: a retried `Acquire` with the same lockstring does not double-charge.
- Conservative release: an ambiguous create failure **keeps** the charge (verified by a leaked entry that
  anti-drift later removes); a provably-pre-CR failure releases immediately; `DELETE` releases only after the
  deletion is accepted; all idempotent with the leader's event-driven release (no double-decrement below zero,
  no over-admission while Redis healthy).
- Deletion-requested = freed: a `DeletionTimestamp`-set / Terminating owner CR no longer counts; anti-drift does
  **not** re-add it.
- Bidirectional anti-drift: subjects enumerated from the key store; (a) an entry whose CR is gone is removed
  after grace; (b) **a live CR missing its entry is re-added after grace**, so a simulated entry loss converges
  to exact `HLEN` membership (note: usage may legitimately remain `> limit` after a fail-open over-admission —
  anti-drift converges to *truth*, and further creates are rejected until usage falls below limit; quota never
  drains existing sandboxes). Every remove-capable pass is gated on informer health (a cold/unsynced/relisting
  cache never removes a live slot); a key-store enumeration error skips the cycle without treating the subject
  set as empty. Both directions run only on the leader and are safe under a flapping leader (idempotent, no
  version guard).
- New key needs no seed: first `Acquire` on an absent `q:live:{K}` charges from zero (no `NEED_SEED`, no
  per-acquire consistent read).
- Fail-open: with Redis unreachable **or absent**, limited keys are allowed (unenforced) and unlimited keys
  provably do zero Redis IO; after Redis returns, the leader rebuild restores exact `live` and enforcement
  resumes.
- Quota settable without Redis: a non-empty quota at key create is accepted and persisted even when no Redis is
  configured; it is unenforced, and `Describe` reports the stored limit with usage stale/unknown (never a
  fabricated `live = 0`).
- No quota `PATCH`; a newly-created limited key enforces from its first `Acquire`; no replica observes a limited
  key as "unlimited" (unknown key → auth failure).
- 429 with no retry: a quota miss returns immediately with the E2B-compatible error body and a tentatively-picked
  pooled sandbox is returned to the pool.
- Validation (§3.1): `sandbox.count` `limit = 0` blocks all creates (the Lua treats `lim == 0` as hard-zero); an
  all-nil-limits quota is normalized to unlimited; negative / duplicate / non-`sandbox.count`-dimension (cpu,
  memory, `limits.*`) / non-empty-scope are rejected at key create; Redis presence imposes **no** validation
  constraint.
- `CountActiveSandboxes` and SandboxClaim self-healing unchanged (regression).
- `IsPrimary()` gates only the anti-drift sweep + event-driven release; correctness holds with the sweep forced
  on all replicas (idempotency regression test).
- Lockstring stamped on **every** create path (claim/create **and clone**); the anti-drift driver lists a key's
  live set via `IndexUser` (informer, never APIReader) and returns the correct set; no owner label, no backfill.
- Redis topology: Phase 1 Lua touches only the one `{K}`-tagged key (no `CROSSSLOT`); standalone/Sentinel
  verified, Cluster structurally compatible.
- Table-driven unit tests for `QuotaManager` (acquire/release/describe, idempotency, fail-open, quota-without-
  Redis) and the anti-drift driver (both directions, grace, cache-health gate, key-store enumeration,
  leader-gating), plus create-path integration.

## 15. Resolved Decisions & Implementation Discretion

### Resolved (product / architecture)

- **Scope:** Phase 1 enforces **`sandbox.count` only**. cpu/memory dimensions, per-template/team scope, and any
  per-entry footprint are reserved model extension points, **rejected at validation**, and deferred to §16.
- **Model:** Redis holds the **live set** (one entry per live sandbox, keyed by lockstring, value = acquire-ts
  only), maintained incrementally; `count = HLEN`; cluster is a backstop. **No committed-counter recompute, no
  version guard** (replaced by per-lockstring idempotency); **no sum keys, no `resyncSums`** (count-only).
- **Backend:** **Redis is the sole dynamic quota backend.** Static `QuotaSpec` is stored in Secret/MySQL; the
  dynamic live-set lives only in Redis. There is no MySQL-native counter and no pluggable alternative store. A
  quota may be **set even when Redis is absent** — it is persisted and simply **unenforced** (fail-open), exactly
  like a Redis outage.
- **Identity:** the existing **lockstring** (`AnnotationLock`, UUID from `LockSandbox`), stamped on **every**
  owner-stamping path — claim/create already do; **clone** is fixed to stamp one too (§1/§5). No new annotation.
- **Removal:** **conservative, no over-admission while healthy.** Release a create-failure charge **only** when
  provably pre-CR (ambiguous failure keeps the charge for anti-drift); manager `DELETE` releases **only after
  the deletion is accepted** (CR → deletion-requested), never pre-emptively; leader event-driven release for
  non-manager deletions; leader bidirectional anti-drift backstop. **Deletion-requested = freed** (do not wait
  for CR invisibility). The single **quota-live predicate** `isLiveForQuota` frees a slot iff
  `DeletionTimestamp != nil` or phase `Terminating`; Failed/Succeeded-but-not-deleted **still occupy** —
  deliberately **narrower** than `CountActiveSandboxes`'s Dead-exclusion, so quota uses its own owner read (§5).
- **Anti-drift is bidirectional** (add missing live-CR entries; remove leaked entries) — required so lost
  entries self-heal rather than drift forever. Add considers `isLiveForQuota` CRs only; the remove age-gate uses
  the entry ts (Redis clock). **Grace = 10 minutes.** The **driver lives in the quota layer** and enumerates
  limited keys from the **key store** (Redis-independent → survives total loss); the **live-CR read primitive**
  is in `pkg/cache` over `IndexUser`. All **Sandbox CR** reads are **informer-only, never APIReader**; **every
  remove-capable pass is gated on informer health**. **No owner label, no backfill.**
- **No seed:** absent `q:live:{K}` for a new key == zero; `NEED_SEED` deleted.
- **Redis unavailable / absent (Phase 1): fail-open** — a **confirmed product decision** (availability over
  enforcement). The "strong consistency" guarantee is scoped to **"strict at admit while Redis is healthy"**.
  Fail-closed posture + its `q:warm`/settle/`WAIT` machinery is **deferred** (config knob `onRedisUnavailable`,
  default `allow`).
- **Quota immutable** after key create (no `PATCH`) — a deliberate Phase 1 scope-down; admin-only to set;
  subject is always the API key. **`unlimited → limited` is forbidden forever** — even a future `PATCH` may only
  change a *limited* key's limits — so the hot path's "unlimited ⇒ bypass Redis" assumption can never be
  invalidated (§6.7).
- **Reconciler placement:** leader-gated **driver in `pkg/servers/e2b/quota`** (event handler registered into
  `pkg/cache`); subjects enumerated from the key store; generic `IsPrimary()` lease; correctness via
  idempotency, not leadership.
- **All Redis writes are single atomic Lua (or one idempotent command) and per-lockstring idempotent**, guarded
  by `HEXISTS`/`HDEL`.
- **Error codes:** quota exceeded **429** (no retry), E2B-compatible error body.
- **Topology:** Phase 1 has no global / no sum Redis keys → standalone/Sentinel/Cluster structurally compatible.

### Left to the implementing agent

- Redis key prefix, hash-tag form (`{K}`), and Lua script encoding.
- Redis client choice and config wiring (`pkg/sandbox-manager/config` / `clients`, `cmd/sandbox-manager`):
  pooling, retry/back-off, acquire timeout, and the fail-open error classification.
- The generic leadership lease name and `leaderelection` parameters; `IsPrimary()` exposure on
  `SandboxManager`.
- Anti-drift cadence values, the key-store enumeration of limited keys, and how missed-event divergence is
  monitored.
- External nested JSON shape for `quota`.
- The exact rule for classifying a create failure as **provably pre-CR**, **cleanup-deleted**, or **ambiguous**
  (which infra errors guarantee no CR was written, and which failed-sandbox cleanup branches prove deletion
  acceptance), and where exactly clone stamps its lockstring (the `Modifier` at `create.go:200` vs
  `newSandboxFromTemplate`), reusing `NewLockString()`.
- Where exactly `Acquire`/`Release` hook into `TryClaimSandbox` / clone / `DELETE`, and how a tentatively-picked
  pooled sandbox is returned to the pool on a 429.
- The `ListLiveLockstringsByOwner` signature in `pkg/cache` (over `IndexUser`).
- A concrete informer-health API from `pkg/cache` (e.g. `SandboxInformerHealthy()`) backing the remove-gate
  (§6.4.2) — tracking initial-list-complete, relist start/success, watch error, and last successful sync — so it
  is **not** implemented as plain `HasSynced`.

## 16. Deferred / Future Work

- **cpu / memory dimensions** (and **per-template scope**): reintroduce a per-entry resource **footprint** in
  the live-set value, per-dimension `q:sum:*` keys, an atomic `resyncSums` in anti-drift, and either a
  `TemplateRef`-immutable-for-quota rule or a footprint snapshot on the CR (so the anti-drift add recomputes the
  identical charge). The model already reserves these dimensions; they are rejected at validation until shipped.
- **Fail-closed posture** (`onRedisUnavailable: reject`): `q:warm` total-loss detector, Redis-clock `q:warmAt`
  settle window, `COLD → 503`, and synchronous replication (`WAIT` / sync topology) for strictness across
  failover. Reintroduces global Redis keys (reopens the Cluster `CROSSSLOT` question).
- **Quota `PATCH`** for *already-limited* keys (limit changes only — never `unlimited → limited`, §6.7) with a
  safe-activation scheme (activation window or all-replica `QuotaSpec` cache invalidation).
- **Post-create 变配** (if the manager ever exposes it): an informer update-event handler that adjusts the entry
  footprint (once footprints exist) by the delta, plus optional admission gating of the resize against quota.
- **Apiserver-side fencing** (a validating webhook rejecting a Sandbox create whose reservation is
  unknown/expired) for absolute 0-oversell under failure intersections — out of scope by product decision.
- **Official Redis Cluster support** (testing + the deferred-posture hash-tag redesign).
