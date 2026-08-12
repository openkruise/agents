# Identity

This package is the shared boundary for Sandbox token issuance, propagation,
refresh metadata, and CA injection.

## Local Invariants

- Provider, propagator, and CA registration is startup configuration; runtime
  mutation is unsupported.
- Explicit Sandbox identity configuration opts into provider issuance, and the
  provider remains authoritative for token content.
- Preserve the `issue -> propagate -> record expiration` order. Never record a
  fresh expiration when issuance or propagation failed.
- Provider failures are returned to the caller; do not silently replace an
  identity token with a random token.
- Keep refresh metadata encoding centralized and preserve unrelated concurrent
  fields when recording it.
- Never log access tokens, security metadata values, Secret data, or generated
  credentials.
