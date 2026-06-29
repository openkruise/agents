# API Key Sandbox Quota — Design Spec

- Date: 2026-06-22
- Scope: `pkg/servers/e2b/` (create, api_key, models, keys, routes; static API-key quota config only),
  `pkg/sandbox-manager/quota/` (new: quota types, `QuotaManager`, the Redis backend with its upsert/release Lua,
  the circuit breaker, and the primary-aware anti-drift driver), `pkg/sandbox-manager/` (manager-level
  create/delete options, quota admission construction, quota cleanup, and generic primary leadership signals),
  `pkg/sandbox-manager/config` + `clients` (Redis config/client + circuit-breaker tunables),
  `pkg/cache/` (an additive owner-indexed live-CR read primitive over the existing `IndexUser` field index,
  a `SandboxInformerHealthy()` health signal, plus event-handler registration for event-driven reconcile),
  `cmd/sandbox-manager/`, `config/`/Helm chart (RBAC for `coordination.k8s.io/leases`, Redis config),
  `go.mod`/`vendor` (add a Redis client).

## 1. Background

sandbox-manager is a stateless, multi-replica backend exposing E2B/MCP APIs to manage sandboxes. In the E2B API
server, each request authenticates via `X-API-KEY`, resolved to a `*models.CreatedTeamAPIKey` (with `ID
uuid.UUID` and `Team`). E2B treats that `ID` as the quota **subject/user** and passes only the resolved user ID
and static `QuotaSpec` into sandbox-manager. sandbox-manager never depends on API-key storage or E2B key models.
That user ID is the sandbox **owner**: every create path stamps it onto the Sandbox CR as
`agentsv1alpha1.AnnotationOwner` (claim and create-on-no-stock via `utils.LockSandbox`, clone via infra clone
creation), and it surfaces as `route.Owner`.

We need to cap **how much a single API key may hold** across several resource **dimensions** (count, cpu,
memory), each optionally narrowed to a **scope** of the key's sandboxes (Phase 1: the `all` scope and the
`running` scope), enforced across replicas without materially slowing down create. Peak create throughput is
~2500/sec (150k/min) aggregate; a single cluster may hold ~**500k** sandboxes; the apiserver may lag by up to
~**1 minute**. The worst case for a single limited key is "small limit + high churn" (constantly creating and
deleting against a tiny limit), which rules out any per-operation external IO on the **unlimited** hot path and
demands a cheap, contention-free check on the **limited** hot path, plus timely slot reclamation.

### Why a Redis live set + per-dimension sums

Redis holds the **live set itself** — one entry per live sandbox, keyed by its **lockstring** — plus a small set
of **incrementally-maintained per-`(dimension, scope)` sums**, so:

- Every dynamic value is `q:sum:{K}:<dim>[<scope>]` — read in O(1), never recomputed by scanning. The entry
  carries the per-sandbox footprint and scope membership; the sums are the rollups.
- Every mutation is a **single, atomic, idempotent upsert** keyed by a stable per-sandbox identity (the
  lockstring): it reads the entry's old `(footprint, scopes)`, diffs against the new value, and applies the
  per-`(dim, scope)` delta to the sums. **Idempotency is intrinsic to the diff** (re-applying the same value
  yields zero deltas) — it replaces a version guard. Concurrent / retried / leadership-handoff / double-fired
  writes all converge.
- The entry and its sum contributions are **always written together in one Lua script** (the co-write
  invariant, §7), so they can never diverge; a full-cluster rebuild from whatever survived a Redis loss is
  therefore self-consistent without a separate sum recompute.
- A full cluster `List` is needed only by a **rare** backstop rebuild, not on every reconcile tick. Steady state
  is incremental (hot path + leader events) plus an infrequent leader-side diff against the warm informer.

### Key facts established from the codebase

- `pkg/utils/utils.go:201` `LockSandbox(sbx, lock, owner)` stamps `AnnotationLock = lock` (a UUID from
  `NewLockString()` / `uuid.NewString()`, `utils.go:211`) **and** `AnnotationOwner = owner` on the **same** CR
  write, used by claim/create (`performLockSandbox`, `claim.go:661`). **The lockstring is an existing
  per-sandbox UUID persisted on the CR** — quota reuses it as the live-entry key; no new annotation is
  introduced. The **clone** path now also stamps a lockstring via its create modifier, so **every**
  owner-stamped CR carries a lockstring (§5).
- The Sandbox CR carries the resolved pod resource requests/limits, from which the cpu/memory **footprint** is
  computed. `Status.Phase` (`sandbox_types.go:238`) ranges over
  `Pending/Running/Paused/Resuming/Upgrading/Succeeded/Failed/Terminating`; `Spec.Paused` (`sandbox_types.go:71`)
  is the pause request. These drive scope membership (§5).
- `pkg/cache` is the sandbox-manager-only, informer-backed cache. It already runs informers, exposes a raw
  Sandbox informer event-handler registration (`AddSandboxEventHandler`, returning a
  `SandboxEventHandlerRegistration` whose sync also feeds `SandboxInformerHealthy()`) for external event handlers,
  and maintains an
  `IndexUser` field index over `AnnotationOwner` (`index.go:83`, for both Sandbox and Checkpoint). This design
  adds an owner-indexed live-CR read primitive and a `SandboxInformerHealthy()` signal here, and lets the quota
  layer register an event handler; the anti-drift **driver** itself lives in `pkg/sandbox-manager/quota`
  (§6.4.2).
  Anti-drift never uses `GetAPIReader` — all its **Sandbox CR** reads are informer reads, gated on cache health
  (subjects come from a quota subject lister supplied by the E2B layer, not from the apiserver; §6.4.2).
- The lifecycle predicate behind `IsLiveForQuota` is single-sourced in `pkg/sandbox/lifecycle`, importing only
  the API types. `pkg/cache` imports that leaf package directly and `pkg/sandbox-manager/quota` wraps it as
  `IsLiveForQuota`, avoiding a cache ↔ quota import cycle and lockstep duplicate predicates.
- `CountActiveSandboxes` **excludes** `Dead` (`cache.go:306`) and is relied on by the SandboxClaim controller.
  **It must not be modified.** Quota uses its own additive owner read with its own live/scope predicates (§5).
- `k8s.io/client-go/tools/leaderelection` (Lease-backed) is already vendored — reused for a single generic
  `IsPrimary()` capability (§8).
- `github.com/redis/go-redis/v9` is used behind the quota backend interface.
- The create hot path is `createSandboxWithClaim` → `ClaimSandbox` and `createSandboxWithClone` →
  `CloneSandbox` (`create.go`); the `Modifier` closures (`create.go:119` / `:200`) are the existing stamping
  hook where `Acquire`/`Release` wrap the call.

## 2. Goals & Non-Goals

### Goals

- Enforce per-API-key limits across the **count**, **cpu**, and **memory** dimensions, each over the `all` or
  `running` scope, **strict at admit while Redis is healthy**, across replicas.
- Make every dynamic value **exact and lag-free in steady state** by storing the literal live set (lockstrings,
  footprint, scope membership) plus incrementally-maintained per-`(dimension, scope)` sums in Redis — no
  periodic recompute, no frequent full `List`.
- Keep enforcement correct under concurrency/retry/leadership-handoff using a **single per-lockstring
  idempotent, per-op atomic** upsert (Lua) — no version guard.
- Unlimited keys perform **zero** Redis IO on the hot path. Limited keys perform **at most one** Redis
  round-trip per acquire.
- The cluster remains the **ground truth**; the Redis state is reconstructible from it and self-heals via a
  **bidirectional** anti-drift diff (charge entries for live CRs missing from Redis; release entries whose CR is
  gone; correct scope-membership/footprint drift in either direction).
- **No over-admission while Redis is healthy:** the create admission is strict, and release is conservative
  (keep the charge on an ambiguous create failure; free a slot only once deletion is accepted), so a
  healthy-state transient can only be a bounded *under-sell*, never an over-sell — **except** for the
  deliberately-accepted, bounded over-limit the `running` scope can exhibit via un-gated `resume` (§6.4.1, §7).
- Backward compatible: keys without a quota field default to **unlimited**.
- The quota **data model is `(user, dimension, scope)` and extensible** (new dimensions, new scopes) by
  construction — adding a dimension or scope later needs no schema change and no data migration (the sums are
  maintained generically).

### Non-Goals (Phase 1)

- **Per-template scope, per-team scope.** The quota subject is always the API key. The storage layer treats a
  scope as an opaque string (so `template:<name>` already works structurally), but Phase 1's manager **emits and
  enforces only `all` and `running`**, and **rejects any other scope at key-create validation** (§3.1), so a
  not-yet-supported scope is never silently accepted.
- **Post-create resize / 变配 admission.** sandbox-manager exposes no post-create resource-change API, so there is
  no admission gate for resizes. A footprint that *does* change out-of-band (e.g. controller-driven in-place
  update) is **reconciled into the sums by the leader** (the upsert handles it, §6.4.2) but is never
  admission-gated — symmetric with how `resume` is handled.
- **Strict no-oversell across Redis unavailability.** Phase 1 is **fail-open** on Redis trouble (§9) — a
  **confirmed product decision** trading enforcement for availability: during a Redis outage limited keys are
  temporarily unenforced (treated as unlimited), accepting a bounded oversell that the leader's anti-drift
  rebuild self-heals once Redis returns. A circuit breaker (§9) keeps fail-open cheap during a sustained outage.
  The **fail-closed** posture is deferred (§16).
- Reclaiming/evicting/draining existing sandboxes; quota only blocks new creates.
- **Quota mutability is out of scope for Phase 1** (settable only at key create). It is **planned future work**,
  not a permanent prohibition (§6.7, §16): a later phase will support changing a *limited* key's limits and
  promoting an `unlimited` key to `limited`, each with a safe-activation scheme.
- Billing/usage reporting; cross-cluster/multi-region quota; a per-op consensus log or apiserver-side fencing
  webhook.

