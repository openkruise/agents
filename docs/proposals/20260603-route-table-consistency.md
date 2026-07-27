---
title: Route Table Consistency for sandbox-manager and sandbox-gateway
authors:
- "@PRAteek-singHWY"
creation-date: 2026-06-03
last-updated: 2026-06-03
status: implementable
---

# Route Table Consistency for sandbox-manager and sandbox-gateway

| Metadata    | Details            |
|-------------|--------------------|
| **Author**  | @PRAteek-singHWY   |
| **Status**  | Implementable      |
| **Created** | 2026-06-03         |

## Summary

Both the sandbox-manager (`pkg/proxy`) and the sandbox-gateway
(`pkg/sandbox-gateway/registry`) keep an in-memory route table mapping a sandbox
ID (`namespace--name`) to its pod IP and state. Entries reach a replica through
two unordered channels: peer `POST /refresh` pushes (memberlist) and, on the
gateway, a Sandbox CR-watch controller reconciling from an informer cache. This
note documents the consistency model those tables must uphold and the design
that enforces it.

## Motivation

Three defects existed in the consumer side of route synchronization:

1. **Divergent delete semantics.** For an identical `{state: "paused"}` push the
   manager handler kept the entry (delete only on `dead`) while the gateway
   handler dropped it (keep only `running`), so the two data planes held
   different state for the same event.
2. **Version-blind deletes.** `Update` refused to let an older `resourceVersion`
   overwrite a newer one, but `Delete` erased the entry — and the recorded
   version — unconditionally. The next write, however stale, then landed as a
   fresh first write. With two unordered writers, a delete followed by a
   lagging-cache update could *resurrect* a removed route, and the Envoy filter
   would route live traffic to a freed pod IP until the informer converged.
3. **Inconsistent keying.** The manager's periodic refresh looked routes up by
   name while storing them under `namespace--name`, making its "skip if
   unchanged" comparison meaningless.

## Design

### Invariant

A route table entry is only mutated by an event whose `resourceVersion` is at
least as new as the version currently recorded for that slot — **including
deletes**. Writers never need to coordinate with each other; the store is the
single arbiter, and writes from any source are idempotent and commutative under
resourceVersion ordering.

### Shared store (`pkg/proxy/routestore`)

Both tables delegate to one implementation:

- `Set(id, route)` keeps the existing CAS + `IsResourceVersionNewer` (`>=`)
  semantics for live entries.
- `Delete(id, resourceVersion)` replaces the entry with a short-lived
  **tombstone** stamped with the larger of the deleting event's resourceVersion
  and the one recorded on the entry (an empty value — e.g. an informer
  not-found — falls back to the recorded one).
- A write may replace an active tombstone only if it is **strictly newer**
  (`IsResourceVersionReallyNewer`, `>`). An equal version is the very write the
  deletion superseded; accepting it would resurrect the route. A genuine
  recreate always carries a strictly higher version (etcd resourceVersions are
  monotonic), so it passes.
- Tombstones expire after `DefaultTombstoneTTL` (10 minutes — by then the
  level-triggered reconcilers have converged) and are reclaimed lazily on
  access, by an inline sweep once a threshold accumulates, and by an explicit
  `GC()` hooked into the manager's periodic route reconciliation. No new
  background goroutine is introduced.

### One keep-vs-delete rule

`proxyutils.ShouldDeleteRoute(state)` (`delete iff state == dead`) is the single
predicate used by both `/refresh` handlers. Every non-dead state — creating,
available, paused, running — is stored with its state; consumers decide
routability from `Route.State` (the gateway filter already serves only
`running`). An identical push therefore produces an identical table on both
data planes.

### Writers

- Manager `/refresh`, gateway `/refresh`: shared predicate + versioned delete.
- Gateway CR-watch controller: updates with the object's resourceVersion;
  deletes pass the observed resourceVersion (deletion-timestamped object) or
  empty (not-found), falling back to the recorded one.
- Manager periodic reconciler: orphan deletes pass the route's own
  resourceVersion and trigger tombstone GC; the refresh path now keys lookups
  by the full sandbox ID.

## Alternatives

- **Single writer (controller-only or push-only).** Rejected: the push path is
  the low-latency channel for lifecycle changes, the watch path is the
  level-triggered backstop; both are wanted. Making the store order-safe keeps
  both without coordination.
- **Reference counting / per-id locks.** Heavier than needed; the
  resourceVersion already provides a total order per object.
- **Unbounded tombstones.** Correct but leaks memory under churn; TTL + lazy
  sweep bounds it without a goroutine.

## Test Plan

- Table-driven unit tests on the store: version arbitration, delete-then-stale
  -write (resurrection guard), equal-version-vs-tombstone, empty-version
  fallback, expiry, inline GC, concurrent writers under `-race`.
- Handler tests: both `/refresh` handlers agree on the shared predicate; a
  paused push no longer drops the gateway entry; a stale running push after a
  dead push cannot resurrect.
- Controller tests: deletes are versioned at the controller boundary.
