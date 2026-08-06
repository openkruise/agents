# Sandbox Proxy

This package is the legacy Envoy ext-proc and route-distribution implementation
shared by sandbox-manager and sandbox-gateway.

## Local Invariants

- Existing imports from API and sandbox-manager packages are legacy debt. Do
  not add new cross-layer dependencies; place shared route data in the existing
  neutral `pkg/utils/proxyutils` package.
- Keep `RequestAdapter` as the seam for protocol parsing, Sandbox mapping, and
  API-versus-Sandbox request classification.
- Route updates are resource-version monotonic. Older cache or peer events must
  not overwrite a newer route, and dead routes must be removed.
- Route traffic only to a Running Sandbox. Keep missing-route, invalid-port,
  and non-running responses consistent across data planes.
- Do not log access tokens, authorization headers, or other credential-bearing
  request data.