## 3. Quota Data Model (static)

A quota is addressed by `(user, dimension, scope)`. Phase 1 enforces three dimensions over two scopes; the
model reserves further dimensions/scopes for forward compatibility but does **not** enforce them.

```go
type QuotaDimension string
const (
    DimSandboxCount QuotaDimension = "sandbox.count" // unit: sandboxes — footprint is the implicit constant 1
    DimLimitsCPU    QuotaDimension = "limits.cpu"    // unit: millicores (integer)
    DimLimitsMemory QuotaDimension = "limits.memory" // unit: MiB (integer)
    // further dimensions are reserved; rejected at validation until shipped (§3.1)
)

// Scope narrows which of the user's sandboxes a limit counts. Subject is the resolved sandbox-manager user.
// Stored/serialized as an opaque string so new scopes need no schema change. Phase 1 accepts only:
type QuotaScope string
const (
    ScopeAll     QuotaScope = "all"     // every sandbox that is live for quota (IsLiveForQuota, §5)
    ScopeRunning QuotaScope = "running" // live for quota AND not paused (the create→Running flow, orthogonal to Paused)
    // e.g. "template:<name>" is structurally supported but rejected at validation in Phase 1
)

type QuotaLimit struct {
    Dimension QuotaDimension `json:"dimension"`
    Scope     QuotaScope     `json:"scope"`
    Limit     int64          `json:"limit"` // >= 0; 0 == valid hard-zero. Presence == limited (absence == unlimited)
}

type QuotaSpec struct {
    Limits []QuotaLimit `json:"limits,omitempty"` // empty/absent == fully unlimited
}
```

- **Presence semantics:** a `(dimension, scope)` pair is **limited** iff it appears in `Limits`. An absent pair
  is **unlimited**. There is no nil-limit sentinel — normalization (§3.1) drops any explicitly-unlimited pair, so
  a "limited key" is defined uniformly as *having ≥1 `QuotaLimit`* (the same definition the hot path and the
  anti-drift driver use, §6.4.2).
- **External JSON uses the same `QuotaSpec` shape as the internal model.** API-key create/list responses serialize
  quota as `{"limits":[{"dimension":"sandbox.count","scope":"running","limit":10}]}` (cpu in millicores, memory in
  MiB). There is no separate public quota model and no short-key mapping (`count` / `cpu` / `memory` are rejected),
  so the public payload, static storage model, and Redis dimensions stay identical.
- `QuotaSpec` is loaded at auth time (`CheckApiKey` puts `user` in context), so the hot path never re-reads the
  key store.

### 3.1 Validation (only at key create — Phase 1 has no quota `PATCH`)

A quota is **never silently ignored or silently accepted**:

Validation and normalization belong with the quota model in `pkg/sandbox-manager/quota/spec`: legal dimensions,
legal scopes, duplicate detection, limit bounds, JSON decoding, and `NormalizeQuotaSpec`. The E2B layer uses
`quotaspec.QuotaSpec` directly in public API request/response structs and only calls quota normalization. This
keeps future API-key `PATCH` or non-E2B callers on the same quota-domain validation path.

- **Absent / `null` quota** (or empty `Limits`) → **unlimited**.
- **Explicitly-unlimited pairs** → **dropped at normalization**, so a "limited key" is uniformly *≥1 limit*.
- **`Limit == 0`** for a `(dim, scope)` → **valid** hard-zero (every create charging that pair returns 403).
- **`Limit < 0`** → **rejected**.
- **Duplicate `(dimension, scope)`** → **rejected**.
- **Any dimension other than `sandbox.count` / `limits.cpu` / `limits.memory`** → **rejected** (reserved, not
  yet enforceable), so a future dimension is never silently dropped or silently honored.
- **Short dimension keys** (`count`, `cpu`, `memory`) → **rejected**; public JSON uses the full internal dimension
  strings in `QuotaLimit.dimension`.
- **Any scope other than `all` / `running`** (e.g. `template:<name>`) → **rejected in Phase 1**.
- **No constraint on Redis presence.** A non-empty quota is **accepted regardless of whether Redis is
  configured**; if Redis is absent (or later unavailable) the limit is simply **unenforced** (fail-open, §6.1).
  The quota is persisted either way and returned as static quota on API-key create/list responses.

## 4. Static Config Storage

Both E2B key backends store **only** the static `quotaspec.QuotaSpec`, alongside the key, written once at **key
create** (immutable thereafter for Phase 1, §6.7). **No dynamic usage is ever written to the key store** — that
lives in Redis and is owned by sandbox-manager quota.

- **Secret backend:** store the normalized `QuotaSpec` in the per-key JSON inside `e2b-key-store`; public API
  request/response structs use the same `QuotaSpec` shape. Old payloads without quota decode to empty == unlimited;
  writes reuse the existing `retryUpdateSecret` CAS. (The single `e2b-key-store` Secret is suited only to
  **static** config — it cannot host per-create dynamic state at 2500/sec; that is exactly why dynamic state lives
  in Redis, not the Secret.)
- **MySQL backend:** add a nullable `quota JSON` column to `team_api_keys` (`NULL` == unlimited); `AutoMigrate`
  adds it, gated by `DisableAutoMigrate` with a documented manual DDL alternative.

## 5. Identity, Owner Index, Footprint, and Scope Predicates

- **Sandbox identity in Redis = the lockstring** (`AnnotationLock`, a UUID stamped by `LockSandbox`). It is
  globally unique and persisted on the CR, so it survives `GenerateName` (the create path need not know the
  final object name). Re-keying every Redis op off it gives idempotency for free. **Every owner-stamped create
  path must stamp a lockstring:** claim/create already do (`LockSandbox`); this design adds it to **clone** (via
  the `Modifier`, `create.go:200`, using `NewLockString()`), closing the one path (`clone.go:303`) that
  previously set only `AnnotationOwner`. This is a hard precondition (§7).
- **Owner read uses the existing `IndexUser` field index — no owner label, no backfill.** `pkg/cache` already
  indexes `AnnotationOwner` (`index.go:83`), so the anti-drift driver lists a subject's CRs with
  `cache.List(MatchingFields{user: K})` directly off the informer. Because all anti-drift reads are informer
  reads (never APIReader, §6.4.2), no server-side label selector is required, so this design adds **no** owner
  label and needs **no** one-time backfill. The index covers every CR carrying `AnnotationOwner`, including
  clones once they are owner-stamped.
- **Quota-live and scope predicates — the single source of truth for membership, used identically by the count
  read and by both anti-drift directions (§6.4.2):**

  ```go
  // IsLiveForQuota reports whether a Sandbox still occupies its owner's quota at all (membership in `all`).
  // Freed (NOT live) iff deletion has been requested or is in progress; a merely Failed/Succeeded-but-not-yet-
  // deleted sandbox still occupies quota until it is. Reusing/reuse-triggered sandboxes are not quota-live.
  func IsLiveForQuota(sbx *agentsv1alpha1.Sandbox) bool {
      return lifecycle.IsLiveForQuota(sbx)
  }

  // InRunningScope reports membership in the `running` scope: live for quota AND not paused. Membership in
  // `running` flips off when Spec.Paused is requested and back on when it is not.
  func InRunningScope(sbx *agentsv1alpha1.Sandbox) bool {
      return lifecycle.IsLiveForQuota(sbx) && !sbx.Spec.Paused
  }

  // ConditionalScopesOf returns the *conditional* scopes a sandbox belongs to (NOT `all`, which is implicit
  // and always holds while IsLiveForQuota). Phase 1: ["running"] or []. Forward-compatible with more scopes.
  func ConditionalScopesOf(sbx *agentsv1alpha1.Sandbox) []QuotaScope

  // FootprintOf returns the per-dimension resource amounts charged for this sandbox, in integer units (cpu
  // millicores, memory MiB). `sandbox.count` is the implicit constant 1 and is NOT included here. Resolved from
  // the sandbox's effective pod resource requests/limits, deterministically — the hot path resolves it from the
  // create spec (the CR may not exist yet), anti-drift recomputes the identical value from the CR (§6.4.2).
  func FootprintOf(sbx *agentsv1alpha1.Sandbox) map[QuotaDimension]int64
  ```

  The neutral lifecycle predicate lives outside both cache and quota, so cache imports it directly and quota wraps
  it as `IsLiveForQuota`. `FootprintOf` is implemented by first resolving a Sandbox CR into the same
  `infra.SandboxResource` shape used by the hot-path Admission, using the shared infra resource extraction helper
  (today `infra.CalculateResourceFromContainers`, already used by `sandboxcr.Sandbox.GetResource`), then passing
  that resource through one shared `quota.FootprintFromResource` dimension mapper. There must not be separate
  hot-path and anti-drift resource extraction or dimension maps.

  `IsLiveForQuota` is **deliberately narrower** than `CountActiveSandboxes`'s "not `Dead`" filter (which *also*
  excludes Failed/Succeeded): quota frees a slot only when deletion is requested/terminating or the sandbox enters
  reuse, **not** the moment a pod fails. That difference is why quota uses its own additive read rather than
  `CountActiveSandboxes` (left untouched, §1). `InRunningScope` uses `Spec.Paused` (the pause request) rather
  than waiting for `Status.Phase == Paused`; we do **not** wait for the CR to become invisible.

  ```go
  // ListLiveSandboxesByOwner returns every Sandbox CR with IsLiveForQuota == true for owner K, read from the
  // warm informer via MatchingFields{user: K} (IndexUser). Never APIReader; the caller invokes it only after the
  // cache is healthy (§6.4.2), so an unsynced cache can never look "empty". Cache applies the shared
  // lifecycle.IsLiveForQuota predicate; the quota driver derives lockstring, ConditionalScopesOf, and FootprintOf
  // from each returned CR.
  func (c *Cache) ListLiveSandboxesByOwner(ctx, K) ([]*agentsv1alpha1.Sandbox, error)
  ```

### 5.1 Layering and request options

