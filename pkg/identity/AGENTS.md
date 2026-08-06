# Identity

This package is the shared boundary for Sandbox token issuance, propagation,
refresh metadata, and CA injection.

## Local Invariants

- Register providers and token propagators during initialization; their
  registries are not safe for runtime mutation. Register CA bundle specs through
  the synchronized package API and bind runtime predicates during startup.
- A non-empty `AnnotationAgentName` opts a Sandbox into identity-provider
  issuance; providers remain authoritative for the annotation's value.
- Preserve the `issue -> propagate -> record expiration` order. Never record a
  fresh expiration when issuance or propagation failed.
- Provider failures are returned to the caller; do not silently replace an
  identity token with a random token.
- Keep refresh-status encoding centralized and patch annotations without
  overwriting unrelated concurrent fields.
- Never log access tokens, security metadata values, Secret data, or generated
  credentials.
