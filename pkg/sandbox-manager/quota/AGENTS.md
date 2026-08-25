# Sandbox Manager Quota

This package owns quota accounting backends, breaker behavior, and primary-aware
anti-drift repair. The detailed data and correctness model is specified in
`docs/specs/2026-06-17-api-key-sandbox-quota-redis.md`.

## Local Invariants

- Backends own atomic accounting and bounded maintenance. Concrete storage
  scripts and key layout stay within their backend.
- Breakers may protect the hot admission path, but maintenance, repair, and
  deleted-subject cleanup must remain able to make progress.
- Anti-drift consumes neutral Infra snapshots/events and quota subjects. It
  must remain independent of Kubernetes and API representations.
- Repair runs only while the local Manager is primary, cancels on primary loss,
  and converges accounting to observed truth; it must not drain real sandboxes
  merely because usage exceeds a configured limit.
- Keep quota spec parsing and validation storage-neutral. When changing
  dimensions, scopes, unlimited semantics, or runtime entry shape, update and
  follow the design spec.