Quota is owned by sandbox-manager, while API-key storage remains owned by the E2B layer:

- `pkg/sandbox-manager/quota/spec` defines the static quota model (`QuotaSpec`, `QuotaLimit`, dimensions,
  scopes) and subject-lister interfaces. `pkg/sandbox-manager/quota` owns the dynamic Redis machinery.
  `pkg/servers/e2b/models` references `quotaspec.*` directly for API-key quota fields.
- E2B creates/list/deletes API keys, decodes the public quota JSON into `quotaspec.QuotaSpec`, and stores static
  quota config. After auth it passes only the resolved `User string` and `Quota *quotaspec.QuotaSpec` to
  sandbox-manager.
- sandbox-manager exposes manager-level `ClaimSandboxOptions` / `CloneSandboxOptions` / `DeleteSandboxOptions`.
  To avoid copying every infra option field, these are thin wrapper types around the existing infra options plus
  quota metadata, for example `ClaimSandboxOptions{Infra infra.ClaimSandboxOptions, Quota *quotaspec.QuotaSpec}`.
  sandbox-manager owns and overwrites internal-only fields such as `Infra.Admission`; callers keep using the
  existing `Modifier func(infra.Sandbox)` for E2B-specific sandbox decoration.
- sandbox-manager creates the `infra.SandboxAdmission` from `(User, Quota)` and its quota manager. E2B never
  constructs Admission and never calls dynamic Redis quota APIs.
- infra remains generic: it only receives `SandboxAdmission`, lockstrings, and `SandboxResource`; it does not
  know quota dimensions, API keys, or Redis.

## 6. Counting Model (Redis live-set + per-(dimension, scope) sums)

All enforcement lives in a `QuotaManager` (package `pkg/sandbox-manager/quota`) behind a `QuotaBackend` interface,
with a `redisBackend` (wrapped by a circuit breaker, §9) and a `noopBackend`.

```go
type QuotaManager interface {
    // Acquire upserts one sandbox's footprint + scope membership and, when enforcing, admits it or returns
    // ErrQuotaExceeded (403). Unlimited keys are a no-op (zero Redis IO). Idempotent on the lockstring.
    Acquire(ctx, req AcquireRequest) error
    // Release returns a charged sandbox (subtracts its footprint from all its scopes' sums and removes the
    // entry). Idempotent. Request-side callers issue it with context.WithTimeout(context.WithoutCancel(ctx),
    // quotaReleaseTimeout); maintenance/event callers provide their own bounded contexts.
    Release(ctx, req ReleaseRequest) error
}
// AcquireRequest carries quota subject/user K, lockstring, the new footprint (FootprintOf), the new conditional scopes
// (ConditionalScopesOf), the loaded QuotaSpec limits, and an `enforce` flag (true on the create hot path,
// false on every leader-driven reconcile call).
```

There is no server-side reservation handle: the persisted lockstring is the only idempotency key, and `Release`
is lockstring-keyed.

The **anti-drift / reconcile driver** (leader-gated, §6.4) is not part of the request-serving interface; it lives
in the quota layer (§6.4.2) and reuses `Acquire(enforce=false)` / `Release`. `pkg/cache` only exposes the
owner-indexed live-CR read and the informer-health signal, and accepts the registered event handler — it never
drives reconcile.

### 6.1 Backend selection (pluggable, optional Redis)

- **Redis configured** → `redisBackend` (behind the circuit breaker): full enforcement.
- **Redis not configured** → `noopBackend`: `Acquire` always allows (fail-open), `Release` / anti-drift are
  no-ops. Setting a **non-empty** quota at key create is **still accepted** (§3.1) and persisted; it is simply
  unenforced while no Redis exists. API-key create/list responses still show the stored static quota, but Phase
  1 has no dynamic usage reporting and never fabricates `0` usage. This is identical to the
  Redis-configured-but-unavailable case (§9), so "Redis absent" and "Redis down" behave the same for enforcement.

### 6.2 Redis keys (per limited key K; every key hash-tagged `{K}` → one slot)

| Redis key | Type | Field → Value | Meaning |
|---|---|---|---|
| `q:live:{K}` | HASH | lockstring → `cjson({d:{<dim>:<amount>...}, s:[<conditional scope>...]})` | The live set. `d` carries the cpu/memory footprint (**no `count`** — it is the implicit 1); `s` carries the **conditional** scopes (**no `all`** — it is implicit while the entry exists). Membership in `all` ⟺ the entry exists. |
| `q:sum:{K}:<dim>` | HASH | scope → integer | The incrementally-maintained rollup for dimension `<dim>`. One hash per dimension (scheme A). `q:sum:{K}:sandbox.count` always exists. `<scope>` includes the implicit `all` plus any conditional scope the key's sandboxes occupy. |

- `value(dim, scope, K)` = `HGET q:sum:{K}:<dim> <scope>` (absent field reads as 0). There is **no `HLEN`-based
  count** — `count` is just `q:sum:{K}:sandbox.count`, maintained like any other dimension (each entry contributes
  the implicit 1 to every scope it occupies).
- **count is structural, not stored per-entry** (every entry is one sandbox); **`all` is structural, not stored
  per-entry** (every live entry occupies it). The Lua adds both implicitly. This keeps the entry storing only the
  non-obvious state: the cpu/memory amounts and the conditional scope list.
- **Integer units + `HINCRBY` only** (never `HINCRBYFLOAT`): cpu in millicores, memory in MiB, count in
  sandboxes — all integers, so long-lived incremental sums never accumulate float drift. A defensive
  `max(0, …)` floor guards against any underflow.
- **Sum fields are never deleted.** A `(dim, scope)` field that decrements to 0 is **kept** (not `HDEL`-ed; the
  per-dimension hash is not `DEL`-ed when empty). The set of dimensions is small and bounded, so the memory is
  negligible and it removes all field-cleanup/empty-hash complexity. The only deletion is the whole-key cleanup on
  API-key delete (§6.6).
- **No global keys.** Because Phase 1 is fail-open (§9) there is **no** `q:warm`/settle barrier. Every Lua touches
  only `{K}`-tagged keys, so with the hash-tag everything for a key co-locates in one slot — **Redis Cluster
  causes no `CROSSSLOT`** (standalone / Sentinel / Cluster are all structurally compatible; officially
  testing/supporting Cluster is a separate call). The global cold/warm barrier returns only with the deferred
  fail-closed posture (§16), at which point the Cluster question reopens.

### 6.3 Acquire (the single upsert) — atomic, idempotent

`Acquire` is one Lua script, run on **every** replica for the create hot path (`enforce=true`) and by the leader
for reconcile (`enforce=false`). It reads the entry's old `(footprint, scopes)`, diffs against the new value,
admits (when enforcing), and applies the per-`(dim, scope)` deltas. Redis is the serialization point; no
leadership on the hot path.

```lua
-- KEYS: q:live:{K}, and q:sum:{K}:<dim> for each dimension touched (all hash-tagged {K}).
-- ARGV: lockstring, new footprint d={<dim>:<amount>}, new conditional scopes s=[...], enforce flag,
--       and, when enforcing, the limited (dim,scope)->limit map.
-- Dimension domain D    = old.d keys ∪ new.d keys ∪ {sandbox.count}   (UNION of OLD and NEW dims — so a
--                                                                       dimension dropped from the new footprint
--                                                                       still gets its negative delta applied)
-- Effective new scopes = s ∪ {all}                          (Acquire always means "live", so `all` is included)
-- Effective old scopes = (entry exists) ? old.s ∪ {all} : {}   (absent entry contributes nothing yet)

old := HGET(q:live:{K}, lockstring)            -- nil, or cjson(old footprint + conditional scopes)

-- For each dim in domain D, per (dim, scope) delta from the add/update/delete classification:
--   add    (scope ∈ new \ old): +new_amount(dim)
--   update (scope ∈ new ∩ old): +(new_amount(dim) - old_amount(dim))
--   delete (scope ∈ old \ new): -old_amount(dim)
-- where new_amount(sandbox.count)=old_amount(sandbox.count)=1; resource amount = value in that state's footprint
-- (old.d for old, new.d for new), or 0 if absent in that state. Iterating dim over the full domain D (not just
-- new.d) is what guarantees an old-only dimension is correctly drained to 0.

if enforce then
  -- Only pairs being *charged more* can breach a cap; a delta<=0 (idempotent re-entry, or a release-direction
  -- change) must NEVER be rejected — otherwise a fail-open oversell (value>limit) would wrongly reject retries.
  for each limited (dim, scope) with delta > 0 do
    if value(dim, scope) + delta > limit(dim, scope) then return 'REJECTED' end   -- atomic; writes nothing
  end
end

for each (dim, scope) with delta ~= 0 do HINCRBY(q:sum:{K}:<dim>, scope, delta) end   -- skip delta==0
if new_entry ~= old then HSET(q:live:{K}, lockstring, cjson(new_entry)) end              -- skip if unchanged
-- NOTE: the "unchanged" test must compare CANONICAL encodings (stable key/element order) or decoded values, so an
-- idempotent re-entry truly writes nothing; a non-canonical re-encode could differ byte-wise and HSET needlessly.
return 'OK'
```

- **Unlimited key** (empty `QuotaSpec`) → `QuotaManager.Acquire` short-circuits **before any Redis call** (zero
  IO) — the majority of the 2500/sec. The Lua only ever runs for a limited key.
- **`enforce=true` (create hot path):** `old` is absent → every scope is an *add*, every delta is the full new
  amount (positive), so the check is `value + new_amount <= limit` per limited `(dim, scope)` the new sandbox
  occupies (it occupies `all` and `running` at create). `OK` → proceed with create (the lockstring is the one
  already stamped on the CR by `LockSandbox` — no extra CR write). `REJECTED` → HTTP **403**, returned
  **immediately with no retry** (a pooled sandbox tentatively picked before the charge is returned to the pool —
  §10).
