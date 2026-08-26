# E2B API Key Storage

This package owns concurrent API-key persistence. Secret and MySQL backends
present the same caller-visible contract unless a compatibility decision
explicitly says otherwise.

## Local Invariants

- Keep lifecycle and observable behavior consistent across backends. Background
  startup and shutdown are paired and idempotent.
- Publish only durable observed state; writer-side state must not diverge from
  informer or backend observation.
- MySQL stores only deterministic `HMAC-SHA256(pepper, rawKey)` hashes.
  Pepper is mandatory in MySQL mode, and plaintext may appear only in the
  one-time create result.
- Authentication caches are populated and invalidated conservatively. Fail
  closed when safe invalidation cannot be determined.
- Never delete the well-known admin key. Team cleanup must never remove the
  admin team. The well-known admin key belongs to the admin team.
- The well-known admin key has no quota; do not persist one.
- Storage backends use the API layer's canonical team identity. Team-scoped
  listing must not regress to creator-scoped behavior.
- Invalid stored quota remains availability-compatible on authentication paths,
  while repair considers only valid quota subjects.
