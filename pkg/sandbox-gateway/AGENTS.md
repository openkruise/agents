# Sandbox Gateway

This subtree implements the standalone Envoy Go-filter data plane and its local
route registry.

## Local Invariants

- Existing imports from API and sandbox-manager packages are legacy debt. New
  gateway behavior should use local or neutral contracts instead of widening
  those dependencies.
- The controller, peer refresh server, registry, and filter must agree on route
  identity, state, and deletion semantics.
- Registry updates are concurrency-safe and resource-version monotonic; stale
  events must not replace newer routes.
- Gateway readiness gates route reads only. Initial informer synchronization
  and peer mutations must populate the Registry before it becomes ready.
- Use the request adapter for Sandbox ID, port, and rewrite extraction rather
  than duplicating protocol parsing in the filter.
- Route only Running Sandboxes. Preserve the established local replies for
  missing, non-running, and unauthorized requests.
- Keep token comparison constant-time and never include access tokens in logs
  or local-reply bodies.