- **`enforce=false` (leader reconcile):** never rejects (anti-drift reflects reality; it cannot reject an
  existing sandbox), just applies the deltas. Used for pause/resume/footprint changes and for the full-diff
  charge direction.
- **Idempotency is intrinsic.** A retry with the same `(footprint, scopes)` reads `old == new` → all deltas 0 →
  the script does only the `HGET` and returns `OK`: **zero writes, no double-charge**. This replaces the version
  guard and the `HEXISTS` short-circuit entirely. Lockstrings are fresh per-create UUIDs, never reused.
- **Redis transport error / circuit open** → **fail-open** (treat the key as unlimited for this request; allow),
  §9. Confirmed Phase 1 product decision.

### 6.4 Removal & reconcile — conservative request-side, event-driven, leader backstop

`Release` is one atomic, idempotent Lua that subtracts the entry's footprint from **all** its scopes' sums
(including the implicit `all` and the implicit `count`) and removes the entry:

```lua
-- KEYS: q:live:{K}, q:sum:{K}:<dim> for each dimension. ARGV: lockstring.
old := HGET(q:live:{K}, lockstring); if not old then return 0 end          -- idempotent: already gone
for each scope in (old.s ∪ {all}) do
  HINCRBY(q:sum:{K}:sandbox.count, scope, -1)
  for each (dim, amount) in old.d do HINCRBY(q:sum:{K}:<dim>, scope, -amount) end
end
HDEL(q:live:{K}, lockstring); return 1                                      -- sum fields kept (may now read 0)
```

A slot is freed only once its sandbox's deletion is **accepted or proven** — never speculatively. Paths (all
idempotent, safe to overlap):

1. **Create failure → conservative release.** The replica handling the create releases the charge **only when it
   is provable that no live CR remains charged by this attempt**: either the failure occurred strictly before any
   CR write, or the attempt did create a CR and the failed-sandbox cleanup successfully requested deletion for
   that CR (so it is no longer `IsLiveForQuota`). On an **ambiguous** failure (a CR may have been committed and
   cleanup did not prove deletion acceptance — e.g. a post-write timeout), it **keeps** the charge. If no live CR
   in fact exists, the bidirectional anti-drift (§6.4.2) releases the leaked entry; if a live CR does exist, the
   charge was correct. The trade is a bounded **under-sell** (over-rejection), never an over-sell.

   **Reserved failed sandbox (positive `ReserveFailedSandboxFor`).** When the create *succeeds* (CR persisted,
   lockstring stamped) but a post-lock step fails (waitReady / initRuntime / csi), the cleanup path may
   **reserve** the failed sandbox for a configurable retention window (default 30 min) before delayed deletion,
   preserving it for diagnosis. In this case the sandbox is `IsLiveForQuota` (no `DeletionTimestamp`, not
   `Terminating`), carries a valid `AnnotationLock`, and **intentionally continues to occupy its quota slot for
   the entire retention duration**. The charge is **not released** on this path — it accurately reflects a live
   CR. The slot is freed only when the retention timer fires, deletion is requested, and the sandbox enters
   `Terminating` (at which point the leader event handler or anti-drift releases the entry, path 3/4).

   Because the outer retry loop may `Acquire` a new slot for the next attempt, a single user request can
   transiently hold **multiple** quota slots (one per reserved failed sandbox plus the eventual success). This is
   a **bounded under-sell** (over-rejection for other concurrent creates on the same key): bounded by
   `maxRetries × retention`, self-healing as each retained sandbox expires and is deleted. It is **never an
   over-admission** — each charged slot corresponds to a real live CR. This trade-off preserves the
   conservative-release guarantee (§7) and avoids any extra apiserver write on the failure path to strip the
   lockstring.
2. **Manager `DELETE /sandboxes/{id}` → release after deletion is accepted.** The handler issues the delete and
   releases the lockstring **only after the apiserver has accepted the deletion** (the CR has a
   `DeletionTimestamp` / has entered Terminating → no longer `IsLiveForQuota`). Not pre-emptive: the slot is
   genuinely free at release time, so it cannot over-admit. Still low-latency — it does not wait for physical pod
   GC.
3. **Leader event reconcile** (covers pause/resume, footprint changes, and non-manager deletions — TTL,
   `kubectl`, controller-driven). On a Sandbox owner-CR event, the leader recomputes `(footprint, conditional
   scopes, IsLiveForQuota)` and: if the CR is **not** `IsLiveForQuota` → `Release(lockstring)`; otherwise →
   `Acquire(enforce=false)` with the new value. A pause therefore subtracts only the `running` sums (the entry
   stays, still in `all`); a resume re-adds them; a delete/terminate removes the entry entirely. Runs **only on
   the leader** (`IsPrimary()`); freeing is cache-health-gated (§6.4.2); idempotent w.r.t. paths 1–2.
4. **Leader anti-drift backstop** (§6.4.2): the periodic bidirectional diff that catches whatever the events
   missed.

#### 6.4.1 Why the bounded transients are safe

- **Deletion-requested = freed.** A released sandbox that is stuck `Terminating` still physically exists until GC,
  so for the window between deletion-acceptance and true deletion a key can have *physical pods* `> limit`. The
  **quota charge stays correct** throughout (the terminating sandbox is uncharged, the replacement is charged), so
  this is **not** a quota over-admission, only a transient excess of physical pods, bounded by concurrent
  terminations. The anti-drift charge direction only considers `IsLiveForQuota` CRs, so it never resurrects an
  intended deletion.
- **`running` scope via un-gated `resume`.** Admission is gated **only at create** (where the sandbox enters the
  `all`+`running` membership). `resume` (paused→not-paused) re-enters `running` through the leader event, which
  charges **without** a limit check (it reflects reality, like any reconcile). So a burst of resumes can push a
  `running`-scope sum **above** its limit. This is the deliberately-accepted relaxation (the user confirmed
  "running spans create→Running, orthogonal to Paused"): the leader converges the sum to the truth and **never
  drains** existing sandboxes; further *creates* charging that pair are rejected until the sum falls below the
  limit. It mirrors the existing tolerance "anti-drift converges to truth; usage may legitimately remain >
  limit; new creates stay blocked until it drops." Gating `resume` (and `pause`) through admission is a deferred
  hardening option (§16).

#### 6.4.2 Bidirectional anti-drift (primary-aware driver in `pkg/sandbox-manager/quota`)

