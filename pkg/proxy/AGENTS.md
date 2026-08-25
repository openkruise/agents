# Sandbox Proxy

This package is the legacy Envoy ext-proc and route-distribution implementation
shared by sandbox-manager and sandbox-gateway.

## Local Invariants

- Existing imports from API and sandbox-manager packages are legacy debt. Do
  not add new cross-layer dependencies; shared route contracts belong in a
  policy-neutral package.
- Keep one boundary for protocol parsing, Sandbox mapping, and request
  classification. Do not duplicate protocol interpretation in traffic
  processing.
- Use the shared route model, projection, and state semantics; do not fork a
  proxy-specific route state machine.
- Route traffic only to a Running Sandbox. Keep missing-route, invalid-port,
  and non-running responses consistent across data planes.
- Do not log access tokens, authorization headers, or other credential-bearing
  request data.
