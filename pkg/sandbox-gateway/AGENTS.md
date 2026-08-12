# Sandbox Gateway

This subtree implements the standalone Envoy Go-filter data plane and its local
route registry.

## Local Invariants

- Existing imports from API and sandbox-manager packages are legacy debt. New
  gateway behavior should use local or neutral contracts instead of widening
  those dependencies.
- Every route ingress and serving path uses the shared route model and state
  semantics; the gateway must not fork its own route state machine.
- Readiness gates route serving, not route ingestion. Initial synchronization
  and peer observations must be retained before the gateway becomes ready.
- Keep one boundary for Sandbox ID, port, and rewrite extraction. Do not
  duplicate protocol parsing in traffic processing.
- Route only Running Sandboxes. Preserve the established local replies for
  missing, non-running, and unauthorized requests.
- Keep token comparison constant-time and never include access tokens in logs
  or local-reply bodies.