This is the **single** correction primitive; it makes drift in **either** direction self-heal, and — because all
Redis writes go through the same `Acquire`/`Release` Lua — it needs **no separate sum recompute** (see "co-write
invariant", §7). Critical: in an incremental live-set model the backstop must be **bidirectional** — a
"remove-only" GC would let a lost entry undercount forever (permanent oversell), since nothing re-charges it.

**Placement & layering.** The reconcile **driver** lives in `pkg/sandbox-manager/quota` with the Redis backend.
It does **not** depend on E2B API-key storage. `pkg/cache` only exposes the live-CR read primitive
`ListLiveSandboxesByOwner` (§5, over `IndexUser`) and the `SandboxInformerHealthy()` signal. The event-driven
reconcile (path 3) is a raw Sandbox informer event handler (`toolscache.ResourceEventHandler`) owning the
`QuotaManager`, registered into the cache via `AddSandboxEventHandler` (returning a `SandboxEventHandlerRegistration`
whose sync also feeds `SandboxInformerHealthy()`); cache never imports quota — no cycle. Path 3 is **best-effort**
(no workqueue / no retry): a transient Redis error during an event reconcile is logged and counted and left for the
periodic full bidirectional diff (path 4) to converge; a CR-gone arrives as a `Delete` event (handling
`DeletedFinalStateUnknown`), not a reconciler `notFound` signal.

**Subject source.** sandbox-manager receives a narrow subject lister supplied by E2B:

```go
type Subject struct {
    User  string
    Quota *QuotaSpec
}

type SubjectLister interface {
    ListLimited(ctx context.Context) ([]Subject, error)
    Load(ctx context.Context, user string) (Subject, bool)
}
```

E2B implements this adapter from its key store, but sandbox-manager only sees quota subjects. This preserves the
boundary: key deletion and static quota config remain E2B concepts; dynamic quota cleanup/reconcile remain
sandbox-manager concepts. Redis is never `SCAN`-ed to discover subjects. Full-rebuild duration after a Redis
loss is bounded by `#limited-subjects × per-subject work`; the implementation paginates/budgets per cycle,
jitters, and exports diff-lag / rebuild-duration / divergence metrics (§15). For each enumerated subject K:

1. `live := ListLiveSandboxesByOwner(K)` → the authoritative CRs (informer only). For each, derive `(lockstring,
   FootprintOf, ConditionalScopesOf)`.
2. `have := HGETALL q:live:{K}` → the current Redis entries (lockstring → footprint + conditional scopes).
3. Diff per lockstring:
   - **Charge / correct (CR live):** if absent from `have`, or present but with a different footprint/scope set →
     `Acquire(enforce=false)` with the CR-derived value. The upsert immediately adds a missing entry (recomputing
     the footprint from the CR — no snapshot annotation, by design), **or** applies exactly the deltas that fix a
     footprint/scope drift the events missed. `enforce=false`, so it never rejects an existing sandbox. Heals lost
     entries and rebuilds after Redis loss without waiting for the CR to age.
   - **Release (CR gone / not live):** lockstring in `have`, no matching `IsLiveForQuota` CR → `Release`. Frees
     failed-create leftovers (path 1's kept charges) and deletions the event handler missed.

There is **no separate `resyncSums`**: every charge/correct/release goes through the same atomic Lua that updates
the entry **and** its sums together, and the co-write invariant guarantees the entry and its sum contributions
were never out of sync to begin with (lost together, survive together). Re-charging a key from whatever survived a
loss is therefore self-consistent. (A belt-and-suspenders per-key "recompute sums from current live entries and
atomically correct" pass MAY be added to the infrequent diff to defend against genuine corruption outside Redis's
atomic-replication guarantee; it is **not required for correctness** and is optional.)

- **Cache-health gate (never APIReader).** All **Sandbox CR** reads are informer reads (subjects come from the
  `SubjectLister`). The gate is **not one-time**: **every release-capable pass** — both the periodic diff and the
  event-driven reconcile (path 3) — first checks `SandboxInformerHealthy()` (≥1 full list completed, `HasSynced`
  true, no watch error / outstanding relist since the last successful sync) and **skips the release direction**
  otherwise. So a freshly-(re)elected leader, a mid-run relist, or a watch-bookmark gap can never make a
  partial/cold cache look "empty" and wrongly free a live slot. The **charge** direction is always safe (it only
  ever charges existing CRs). Even a spuriously-released entry **self-heals** — the charge direction re-adds the
  still-live CR on a later charge pass; correctness does not *rest* on the gate, the gate just shrinks the
  transient. A lagging-but-synced informer is also safe — a create not yet in cache defers the charge; a delete still in cache
  defers the release — both converge on the next pass. A **subject-listing error** likewise skips that
  cycle (metric + log) and is **never** interpreted as an empty subject set. Skipped passes are counted.
- **No per-entry timestamp.** The live entry carries **no `ts`**. The full-diff **release of a leaked entry**
  (CR-gone, which has no CR to read an age from) is gated instead by **leader-local "seen-leaked in two
  consecutive passes"** memory plus the cache-health gate: an entry is freed only if it looked leaked on the
  previous pass too. The implementation MUST pick a diff cadence such that the span of two consecutive passes
  **exceeds the worst-case apiserver lag**, so two passes comfortably clear the ~1-minute lag — giving the same
  protection the old time-grace did without storing anything per entry. On leader failover the in-memory set resets
  — safe, it only delays a GC. The **charge** direction deliberately has no age grace: a live CR missing from Redis
  is acquired immediately with `enforce=false`, which is idempotent with any late-arriving hot-path `Acquire`.
- **Cadence / cost.** The *release* direction is driven primarily by the leader's informer **events** (path 3) at
  event speed; the periodic full **bidirectional diff** is an **infrequent** backstop (minutes). Every read is
  informer-served — **no apiserver `List`** anywhere — and the per-subject work (`ListLiveSandboxesByOwner` by
  index + `HGETALL`) is bounded by that subject's **actual live-set size** (normally near its limits, but
  legitimately larger during a fail-open/rebuild window or an accepted lifecycle over-limit, §7.1), not by the
  500k cluster total.
- **Primary-aware run loop.** Every sandbox-manager replica starts the anti-drift driver, but the driver blocks
  until the local manager becomes primary. When primary is acquired it immediately runs one full diff, then runs
  on the configured interval while primary. When primary is lost, the current cycle context is canceled, local
  leaked-entry observations are cleared, and the driver returns to the primary wait. Event handlers are
  registered on every replica but process events only while primary. This avoids waiting a full interval after
  failover and stops long cycles promptly when leadership changes; correctness still rests on idempotency, not on
  perfect singleton execution (§8).

### 6.5 New keys need no seed

A brand-new limited key provably owns **zero** sandboxes, so absent `q:live:{K}` / `q:sum:{K}:*` legitimately mean
0 — `Acquire` simply starts charging from empty (`HINCRBY` on a missing field starts at 0). There is **no**
`NEED_SEED` round-trip and no per-key consistent read on the cold path. The only case where absent state does
**not** mean "truly zero" is a Redis data loss for an already-active key; that is handled by fail-open + the leader
rebuild (§9), the same bounded self-healing envelope, not by a per-acquire seed.

### 6.6 Subject cleanup after key deletion

API-key deletion removes static key config in the E2B layer. Dynamic quota cleanup is a separate sandbox-manager
operation: after a limited key is deleted, E2B calls `SandboxManager.CleanupQuota(ctx, user)` with the resolved
quota subject. sandbox-manager then deletes `q:live:{K}` plus `q:sum:{K}:<dim>` for each dimension in the known,
bounded dimension set — all `{K}`-tagged (one slot, Cluster-safe).

This is the **sole** cleanup for a deleted subject's Redis state: once the key is deleted, E2B's subject lister no
longer returns it, so the anti-drift driver no longer reconciles it and there is **no `SCAN` sweep**. To shrink the
leak window the `DEL`s are retried a bounded, **non-blocking** number of times off the hot path; if they still fail
the residue is harmless dead memory (subject IDs are fresh, never reused) that is never read again — monitor Redis
memory for it. Cleanup failure is **non-fatal** and must not roll back or fail the already-accepted static key
deletion.

### 6.7 Quota lifecycle: immutable in Phase 1 (mutability is planned, not forbidden)

For Phase 1 a key's `QuotaSpec` is **set only at creation and immutable thereafter — there is no quota `PATCH`**.
This is a deliberate Phase 1 scope-down, **not** a permanent design constraint. It avoids, for now, the only
normal-operation oversell this design would otherwise have to handle — the **`unlimited → limited` transition
race** (while unlimited the hot path bypasses Redis, so a later re-limit would leave CRs created by a
stale-`unlimited` replica uncharged while a fresh-`limited` replica admits against a short live set):

- A key created **limited** is enforced from its first `Acquire`; it owns zero sandboxes at birth (§6.5), so there
  is no prior unlimited interval leaving uncharged CRs.
- A key created **unlimited** stays unlimited for Phase 1; the hot path always bypasses Redis for it.
- A replica that has not yet observed a newly-created key resolves it as **unknown** (auth failure), never as
  "unlimited" — so no replica ever holds a stale `unlimited` view of a limited key.

**Future work (§16), explicitly planned:** changing a *limited* key's limits, and **promoting `unlimited →
limited`**, are both intended to be supported in a later phase. Each requires a **safe-activation scheme** (e.g. an
activation window or an all-replica `QuotaSpec` cache invalidation that drains stale-`unlimited` admissions before
enforcement begins). Phase 1 simply does not implement mutation; nothing here forecloses it.

## 7. Correctness

Admission (create hot path, `enforce=true`) grants iff, for every limited `(dim, scope)` the new sandbox occupies
with a positive delta, `value + delta <= limit`, computed and committed **atomically** in the Acquire Lua. **While
Redis is healthy**, at the instant of every admission each `value` equals the exact charged sum, so a grant cannot
exceed any limit — **strict enforcement at admit**.

The whole-system invariant is **convergence**: the Redis live set converges to the cluster's set of `IsLiveForQuota`
owner-K CRs (by lockstring), each with the correct footprint and scope membership, and every
`q:sum:{K}:<dim>[<scope>]` equals the sum of the matching entries' contributions. This rests on:

1. **One per-lockstring idempotent, per-op atomic upsert.** Every Redis mutation is one Lua script keyed by a
   stable lockstring; idempotency is intrinsic to the old→new diff (re-applying a value yields zero deltas). So
   concurrent acquires, retries, leadership handoff, and double-fired releases all converge — **no version guard,
   no stop-the-world.**
2. **The co-write invariant.** `q:live:{K}` and the matching `q:sum:{K}:<dim>` fields are **always mutated within
   the same Lua script**, and every key is `{K}`-hash-tagged into one slot. Redis applies and replicates a
   script's effects atomically (effects wrapped in `MULTI/EXEC`; AOF loads a partial `MULTI/EXEC` block as
   nothing), so a replica/AOF never reflects half a script. Therefore an entry and its sum contributions are
   **never out of sync** — they are written together, survive together, and are lost together on a bounded
   failover. This is what lets the anti-drift rebuild re-charge from whatever survived **without** a separate sum
   recompute (§6.4.2). *(Implementation must preserve this: never mutate a `q:sum` field outside the Acquire/
   Release/cleanup scripts.)*
3. **Bidirectional anti-drift (§6.4.2).** Every divergence is corrected: a live CR with no/incorrect entry is
   charged/corrected; an entry with no live CR is released. This keeps lost entries (Redis restart / failover /
   rollback) from drifting permanently — the property a remove-only GC would lack.
4. **Conservative release (§6.4).** A charge is dropped only when provably pre-CR or after deletion is accepted,
   so a healthy-state release can never free a slot a live CR still holds.
5. **Membership predicates (§5).** `all` membership ⟺ `IsLiveForQuota`; `running` membership ⟺ live and not
   paused. The anti-drift charge direction only considers `IsLiveForQuota` CRs, so it never fights a deletion.

### 7.1 Honest no-oversell statement

- **Redis healthy, `sandbox.count` / `limits.cpu` / `limits.memory` over the `all` scope, and `running` modified
  only by create/delete:**
  enforcement is **strict at admit** *and* **release is conservative**, so there is **no over-admission**. The
  only healthy-state transient is a bounded **under-sell** (over-rejection): an ambiguous failed create that
  charged but produced no CR holds a leaked entry until anti-drift releases it.
- **Reserved failed sandbox under-sell (§6.4 path 1).** A create that succeeds but whose post-lock step fails
  with positive `ReserveFailedSandboxFor` holds its quota slot for the retention window. If the outer retry loop
  succeeds, both the reserved failed sandbox and the successful sandbox occupy quota simultaneously — a bounded
  under-sell (over-rejection for the same key), self-healing when the retention expires and deletion frees the
  slot. Never an over-admission (each slot maps to a live CR). Bounded by `maxRetries × retention`.
- **`running` scope via un-gated `resume` (§6.4.1):** a burst of resumes can push a `running` sum above its
  limit. The quota charge tracks the truth; the leader never drains; further creates charging that pair are
  rejected until it falls. Bounded by the number of paused sandboxes a key can resume; a **deliberately-accepted
  relaxation**, self-healing in the rejection direction.
- **Physical-pods transient (not a quota over-admission):** "deletion-requested = freed" lets a stuck-terminating
  pod plus a replacement briefly exceed `limit` in *physical pods* while the quota charge stays exactly correct.
  Bounded by concurrent terminations.
- **Redis unavailable / absent (confirmed product decision = fail-open):** limited keys are **temporarily
  unenforced** (treated as unlimited). Oversell during the outage is bounded by the outage's create volume and
  self-heals once Redis returns and anti-drift rebuilds. The circuit breaker (§9) only changes *how* fail-open is
  detected (cheaply, without per-request probes), not the posture.
- **Lost Redis entries while the key stays active:** transient over-admission until the leader rebuild re-charges;
  under fail-open the gap is "unenforced", not "rejected".

In short: **strict at admit and no over-admission while Redis is healthy** — the bounded exceptions are the
`running`/resume relaxation, a Redis outage, and a post-loss rebuild window, all bounded, self-healing, and
confirmed availability-favoring decisions. Phase 1 never permits *unbounded* oversell.

Implicit precondition: **every path that stamps `owner=K` onto a CR goes through `Acquire`** (so the charge
precedes the CR). Phase 1 stamps owner only on the E2B create paths (claim/clone), which all go through `Acquire`.

## 8. Generic "primary manager" leadership

The hot path (`Acquire` / the path-1/2 release) runs on **all** replicas. Only the leader-side
reconcile/anti-drift (§6.4 paths 3–4) benefits from running once.

- `SandboxManager` gains a **generic** primary signal, backed by a single `coordination.k8s.io/Lease`
  (`sandbox-manager-primary`) via the vendored `client-go/tools/leaderelection` — intentionally not coupled to
  quota, so future singleton tasks can reuse it. The minimal surface is `IsPrimary()` plus a blocking/notification
  primitive such as `WaitPrimary(ctx)` or `PrimaryChanged()`.
- The quota anti-drift **driver** (in `pkg/sandbox-manager/quota`) is started on every replica. It waits until
  the local manager becomes primary, immediately runs one full diff, then runs periodic diffs while primary. On
  primary loss it cancels the active cycle, clears leader-local leaked-entry observations, and waits again.
- Event-driven reconcile is registered into the cache on every replica but handles events only while primary. The
  hot path and per-request release are **not** gated.
- **Leadership carries no correctness weight.** Correctness rests on per-lockstring idempotency + the co-write
  invariant (§7). If leadership flaps/splits, the worst case is anti-drift running on several replicas at once —
  idempotent, hence safe.
- RBAC: `get/list/watch/create/update` on `coordination.k8s.io/leases` in the manager namespace (one lease).

## 9. Degradation & Redis Data Loss (Phase 1: fail-open with a circuit breaker — confirmed product decision)

Phase 1 posture: **fail-open** on any Redis trouble; rely on the leader's bidirectional anti-drift to rebuild.
This is an explicit, confirmed availability-over-enforcement decision — see §7.1.

- **No Redis configured** → `noopBackend`: all keys unenforced (unlimited); a non-empty quota is still accepted
  and persisted (§6.1) and surfaced as static quota on API-key create/list responses. No usage field.
- **Redis transiently unreachable** (restart, network error, failover unreachable phase): limited-key `Acquire`
  **allows** (treated as unlimited for that request); unlimited keys unaffected. Bounded oversell, self-healing.
- **Redis data loss** (cold restart / flush / first boot): the hashes are empty, so `Acquire` reads value 0 and
  allows — which **is** the fail-open behaviour; no special detection needed. The leader's anti-drift **charge**
  pass repopulates `q:live:*` / `q:sum:*` by enumerating limited subjects through the Redis-independent
  `SubjectLister` (§6.4.2) and reading each subject's live CRs off the **informer** (`IndexUser`, never
  APIReader), recomputing each footprint from the CR. Enforcement resumes per subject as its entries are rebuilt.
- **Partial rollback / async failover** (some scripts lost): each lost script is lost **whole** (co-write
  invariant, §7), so the surviving state stays self-consistent and the **charge** direction re-adds the missing
  live CRs on the next leader anti-drift pass. No detection key is needed.
- **Redis config removed after keys exist** (operational): treated exactly as a Redis **outage** — limited keys
  fall to fail-open. No special cross-check.

### 9.1 Circuit breaker (cheap fail-open during a sustained outage)

To avoid paying a Redis round-trip (and its timeout) on **every** limited-key request during an outage, the
`redisBackend` is wrapped by a small circuit breaker:

- **Closed (healthy):** requests hit Redis normally.
- **Open:** after **N consecutive Acquire failures** (transport error / timeout; default **N = 3**), the breaker
  opens for a cooldown window (default **D = 30s**). While open, create-path `Acquire` **fails open immediately
  without touching Redis** (the limited key is treated as unlimited), so a sustained outage costs no Redis IO on
  limited-key admission requests. Request-side `Release`, key cleanup, `ListEntries`, and leader reconcile bypass
  the acquire breaker and talk to Redis directly with their own bounded contexts; their failures are logged/counted
  and do not reset acquire-breaker state.
- **Half-open:** after the window, the next probe is allowed through; success → close, failure → re-open for
  another window.
- `N` and `D` are configurable. The breaker is purely an **optimization of the fail-open path** — it changes how
  unavailability is detected, never the posture; correctness still rests on the leader rebuild once Redis returns.
  Breaker state transitions and open-duration are exported as metrics (§15).

> **Deferred (later PR): fail-closed posture.** A config knob `onRedisUnavailable: allow | reject` (default
> `allow`). `reject` reintroduces strictness across loss: a global `q:warm` total-loss detector, a Redis-clock
> settle window, `COLD → 503` retryable on the hot path, and optionally synchronous replication (`WAIT` / sync
> topology). Its global keys are also what reopen the Redis Cluster `CROSSSLOT` question (§6.2). **Not** built in
> Phase 1.

Operational recommendation to keep loss rare even under fail-open: Redis AOF `appendfsync everysec` + HA
(Sentinel-managed primary with a replica).

## 10. Create Hot Path (limited key)

1. `CheckApiKey` already put `user` (with `QuotaSpec`) in context.
2. E2B calls sandbox-manager with manager-level claim/clone options: `User`, `Quota`, the E2B-owned sandbox
   `Modifier`, and existing create settings. E2B does **not** build `infra.SandboxAdmission`.
3. sandbox-manager converts manager options to infra options and, for a limited quota, creates the Admission that
   calls `QuotaManager.Acquire/Release`.
4. **Unlimited key** → `Acquire` returns immediately; no Redis, no leadership lookup; zero cost.
5. **Limited key** → after claim/clone has prepared and locally modified the final Sandbox object, derive the
   admission footprint from that object's resource data (`limits.cpu` / `limits.memory`; `sandbox.count` remains
   implicit) and derive `ConditionalScopesOf` (at create the sandbox is live and not paused → it occupies `all` +
   `running`). One Lua `Acquire(enforce=true)`:
   - `OK` → proceed.
   - `REJECTED` → **HTTP 403 immediately, no retry**; if a pooled sandbox was tentatively picked, return it to
     the pool (do not lock it). `TryClaimSandbox` must surface the quota miss as terminal, not loop.
   - transport error / breaker open → **fail-open** (allow).
6. The `lockstring` is the one `LockSandbox` already stamps on the CR — no extra CR write. The admission hook
   receives the final local sandbox resource data, so the E2B layer does not need to pre-resolve templates or
   inspect Sandbox CR internals to compute the quota footprint.
7. Run infra `ClaimSandbox` / `CloneSandbox`. On failure → **conservative release** (§6.4 path 1): release with a short
   bounded request context (`context.WithTimeout(context.WithoutCancel(ctx), quotaReleaseTimeout)`) **only** if
   the failure is provably pre-CR or failed-sandbox cleanup has successfully requested deletion of the attempt's
   CR; otherwise keep the charge for anti-drift. Release failure/timeout is logged and counted but never changes
   the create failure being returned. On success → nothing; the entry already reflects the live sandbox.

`DELETE /sandboxes/{id}` remains authenticated and authorized by E2B, then E2B calls sandbox-manager with
`User`, `Quota`, and the sandbox. sandbox-manager releases the lockstring (§6.4 path 2) **after** the apiserver
accepts the deletion (CR → deletion-requested), for low-latency-but-safe slot return. This release also uses the
short bounded request context, and release failure/timeout only logs/metrics; it must not block or fail the 204
response beyond that short timeout. The leader's event handler (path 3) is the backstop for all non-manager
deletions, and the sole driver of `running`-scope adjustments on pause/resume.

## 11. API Surface

- **Create** (`POST /sandboxes`): unchanged shape; quota enforced internally; exceeded → **403** with the
  existing E2B error body (the spec-compliant `{code,message}` shape used by other E2B error paths), no retry.
- **Key create** (`POST /api-keys`): optional `quota` in the canonical `QuotaSpec` shape; **admin-only** to set;
  validated (§3.1). Public quota JSON is `{"limits":[...]}` and uses full dimension keys (`sandbox.count`,
  `limits.cpu`, `limits.memory`) in `QuotaLimit.dimension`; short keys are not supported. **Accepted regardless
  of Redis presence** (unenforced if no Redis, §6.1).
- **No quota `PATCH` in Phase 1** (§6.7): immutable after create; change quota by creating a new key. (Quota
  mutation and `unlimited → limited` promotion are **planned** for a later phase, §16.)
- **No Describe / usage reporting in Phase 1:** do not add a public quota Describe endpoint, dynamic usage field,
  or internal `QuotaManager.Describe`/`QuotaStatus` surface. API-key create/list responses expose static quota
  only. Metrics/logs may report backend health and fail-open events, but user-facing API responses must not
  fabricate usage when Redis is absent/unavailable.
- **Key delete** (`DELETE /api-keys/{id}`): drop the static API-key config; **keep** existing sandboxes. If the
  deleted key carried limited quota, E2B separately calls `SandboxManager.CleanupQuota(ctx, user)` to remove
  dynamic Redis state (`DEL q:live:{K}` + `DEL q:sum:{K}:<dim>`, §6.6). Static deletion and dynamic cleanup are
  decoupled operations; cleanup is bounded, best-effort, non-fatal, and uses no `SCAN`.

Authorization reuses `CheckCreateAPIKeyPermission`; quota set chains `CheckApiKey` + admin check.

## 12. Compatibility

- Old keys without `quota` → unlimited; the new JSON field is `omitempty` / nullable.
- Public quota JSON is pre-release in this branch. Removing short dimension keys and the old scope-outer quota
  shape is therefore accepted; all branch tests and fixtures must use the canonical `{"limits":[...]}` shape with
  `sandbox.count` / `limits.cpu` / `limits.memory`.
- `CountActiveSandboxes` untouched; SandboxClaim self-healing preserved.
- No owner label and **no backfill**: anti-drift reads live CRs off the existing `IndexUser` informer index (§5).
- New RBAC: one generic lease (§8).
- New dependency: a Redis client (dormant unless configured). Phase 1 has **no global Redis keys**, so
  standalone / Sentinel / Cluster are all structurally compatible (§6.2).
- No change to E2B lifecycle semantics beyond create-time admission, the removal paths of §6.4, and the
  leader-driven `running`-scope/footprint reconciliation.

## 13. Risks

- **Redis memory for the live set + sums.** Storing every live sandbox (lockstring + small footprint JSON +
  scope list) plus a handful of per-dimension sum hashes — on the order of tens of MB at 500k. Monitor memory
  and per-key cardinality.
- **Footprint recompute consistency.** The hot path derives the footprint from the final local sandbox resource
  data immediately before the create/update write; anti-drift recomputes it from the CR. Both must flow through
  the same infra resource extraction helper to get `infra.SandboxResource` and the same
  `infra.SandboxResource -> quota dimensions` mapper in `pkg/sandbox-manager/quota`. Phase 1 has no manager
  resize, so they are stable; an out-of-band in-place update changes the CR's resources and is reconciled by the
  leader (§6.4.2).
- **`running` over-limit via resume.** A deliberately-accepted bounded relaxation (§6.4.1/§7.1); the leader never
  drains. If strict `running` enforcement is needed, gate pause/resume through admission (§16).
- **Under-sell from a kept ambiguous-failure charge.** Conservative release (§6.4 path 1) can hold a leaked entry
  until anti-drift releases it, transiently over-rejecting a high-churn key. The deliberate trade for never
  over-admitting while healthy; bounded and self-healing.
- **Deletion-requested = freed → transient excess physical pods** (§6.4.1). Bounded by concurrent terminations;
  not resurrected by anti-drift (live-CR-only charge).
- **Fail-open during Redis outage = temporary no enforcement.** Bounded oversell for the outage duration,
  self-healed by rebuild; the circuit breaker keeps it cheap. The deferred fail-closed knob trades availability
  for strictness (§9/§16). Confirmed product decision.
- **Co-write invariant is load-bearing.** Correctness of the no-resyncSums rebuild depends on `q:live` and
  `q:sum` only ever being mutated together in one script. Any future code path that touches a `q:sum` field
  directly would break it — enforce via review and keep all sum writes inside the Acquire/Release/cleanup scripts.
- **Clone must stamp a lockstring before admission.** Quota keys every entry off the lockstring; the clone path
  historically stamped only `AnnotationOwner` (`clone.go:303`). This design fixes clone to stamp one (§1/§5)
  before `Admission.Acquire` reads the local object, and the exact same value must be persisted on the CR. Any
  future owner-stamping path must do the same, or its sandboxes are untracked.
- **Subject cleanup `DEL` failure.** If the post-key-delete quota cleanup `DEL`s and their bounded retry all fail,
  the subject's Redis keys leak as never-read dead memory (no `SCAN` reclaims them). Correctness-harmless
  (deleted subjects are no longer enumerated and subject IDs are never reused), but monitor Redis memory and
  `DEL`-failure count.

## 14. Acceptance Criteria

- Concurrent creates for one limited key across replicas never exceed any limited `(dim, scope)` **while Redis
  is healthy** (`sandbox.count` / `limits.cpu` / `limits.memory` over `all`; `running` modified only by
  create/delete).
- Idempotent upsert: a retried `Acquire` with the same `(footprint, scopes)` yields zero deltas and zero writes
  (no double-charge); the `HEXISTS`/version-guard is gone.
- `running` scope semantics: a paused sandbox leaves the `running` sums but stays in `all`; a resume re-adds it
  (un-gated, may transiently exceed the `running` limit — leader converges, never drains; new creates blocked
  until it falls); a terminate/delete removes it from all sums.
- cpu/memory dimensions: the hot path derives the quota footprint from the final local sandbox resource data
  passed through the admission hook; sums maintained in integer units via `HINCRBY`; anti-drift resolves the CR
  into `infra.SandboxResource` with the shared infra resource extraction helper and then uses the same
  resource-to-dimension mapper as the hot path.
- Conservative release: an ambiguous create failure **keeps** the charge (verified by a leaked entry anti-drift
  later releases); a provably-pre-CR failure releases immediately; `DELETE` releases only after the deletion is
  accepted; all idempotent with the leader's event-driven reconcile (no double-decrement below 0 — `max(0,·)`
  floor; no over-admission while Redis healthy).
- Request-side release is bounded: failed-create cleanup release and accepted manager `DELETE` release use a
  short `quotaReleaseTimeout`; Redis release errors/timeouts are logged/counted and do not replace the create
  failure or change the delete success response.
- Bidirectional anti-drift: subjects enumerated through `quota.SubjectLister`, not by importing or depending on
  key storage; (a) an entry whose CR is gone is released
  (gated by two-consecutive-pass + cache health, no per-entry ts); (b) a live CR missing/incorrect in Redis is
  charged/corrected immediately, so a simulated entry loss converges to the exact sums (usage may legitimately
  remain `> limit` after a fail-open over-admission — anti-drift converges to *truth*, further creates rejected
  until usage falls; quota never drains). A subject-listing error skips the cycle without treating the subject set
  as empty. Every replica starts the anti-drift driver; it waits for primary, runs immediately on primary
  acquisition, cancels the active cycle on primary loss, and is safe under a flapping leader (idempotent, no
  version guard).
- Co-write invariant: a forced partial Redis loss (or async failover) leaves `q:live` and `q:sum` mutually
  consistent (no half-script), and the leader rebuild restores exact sums **without** a separate sum recompute.
- New key needs no seed: first `Acquire` on absent `q:live:{K}` / `q:sum:{K}:*` charges from zero.
- Fail-open + circuit breaker: with Redis unreachable **or absent**, limited keys are allowed (unenforced) and
  unlimited keys provably do zero Redis IO; after N consecutive Acquire failures the breaker opens and subsequent
  limited-key admission requests do **no** Redis IO for the cooldown D, then half-open probes. Maintenance paths
  bypass the acquire breaker and use bounded direct Redis calls. After Redis returns, the breaker closes and the
  leader rebuild restores exact sums and enforcement.
- Quota settable without Redis: a non-empty quota at key create is accepted and persisted even when no Redis is
  configured; unenforced; API-key create/list responses still return the stored static quota; no dynamic usage
  field is returned.
- No quota `PATCH` in Phase 1; a newly-created limited key enforces from its first `Acquire`; no replica observes
  a limited key as "unlimited" (unknown key → auth failure).
- 403 with no retry: a quota miss returns immediately with the E2B-compatible error body and a tentatively-picked
  pooled sandbox is returned to the pool.
- Validation (§3.1): `limit = 0` blocks all creates charging that pair; an all-unlimited quota normalizes to
  unlimited; negative / duplicate / non-`{sandbox.count,limits.cpu,limits.memory}`-dimension / non-`{all,running}`
  -scope are rejected at key create; short dimension keys (`count`, `cpu`, `memory`) are rejected; Redis presence
  imposes **no** validation constraint. Dimension/scope legality, JSON decoding, and `NormalizeQuotaSpec` are
  tested in `pkg/sandbox-manager/quota/spec`; E2B tests cover only API-key request/response wiring and admin
  authorization.
- Existing E2E/unit tests introduced before this decision and using short keys or the old scope-outer shape are
  updated to the canonical `{"limits":[...]}` quota spec.
- `CountActiveSandboxes` and SandboxClaim self-healing unchanged (regression).
- Primary signals gate only the anti-drift diff + event-driven reconcile; correctness holds with them forced on
  all replicas (idempotency regression test).
- Lockstring stamped on **every** create path (claim/create **and clone**) before admission reads it, and the
  admitted lockstring equals the persisted CR annotation; the anti-drift driver lists a subject's live set via
  `IndexUser` (informer, never APIReader); no owner label, no backfill.
- Layering: quota types live under the stdlib-only `pkg/sandbox-manager/quota/spec` leaf package and dynamic
  quota implementation lives under `pkg/sandbox-manager/quota`; E2B owns static API-key config and passes only
  `User` + `QuotaSpec` to sandbox-manager; E2B no longer constructs `infra.SandboxAdmission` or calls dynamic
  Redis quota APIs.
- Manager-level create options are wrapper types around existing infra options plus quota metadata, so the
  refactor does not duplicate every `ClaimSandboxOptions` / `CloneSandboxOptions` field.
- Dynamic cleanup is decoupled from key deletion: deleting a key removes static config, then best-effort
  `SandboxManager.CleanupQuota(ctx, user)` removes Redis state without rolling back key deletion on failure.
  This call is still required for limited deleted keys because there is no SCAN fallback.
- Redis topology: Phase 1 Lua touches only `{K}`-tagged keys (no `CROSSSLOT`); standalone/Sentinel verified,
  Cluster structurally compatible.
- Table-driven unit tests for `QuotaManager` (upsert add/update/delete classification, enforce vs reconcile,
  idempotency/zero-delta, fail-open, circuit-breaker open/half-open/close, quota-without-Redis), the scope/
  footprint predicates, and the anti-drift driver (both directions, immediate missing-entry charge, two-pass
  release gate, cache-health gate, subject-lister errors/cache, primary wait/cancel), plus create-path
  integration.

## 15. Resolved Decisions & Implementation Discretion

### Resolved (product / architecture)

- **Model:** `(user, dimension, scope)`. Phase 1 dimensions = `sandbox.count` / `limits.cpu` /
  `limits.memory`; Phase 1 scopes = `all` / `running`. `running` = live and `Spec.Paused == false`. Further
  dimensions/scopes are reserved and **rejected at validation** (§3.1).
- **Layering:** static API-key config is E2B-owned; quota types and quota-domain validation/normalization live in
  `pkg/sandbox-manager/quota/spec`; dynamic quota lives in `pkg/sandbox-manager/quota`. sandbox-manager receives
  only `User` + `QuotaSpec` and creates Admission internally. E2B keeps its sandbox `Modifier` business logic and
  API-key business logic but does not create `infra.SandboxAdmission` and does not call Redis quota APIs.
- **Manager options:** use wrapper options around existing infra options plus quota metadata; do not copy every
  infra option field into parallel manager structs.
- **Quota shape:** public API quota JSON uses the canonical `QuotaSpec` shape (`{"limits":[...]}`) with full
  dimension keys (`sandbox.count`, `limits.cpu`, `limits.memory`). No short-key mapping and no separate public
  quota model.
- **Storage:** Redis holds the **live set** (`q:live:{K}` HASH: lockstring → footprint without count + conditional
  scopes without `all`) plus **per-dimension sum hashes** (`q:sum:{K}:<dim>`: scope → integer, scheme A).
  `count` and `all` are **structural/implicit** (count = const 1 per entry; `all` ⟺ entry exists), never stored
  per entry, always present in the sums. Integer units + `HINCRBY` (no floats); sum fields kept at 0, never
  deleted; **no per-entry `ts`**.
- **Single mutation primitive:** an idempotent **upsert `Acquire`** returning only `error` (read old, diff
  old→new, apply per-`(dim, scope)` deltas; skip zero deltas; skip unchanged `HSET`) with an `enforce` flag
  (hot-path create rejects on a positive-delta breach; leader reconcile never rejects), and a `Release` (subtract
  old, remove entry). **No reservation handle**, **no version guard** (idempotency is intrinsic to the diff),
  **no `HEXISTS` short-circuit**, and **no separate `resyncSums`** (the co-write invariant makes the rebuild
  self-consistent).
- **Co-write invariant:** `q:live:{K}` and `q:sum:{K}:<dim>` are always mutated in the **same** atomic Lua, all
  `{K}`-hash-tagged into one slot — so they never diverge and are lost together. Load-bearing for the
  no-resyncSums rebuild (§7).
- **Identity:** the existing **lockstring** (`AnnotationLock`), stamped on **every** owner-stamping path before
  admission reads it — claim/create already do; **clone** is fixed to stamp one (§1/§5). No new annotation.
- **Footprint:** resolved deterministically; hot path from the final local sandbox resource data passed through
  admission, anti-drift recomputes from the CR (**no snapshot annotation**). Both paths use the same infra
  resource extraction helper and one shared `infra.SandboxResource -> quota footprint` mapper in
  `pkg/sandbox-manager/quota`.
- **Shared live predicate:** quota-live membership lives in the leaf sandbox lifecycle package. It excludes
  deleting/terminating sandboxes, `SandboxReusing`, and reuse-triggered sandboxes. Cache imports that leaf package
  directly; quota wraps it as `IsLiveForQuota`; cache does not import quota.
- **Removal:** conservative request-side (failed-create keeps an ambiguous charge; manager `DELETE` releases only
  after deletion is accepted); **leader event reconcile** for pause/resume/footprint/non-manager-delete; **leader
  bidirectional anti-drift** backstop. Subjects come from a Redis-independent `SubjectLister` supplied by E2B, not
  from key storage imports. All Sandbox reads are **informer-only, never APIReader**; **every release-capable pass
  is cache-health-gated**; the full-diff leaked release uses **two-consecutive-pass** leader memory in place of a
  per-entry `ts`. Missing live entries are charged immediately with `enforce=false`. **No owner label, no
  backfill.**
- **`running` admission:** gated **only at create** (un-gated resume → bounded over-limit tolerated, §6.4.1);
  out-of-band footprint changes reconciled by the leader, not admission-gated.
- **No seed:** absent state for a new key == zero.
- **Redis unavailable / absent (Phase 1): fail-open**, with an **Acquire-only circuit breaker** (default N=3
  Acquire failures → open D=30s, then half-open) so a sustained outage costs no admission-path Redis IO.
  Maintenance paths bypass the acquire breaker and use bounded direct Redis calls. Fail-closed posture is
  **deferred** (§16).
- **Quota immutable in Phase 1** (no `PATCH`); admin-only to set; E2B maps each API key to a sandbox-manager
  subject/user and passes that static quota to manager calls. **Mutation and
  `unlimited → limited` promotion are planned future work** (§16), each with a safe-activation scheme — *not*
  permanently forbidden.
- **Reconciler placement:** primary-aware **driver in `pkg/sandbox-manager/quota`** (event handler registered into
  `pkg/cache`); every replica starts it, it waits for primary, runs immediately when elected, and cancels the
  active cycle on primary loss. Generic primary lease/signals; correctness via idempotency + co-write invariant,
  not leadership.
- **Cleanup:** API-key deletion and dynamic quota cleanup are decoupled. E2B deletes static key config, then calls
  `SandboxManager.CleanupQuota(ctx, user)` best-effort for Redis dynamic state.
- **Error code:** quota exceeded **403** (no retry), E2B-compatible error body.
- **Topology:** Phase 1 has only `{K}`-tagged keys → standalone/Sentinel/Cluster structurally compatible.

### Left to the implementing agent

- Redis key prefix, hash-tag form (`{K}`), the entry JSON encoding — which must be **canonical** (stable
  key/element order) or compared by decoded value, and must encode an empty `s` as an array (not `{}`), so an
  idempotent re-entry truly writes nothing (§6.3) — and the Lua script encoding (cjson decode/encode, the add/update/delete
  classification, the `delta > 0` enforce check).
- Redis client choice and config wiring (`pkg/sandbox-manager/config` / `clients`, `cmd/sandbox-manager`):
  pooling, retry/back-off, acquire timeout, the request-side `quotaReleaseTimeout`, the fail-open error
  classification, and the circuit-breaker `N` / `D` defaults and tunables.
- The generic leadership lease name, `leaderelection` parameters, and exact primary signal API (`IsPrimary` plus
  `WaitPrimary` / `PrimaryChanged` or equivalent) on `SandboxManager`.
- Anti-drift cadence values, minimum interval on primary reacquire, budget/pagination/jitter for the immediate
  primary diff, the two-consecutive-pass leaked-release memory, `SubjectLister` adapter wiring from E2B key
  storage, and how missed-event divergence / breaker state are monitored (metrics).
- The integer units for cpu (millicores) and memory (MiB), implemented through the shared infra resource
  extraction helper plus the shared `FootprintFromResource` mapper used by both hot path and anti-drift.
- API-key request/response wiring for `quota` using `quotaspec.QuotaSpec` directly, plus updating the branch's
  existing E2E/unit tests that still use short keys or the old scope-outer shape. Normalization and quota-domain
  validation stay in `pkg/sandbox-manager/quota/spec`.
- The exact rule for classifying a create failure as **provably pre-CR**, **cleanup-deleted**, or **ambiguous**,
  and the mechanical clone lockstring call site (the `Modifier` at `create.go:200` vs
  `newSandboxFromTemplate`), provided it happens before `Admission.Acquire` reads the object and persists the
  same value on the CR.
- Where exactly `Acquire`/`Release` hook into `TryClaimSandbox` / clone / `DELETE`, and how a tentatively-picked
  pooled sandbox is returned to the pool on a 403.
- The `ListLiveSandboxesByOwner` signature in `pkg/cache` (over `IndexUser`) and the concrete
  `SandboxInformerHealthy()` API (initial-list-complete, relist start/success, watch error, last successful sync)
  — **not** plain `HasSynced`.
- The exact `InRunningScope` boundary for transition phases (`Resuming`, pausing-in-progress) — the fixed
  semantics is "live and not paused".

## 16. Deferred / Future Work

- **Further dimensions / scopes** (e.g. per-`template:<name>` scope, gpu, ephemeral-storage): the storage layer
  already treats dimensions/scopes generically and maintains sums for whatever a key declares, so adding one is a
  validation + footprint/scope-derivation change with **no data migration**. Reserved and rejected at validation
  until shipped.
- **Fail-closed posture** (`onRedisUnavailable: reject`): `q:warm` total-loss detector, Redis-clock settle
  window, `COLD → 503`, and synchronous replication (`WAIT` / sync topology) for strictness across failover.
  Reintroduces global Redis keys (reopens the Cluster `CROSSSLOT` question).
- **Quota mutation** — both changing a *limited* key's limits **and promoting `unlimited → limited`** — with a
  safe-activation scheme (activation window or all-replica `QuotaSpec` cache invalidation that drains stale
  `unlimited` admissions before enforcement begins). Explicitly **planned**, not forbidden (§6.7).
- **Strict `running` (and other lifecycle-scope) enforcement:** gate pause/resume (and any →scope transition)
  through admission instead of the create-only + leader-reconcile model, removing the bounded resume over-limit
  (§6.4.1).
- **Post-create 变配 admission** (if the manager ever exposes resize): an admission gate against quota plus the
  already-present leader footprint reconciliation.
- **Apiserver-side fencing** (a validating webhook rejecting a Sandbox create whose lockstring has no accepted
  quota admission state) for absolute 0-oversell under failure intersections — out of scope by product decision.
- **Optional belt-and-suspenders sum recompute** in the infrequent diff (recompute each `(dim, scope)` from
  current live entries and atomically correct), defending against corruption outside Redis's atomic-replication
  guarantee — not required for correctness (§6.4.2).
- **Official Redis Cluster support** (testing + the deferred-posture hash-tag redesign).
