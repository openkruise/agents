# Traffic Extension

This subtree implements the SecurityProfile-driven Envoy ext-proc data plane.
See `docs/components/traffic-extension.md` for its protocol and plugin model.

## Local Invariants

- Plugins have unique stable names, are safe for concurrent use, and preserve
  registration order. Terminal and deferred actions follow one shared,
  deterministic execution contract.
- Keep request processing on immutable, precompiled profile snapshots. Profile
  matching remains namespace-scoped and deterministically ordered.
- Invalid match regexes fail closed. Informer updates with invalid selectors
  retain the last valid snapshot instead of installing the invalid profile.
  Preserve the configured unauthenticated-egress behavior.
- Submit exactly one audit record for each handled request-header call. Audit
  submission must remain non-blocking and count drops instead of delaying the
  request path.
