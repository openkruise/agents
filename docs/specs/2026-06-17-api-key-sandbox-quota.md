# API Key Sandbox Quota — Design Spec

- Date: 2026-06-17
- Branch: `feature/e2b-api-quota-260617`
- Scope: `pkg/servers/e2b/` (create, api_key, models, keys, routes), `pkg/servers/e2b/quota/` (new), `pkg/sandbox-manager/` (api, infra wiring), `pkg/cache/`, `pkg/peers/`, `pkg/proxy/` (peer transport reuse), `cmd/sandbox-manager/`, Helm chart / `config/sandbox-manager/`

## 1. Background

sandbox-manager is a stateless, multi-replica backend exposing E2B/MCP APIs to manage sandboxes. Each request authenticates via `X-API-KEY`, resolved to a `*models.CreatedTeamAPIKey` (with `ID uuid.UUID` and `Team`). That `ID` is the sandbox **owner**: every create path stamps it onto the Sandbox CR as `agentsv1alpha1.AnnotationOwner` (claim and create-on-no-stock via `utils.LockSandbox`, clone via direct set), and it surfaces as `route.Owner`.

We need to cap how many sandboxes a single API key may hold, enforced **without overselling** even under highly concurrent creates across replicas, and **without materially slowing down create**. Peak create throughput is ~2500/sec (150k/min) aggregate. The worst case for a single limited key is "small limit + high churn" (a key that constantly creates and deletes against a tiny limit), which rules out any per-operation external IO on the hot path.

Phase 1 exposes only a per-key **sandbox count** limit, but the internal model must extend to future dimensions (CPU, memory) and scopes (per-team, per-template, api-key+template).

### Key facts established from the codebase

- `pkg/cache` already maintains a `user` field index (`IndexUser`) over `AnnotationOwner` on Sandbox/Checkpoint, plus `ListSandboxes({User})` and `CountActiveSandboxes({User})`.
- `CountActiveSandboxes` **excludes** `Dead` (`cache.go:306`). Its only production caller is the SandboxClaim controller's `countClaimedSandboxes` (`common_control.go:408`), which relies on Dead being excluded so a dead sandbox triggers a replacement. **It must not be modified.**
- Peer-to-peer transport already exists: `pkg/proxy` runs an HTTP server on `SystemPort` (routes registered via `web.RegisterRoute`) and ships a peer HTTP client (`requestPeerClient`). `pkg/peers.Peers` exposes `GetAllMembers() []Peer{IP,Name}` and `LocalAddr()`.
- Create paths: `CreateSandbox` → `createSandboxWithClaim` / `createSandboxWithClone`, both passing `User: user.ID.String()` and accepting a `Modifier func(infra.Sandbox)` that runs before the CR is written.
- Key storage: `keys.KeyStorage` with Secret backend (`e2b-key-store` Secret, informer-synced in-memory indexes, `retryUpdateSecret` CAS writes) and MySQL backend (GORM `teams`/`team_api_keys`, HMAC-only, TTL caches, `DisableAutoMigrate`).

## 2. Goals & Non-Goals

### Goals

- Enforce a per-API-key maximum on the number of sandboxes it holds, strongly consistent across replicas (no oversell).
- Internal model distinguishes static **QuotaSpec** (limits) from dynamic **usage** (committed) and **reservation** (in-flight). Phase 1 exposes only `sandbox.count`; dimension/scope are extension points.
- Hot path performs **at most one** in-memory acquire for limited keys and **zero** extra work for unlimited/default keys. No full sandbox list, no per-op external IO, no high-frequency rewrite of `e2b-key-store`.
- Static QuotaSpec is stored with API-key metadata; dynamic usage/reservation is **never** written back to the key store.
- During replica membership change (scale up/down, crash + replacement, rolling upgrade), the system **prefers under-sell (temporary retryable rejection) over over-sell**.
- Backward compatible: old keys without a quota field default to **unlimited**.

### Non-Goals

- Implementing CPU / memory / disk dimensions or per-team / per-template enforcement. Only model extension points are added.
- Reclaiming or evicting existing sandboxes when a quota is lowered. Lowering a limit only blocks new creates.
- Strict no-oversell **during the brief membership-convergence window** without any availability cost. We deliberately accept a short fail-closed window instead of introducing a CP lease/consensus subsystem (see §11).
- Billing / usage reporting. This is hard-limit enforcement only.
- Cross-cluster / multi-region quota.

