# Sandbox Manager Quota

This package owns quota accounting backends, breaker behavior, and primary-aware
anti-drift repair. The detailed data and correctness model is specified in
`docs/specs/2026-06-17-api-key-sandbox-quota-redis.md`.

## Local Invariants

- `Manager` owns typed quota-exceeded results and fail-open behavior for
  backend transport failures.
- `Backend` implementations own atomic acquire/release and bounded
  maintenance operations. Redis scripts and key layout stay in the Redis
  backend.
- Breaker wrapping applies to the hot acquire/release path. `ListEntries` and
  `Cleanup` must bypass an open breaker so repair and deleted-subject cleanup
  can still make progress.
- Anti-drift consumes neutral Infra snapshots/events and quota subjects. It
  must not import CRD types, `pkg/cache`, client-go cache types, or API
  packages.
- Repair runs only while the local Manager is primary, cancels on primary loss,
  and converges accounting to observed truth; it must not drain real sandboxes
  merely because usage exceeds a configured limit.
- Keep quota spec parsing and validation storage-neutral. When changing
  dimensions, scopes, unlimited semantics, or runtime entry shape, update and
  follow the design spec.
