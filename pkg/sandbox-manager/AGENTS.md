# `pkg/sandbox-manager` Guide

This package owns sandbox-manager service logic between the E2B HTTP layer and
the backend-neutral infra layer.

## Quota Boundary

- `InitQuota` wires the production quota subsystem after `Build`, because it needs infra and cache-backed sandbox state.
- Keep `QuotaEnforcer` as the manager-facing surface for admission, delete release, and API-key cleanup.
- Build create/clone quota admission in this package from the caller user, quota spec, and infra `SandboxResource`.
- Release quota after accepted sandbox deletes here; API-key deletion cleanup is called from the E2B layer through `CleanupQuota`.
- Anti-drift repair is gated by manager primary state. Only the current primary should mutate backend quota during repair.
- HTTP status codes, request parsing, and response compatibility stay in `pkg/servers/e2b`; do not encode them here.