## 3. Quota Data Model

Three layers, all addressed by `(apiKeyID, dimension, scope)`. Phase 1 only populates `dimension = sandbox.count`, `scope = {}`.

```go
// Static — stored with key metadata.
type QuotaDimension string
const DimSandboxCount QuotaDimension = "sandbox.count" // future: cpu.millicores, memory.mb, ...

// Scope narrows where a limit applies. Phase 1: empty == per-api-key.
type QuotaScope struct {
    Template string `json:"template,omitempty"` // future extension point
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

External JSON on the key request/response is **nested** (not a flat `maxSandboxes`), e.g. `"quota": { "sandbox": { "count": 50 } }`; absent / `null` == unlimited. The handler normalizes it to `QuotaSpec.Limits`. The exact external shape is an implementation detail provided it stays nested and extensible.

Dynamic state is **in-memory only** (see §6):

```go
type quotaCell struct {
    spec     QuotaSpec
    counters map[scopeDimKey]*counter
    warm     bool // becomes true after a base rebuild + settle
}
type counter struct {
    base         int64                   // committed: count of owner CRs (from cluster), incl Dead
    reservations map[string]*reservation // opID -> in-flight not-yet-visible-in-base
}
type reservation struct {
    opID      string
    sandboxID string    // bound after the CR is created
    expiresAt time.Time // TTL backstop
}
// used = base + len(open reservations); amount per reservation is 1 for count, generalizes for cpu.
```

## 4. Static Config Storage

Both backends store **only** the static `QuotaSpec`, alongside the key. These are low-frequency writes (create key / admin PATCH).

### 4.1 Secret backend

- Add `Quota *QuotaSpec json:"quota,omitempty"` to `models.CreatedTeamAPIKey`; it serializes into the per-key JSON inside `e2b-key-store`.
- Old payloads decode to `nil` == unlimited (back-compat).
- Writes reuse the existing `retryUpdateSecret` CAS path. **No usage/reservation is ever written to the Secret.**

### 4.2 MySQL backend

- Add a nullable `quota JSON` column to `team_api_keys` (`NULL` == unlimited). `AutoMigrate` adds the column; gated by `DisableAutoMigrate`, with a documented manual DDL alternative.
- Old rows (`NULL`) == unlimited (back-compat).
- **No usage/counter/reservation tables are required** (see §5). An optional `quota_reservation_snapshot` table may be added later purely to speed warm-up / for audit; it is not required for correctness and is out of Phase-1 scope.

## 5. Usage Source: the Cluster Itself

Because the chosen counting rule is "a sandbox occupies a slot until it is truly deleted," committed usage is **exactly** the number of non-deleted Sandbox CRs carrying `AnnotationOwner = keyID`. This has two consequences:

1. **Committed usage is auto-persisted by the CRs themselves.** Every create produces a CR with the owner annotation; that IS the durable record. No separate usage store is needed, and **delete needs no quota-release operation** — the count drops naturally when the CR is garbage-collected and observed by the informer.
2. **The base count is reconstructible by any replica** from the cluster (`CountSandboxesByOwner` below or the informer `user` index), which is what makes the in-memory authority resilient to replica churn (§8).

### Counting primitive

`CountActiveSandboxes` excludes `Dead` and must not change (§1). Add a small additive method that counts **all** existing owner CRs including Dead:

```go
// CountSandboxesByOwner counts every existing Sandbox CR owned by User,
// regardless of state (including Dead-but-not-yet-GC), matching the
// "occupies a slot until truly deleted" quota rule.
func (c *Cache) CountSandboxesByOwner(ctx, opts ListSandboxesOptions) (int32, error)
```

(~5 lines, mirrors `CountActiveSandboxes` without the Dead filter. Alternatively `len(ListSandboxes({User}))`; a dedicated count method avoids allocating full objects on rebuild.)

### The correctness boundary (why base alone is insufficient at admission time)

The informer is per-replica and eventually consistent, and there is a gap between admitting a create and the new CR becoming visible. Counting purely from the cluster **at the instant of admission** oversells: two concurrent creates both read `N` and both admit. Therefore admission uses

```
used(K) = clusterBase(K) + pendingReservations(K)
```

where the reservation overlay bridges "admitted but CR not yet in base," and a **single writer per key** keeps that overlay consistent. The overlay is the only volatile, non-reconstructible state — and it is small and short-lived.

## 6. In-Memory Authoritative Counting & Single-Writer Control

All quota enforcement lives in a new `QuotaManager` (package `pkg/servers/e2b/quota`), holding one `quotaCell` per key. The external store is demoted to "static spec + crash-rebuild source." There is **no per-op external IO**.

### 6.1 Ownership ring (single writer per key)

- `owner(K) = consistentHash(keyID)` over `peers.GetAllMembers()` (stable sort by `Name`). Consistent hashing keeps key movement to ~1/N on membership change.
- If `owner.IP == LocalAddr()` → handle locally. Otherwise forward to `owner.IP:SystemPort` using the existing `requestPeerClient`.
- Front-end routing is **not** changed; same-key creates may land on any replica and are forwarded to the owner.

### 6.2 Acquire / Commit / Release

```go
type QuotaManager interface {
    // Acquire returns a Reservation or ErrQuotaExceeded. Routes to the owner replica
    // (local or forwarded). Fail-closed on uncertainty (see 6.4).
    Acquire(ctx context.Context, req AcquireRequest) (Reservation, error)
    // Release returns an in-flight reservation (create failed / cancelled). Idempotent.
    Release(ctx context.Context, req ReleaseRequest) error
    // Reconcile expires stale reservations and realigns base from the cluster.
    Reconcile(ctx context.Context, scope ReconcileScope) error
    // Describe reports current usage/limit for read APIs.
    Describe(ctx context.Context, apiKeyID string) (QuotaStatus, error)
}
```

`AcquireRequest` carries `apiKeyID, namespace, dims+amounts, QuotaSpec` (the spec is already loaded at auth time, so the owner need not re-read the store on the hot path).

**Commit is implicit.** When the owner's informer observes a CR carrying the reservation's `opID` annotation (owner = K), the reservation is closed and the CR is now counted in `base`. **Release is mostly implicit too**: deletion of any owner CR (by any replica) lowers `base` via the informer. An explicit `Release` RPC is needed only when a create **fails and never produced a CR**; the reservation TTL is the backstop if that RPC is lost.

### 6.3 Create hot path (limited key)

1. `CheckApiKey` already put `user` (with `QuotaSpec`) in context.
2. Unlimited key → no-op (`Acquire` returns a sentinel reservation; `Release` is a no-op). The ring is never consulted; zero cost.
3. Limited key → `owner = ring.Lookup(K)`; local in-memory acquire, or one forwarded `Acquire` RPC. On the owner: if `used+1 <= limit`, record a pending reservation `opID`; else reject with `429`.
4. The `opID` is stamped onto the CR via the existing `Modifier` closure (`basicSandboxCreateModifier`), e.g. annotation `agents.kruise.io/quota-op-id`. **No infra interface change.**
5. Call `ClaimSandbox` / `CloneSandbox`. On failure, issue `Release(opID)` using `context.WithoutCancel(ctx)` so client cancellation/timeout cannot skip it. On success, do nothing — the CR-with-opID will close the reservation via the informer.

Acquire covers both claim and clone.

### 6.4 Strong consistency without a lease: fail-closed on uncertainty

The no-oversell guarantee comes from **rejecting whenever ownership is uncertain**, which is under-sell by construction:

- **Steady state:** all replicas derive the same ring from the same membership → same owner for K → single writer → no oversell.
- **Membership change:** memberlist delivers the event to all live replicas; each recomputes the ring. For shards whose owner changed, the new owner's cell is cold **and** every replica enters a **settle window** for the affected shards. During the window the owner candidate rejects (fail-closed). Even if two replicas transiently disagree during convergence, both are in settle → both reject → no oversell. After the window (`> convergence + rebuild`), views agree → single owner.
- **Owner unreachable** (crashed but ring not yet updated): the forward fails → reject (under-sell).
- **Cold cell rebuild:** seed `base` via `CountSandboxesByOwner` using `GetAPIReader()` (a quorum read that bypasses the lagging local informer) plus a settle window; reject for that key until warm.

This trades a brief, per-affected-key availability dip for "never oversell," matching the chosen preference.

### 6.5 Reconcile (self-healing)

A periodic + informer-event-driven loop on the owner: set `base` from the cluster, drop reservations whose CR became visible, expire reservations past TTL. Drift only converges toward the cluster truth.

## 7. Performance

- **Unlimited / default keys:** one in-memory check; ring not consulted; zero external IO. This is the majority of the 2500/sec.
- **Limited keys:** hot path is one ring lookup (O(1) in-memory) plus one cell mutation, either local or a single intra-cluster HTTP `Acquire`. Never a per-op DB/apiserver write.
- A limited key's per-key op rate (the only contention point) is bounded by that tenant's own create+delete churn, not the global 2500/sec.
- Rebuild/reconcile run off the hot path (cold start, membership change) and use the indexed `user` count, not a full scan.
- Optional optimization: when `used` is far below `limit`, the owner may serve from slack without strict serialization; strict single-writer only matters near the limit.

## 8. Failure Recovery & Replica Lifecycle

Invariant: **bias to over-count during uncertainty; never under-count.** Over-count → temporary conservative rejection (self-heals); under-count → oversell (forbidden).

| Scenario | Base | Reservation overlay | Oversell? |
|---|---|---|---|
| Scale up (new replica) | new owner lazily rebuilds from cluster | old owner alive → graceful handoff of overlay | No |
| Scale down (SIGTERM) | same | departing replica hands off owned overlays before exit | No |
| Rolling upgrade | same | graceful handoff each step; consistent hashing bounds movement to ~1/N | No |
| Crash (no SIGTERM) | new owner rebuilds from cluster (quorum read) | lost; in-flight creates failed client-side, produced no CR | No (fail-closed during detection + settle) |

- A crashed owner's lost reservations do not oversell: its in-flight creates broke client-side and produced no CR, while any CR it did persist is visible to a quorum read and counted in `base`.
- During memberlist failure detection + convergence, affected keys are fail-closed (creates return retryable `429`/`503` and clients retry) — under-sell, never oversell.
- Graceful handoff (planned changes) avoids even the settle stall by transferring the overlay; crash falls back to rebuild + settle.

Defaults (tunable): reservation TTL ~30–60s; settle window must exceed worst-case memberlist convergence + base rebuild (a few seconds); rebuild window is fail-closed.

## 9. API Surface

- **Create** (`POST /sandboxes`): unchanged request shape; quota enforced internally. Quota exceeded → **HTTP 429** with the E2B-compatible error body.
- **Key create** (`POST /api-keys`): optional nested `quota`. Setting/raising quota is **admin-only** (admin-team key); non-admin callers may not set or raise quota. Admin keys **may** be explicitly limited (default unlimited).
- **Quota update** (new, admin-only): `PATCH /api-keys/{id}/quota` to set/change a key's `QuotaSpec`. Dynamic usage is never settable — only reconciled.
- **Describe** (optional, read): expose current usage/limit for a key (admin or owner-team), backed by `QuotaManager.Describe`.
- **Key delete** (`DELETE /api-keys/{id}`): minimal-change behavior — the key's quota config is removed with the key; **existing sandboxes are kept** (no cascade delete) and run out their own lifecycle. Their CRs simply no longer back any live key.

Authorization reuses the existing `CheckCreateAPIKeyPermission` admin/team gating; the quota PATCH chains `CheckApiKey` + an admin check.

## 10. Compatibility

- Old keys without a `quota` field → unlimited. New JSON field is `omitempty`; old/new payloads interoperate.
- MySQL column add is gated by `DisableAutoMigrate` with a manual DDL fallback; no behavior change for existing rows.
- `CountActiveSandboxes` is untouched; SandboxClaim self-healing is preserved. Quota uses the additive `CountSandboxesByOwner`.
- No change to E2B sandbox lifecycle semantics (pause/resume/timeout/delete) beyond the create-time admission and the implicit release on CR deletion.

## 11. Alternatives Considered

| Option | Mechanism | Hot-path external IO | Small-limit high churn | No oversell | Verdict |
|---|---|---|---|---|---|
| **In-memory owner ring + forwarding (chosen)** | consistent hash over memberlist + fail-closed-on-uncertainty | none | OK | Yes (under-sell during convergence) | Recommended; fits "internal only + minimal code + prefer under-sell" |
| Per-shard Lease (CP) fencing | etcd-backed Lease per shard | none (renew off hot path) | OK | Yes, even during convergence | Deferred — adds a consensus/lease subsystem; not needed given under-sell preference |
| Local lease pool | each replica leases blocks | only at block boundaries | Fails (degrades to per-op) | Yes | Rejected for worst case |
| Synchronous DB counter | row-lock conditional UPDATE per op | every op | single hot row serializes | Yes | Rejected at this throughput; MySQL-only |
| K8s ConfigMap/Secret CAS | resourceVersion per op | every op | apiserver cannot sustain | Yes | Rejected (infeasible at scale) |
| Informer-only counting | derive from cache | none | — | No (cross-replica lag) | Rejected as enforcement; used only for rebuild |
| Gateway consistent-hash affinity (A1) | envoy hash on `X-API-KEY` | none | OK | Yes | Out of scope — front-end cannot be changed |

Strict mutual exclusion across a stateless multi-replica system requires a CP primitive (lease/consensus). Since the product preference is "prefer under-sell over oversell," we achieve safety via fail-closed-on-uncertainty instead, avoiding that subsystem. The per-shard Lease remains a clean future hardening if strict no-oversell **during** convergence is ever required.

## 12. Risks

- **Settle window misconfiguration**: if the settle window is shorter than actual memberlist convergence, a brief split-brain could oversell. Mitigated by a conservative default and reconcile; documented as a tuning constraint. The Lease option (§11) removes this risk if needed.
- **Hot single key concentrates on one owner replica**: in-memory cost is negligible; many keys balance across replicas by hash. Forwarding load for one hot limited key is bounded by its own churn.
- **Forwarding adds latency** for remote limited-key acquires (one intra-cluster hop). Unlimited keys (the majority) never forward.
- **Lingering Dead CRs hold quota** under the "count until deleted" rule (e.g., stuck finalizers). This is the conservative, no-oversell direction and was chosen deliberately.
- **New peer RPC surface** (acquire/release handlers) reuses the existing `SystemPort` HTTP server and client — modest, but must honor the existing auth/transport conventions.

## 13. Acceptance Criteria

- Concurrent creates for one limited key (single replica, and multi-replica with a simulated owner ring) never exceed `limit`; no oversell under small-limit high-churn load.
- Membership change triggering ownership transfer: the new owner is fail-closed during rebuild/settle; after warm, `used` equals `CountSandboxesByOwner(owner)`.
- Quota store unavailable (static spec load fails) or owner unreachable → limited key fail-closed; unlimited fast path unaffected and provably does zero external IO.
- Release is exercised on create failure, request cancel, reservation expiry, and (implicitly) delete; reconcile corrects injected drift.
- Old keys without `quota` behave as unlimited; both Secret and MySQL backends store and load `QuotaSpec` correctly; MySQL migration respects `DisableAutoMigrate`.
- Quota exceeded returns HTTP 429 with the E2B-compatible error body.
- Admin-only quota set/raise enforced; non-admin cannot raise own quota; admin key may be explicitly limited.
- `CountActiveSandboxes` and SandboxClaim self-healing behavior are unchanged (regression test).
- Table-driven unit tests for QuotaManager (acquire/commit/release/reconcile, fail-closed paths) and for the create-path integration.

## 14. Resolved Decisions & Implementation Discretion

### Resolved (product)

- Counting rule: occupies a slot **until truly deleted** (includes Dead-not-GC); use `CountSandboxesByOwner`.
- Quota mutability: settable at key create **and** via an admin `PATCH` endpoint.
- Authorization: **admin-only** to set/raise quota; tenants cannot raise their own.
- Store unavailable: limited keys **fail-closed**.
- Crash → recovery window: **prefer under-sell**, never oversell (no Lease; fail-closed-on-uncertainty).
- Error code: **429**.
- Delete key with existing sandboxes: **keep** sandboxes, drop only the quota config (minimal change).
- Admin key may be **explicitly limited** (default unlimited).
- Reservation TTL / settle defaults agreed (tunable; TTL ~30–60s, settle covers convergence, rebuild fail-closed).

### Left to the implementing agent

- Exact annotation key for `opID`, peer RPC route paths and payload encoding, consistent-hash implementation (reuse a vendored ring lib if present), cell-map/locking details, metric names.
- External nested JSON shape for `quota` (must stay nested/extensible).
- Whether base is maintained incrementally via an informer handler (`AddReconcileHandlers`) or recomputed on rebuild; settle/TTL exact values; back-off/retry tuning for forwarded acquires.
