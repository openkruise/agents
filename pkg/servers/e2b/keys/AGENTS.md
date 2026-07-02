# `pkg/servers/e2b/keys` Guide

This directory owns E2B API-key persistence behind the `KeyStorage` interface. Keep behavior aligned across backends unless a caller-facing change is explicitly intended.

## Current Backends

- `secretKeyStorage` is the compatibility baseline for API behavior.
- `mysqlKeyStorage` is the database backend. Keep the concrete type unexported; expose it only through `KeyStorage` and `NewKeyStorage`.

## Secret Backend Refresh Semantics

`secretKeyStorage` keeps its in-memory indexes in sync with the backing
`e2b-key-store` Secret via an informer event handler installed on the shared
`pkg/cache.Cache`. There is no periodic poller. Cross-replica propagation
delay for key create/delete is bounded by informer push latency (typically
sub-second).

- `Config.Cache` is required for secret mode and is validated in
  `NewKeyStorage`. `NewSecretKeyStorage` itself does not validate, allowing
  unit tests to construct the storage without a cache as long as they do not
  call `Run()`.
- `Run()` registers the handler and starts a single-worker goroutine that
  drains a coalescing channel and calls `refresh()` against the cached
  client. `Stop()` removes the handler and waits for the worker to exit.
- Cross-replica visibility (including the historical `retryCreateKey` case
  where a retry re-reads a Secret revision containing concurrent writes
  from another replica) is healed by the next informer event reaching the
  affected replica — typically within a few hundred milliseconds.

## Interface And Callers

- Keep `KeyStorage` signatures in sync with current callers in `pkg/servers/e2b/api_key.go` and `pkg/servers/e2b/routes.go`.
- `Run()` and `Stop()` are paired lifecycle hooks. If you add background work, make shutdown idempotent and wire controller shutdown through `Stop()`.
- `LoadByKey`, `LoadByID`, `DeleteKey`, and `ListByOwner` are request-path methods; preserve context propagation for DB work.

## Team Identity

- API-key team identity is the unique team name. The team name maps directly to a Kubernetes namespace, and Kubernetes
  namespace uniqueness is sufficient for the required isolation boundary.
- Team UUIDs are display-only compatibility metadata, like creator metadata. MySQL must continue writing `teams.uid`
  for upgrades from schemas where the column is `NOT NULL`, and Secret storage may preserve existing team IDs.
- Do not use team UUIDs for API-key team authorization, storage lookup, namespace selection, or `/teams` filtering.

## MySQL Storage Rules

- Never store plaintext API keys in MySQL. Persist only deterministic `HMAC-SHA256(pepper, rawKey)` as `key_hash`.
- Pepper comes from `E2B_KEY_HASH_PEPPER` via `keys.Config.Pepper`. Pepper is required when using MySQL mode; the application will fail to start if it is empty. Do not silently switch hash algorithms.
- `CreateKey` may return plaintext once in the response model. Entries reconstructed from DB do not have plaintext `Key`.
- `LoadByKey` and `LoadByID` must populate both TTL caches on DB hit.
- Keep cache invalidation conservative: if `DeleteKey` cannot safely determine the cached `key_hash` because of an unexpected DB error, fail the delete rather than risking stale `byKey` authentication.
- `DeleteKey` uses hard-delete semantics for MySQL API-key rows. When deleting the last key for a non-admin team, hard-delete that team row too. Never delete the well-known admin key, and never delete the AdminTeam row as part of key cleanup.

## Current Temporary Business Semantics

- MySQL mode currently creates all keys under the single `AdminTeam` row (`550e8400-e29b-41d4-a716-446655449999`).
- `ListByOwner` is intentionally team-based for MySQL. In the current phase this effectively means returning the team's full key list, not recreating the old secret-storage `CreatedBy` filtering semantics.
- Do not "fix" `ListByOwner` back to secret semantics unless the broader team model or API contract changes.

## Data Safety

- Avoid `uuid.MustParse` on DB-loaded values. Use `uuid.Parse` and return/log errors instead of panicking on bad persisted data.
- Be explicit about GORM delete behavior. If you rely on hard delete semantics, do not accidentally change behavior via soft-delete fields or default `Delete`.
- When changing entities, also review `DeleteAPIKey` authorization expectations around `CreatedBy`.
- Invalid stored quota payloads on request authentication/load paths are intentionally treated as unlimited for
  compatibility and availability. `ListLimited` remains strict and skips invalid quota rows so anti-drift only
  reconciles keys with a valid stored quota. Do not change this to fail-closed without an explicit product decision.

## Tests

- Update `mysql_test.go` when changing MySQL query flow, cache behavior, or shutdown behavior.
- Use `sqlmock` for branch coverage in MySQL storage tests and keep tests table-driven where practical.
- Update `factory_test.go` if constructor behavior or backend selection changes.
