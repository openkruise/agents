# E2B API Key Storage

This package owns concurrent API-key persistence behind `KeyStorage`. Secret
and MySQL backends must present the same caller-visible contract unless a
compatibility decision explicitly says otherwise.

## Local Invariants

- Keep `KeyStorage` signatures and lifecycle consistent across backends.
  `Run` and `Stop` are paired; background shutdown must be idempotent.
- The Secret backend publishes in-memory index changes only through
  `refresh`. Create and delete update the Secret, then informer-triggered or
  periodic refresh publishes the observed revision; do not mix writer-side
  index mutation with refresh publication.
- MySQL stores only deterministic `HMAC-SHA256(pepper, rawKey)` hashes.
  Pepper is mandatory in MySQL mode, and plaintext may appear only in the
  one-time create result.
- Populate and invalidate both key caches conservatively. If deletion cannot
  determine which authentication cache entry is safe to invalidate, fail the
  delete rather than leave stale authorization state.
- Never delete the well-known admin key. Team cleanup must never remove the
  admin team.
- Team name is the storage and authorization identity; team UUIDs are
  compatibility metadata. Team-scoped listing must not regress to
  creator-scoped behavior.
- Invalid stored quota on authentication/load paths is treated as unlimited for
  compatibility and availability. `ListLimited` remains strict and skips
  invalid quota subjects so anti-drift repairs only valid configurations.
- Parse persisted UUIDs and return or log corruption errors; do not panic on
  malformed stored data.
