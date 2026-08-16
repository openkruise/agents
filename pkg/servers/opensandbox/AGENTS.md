# OpenSandbox API

This directory implements the OpenSandbox-compatible API layer, coexisting in
the same process as the E2B API (`pkg/servers/e2b`) and reusing the same
Manager-layer orchestration (`pkg/sandbox-manager`) and identity/quota system
(`pkg/servers/e2b/keys`) rather than duplicating them. It owns OpenSandbox
protocol behavior only; business rules stay in Manager per the root
`AGENTS.md` layering rules.

## Protocol Contract

- Before changing endpoints, status codes, request/response fields, or
  validation, check the upstream OpenSandbox lifecycle spec:
  https://github.com/alibaba/OpenSandbox/blob/main/specs/sandbox-lifecycle.yml
  (and `diagnostic-api.yml` / `execd-api.yaml` for diagnostics and execd).
- `opensandbox.io/`-prefixed metadata keys are reserved and must be rejected
  on create and on metadata patch, mirroring the E2B API's
  `BlackListPrefix` guard for the same underlying annotation/label store.
- POST /v1/sandboxes/{id}/pause and /resume return 202 Accepted per the spec's
  async contract. This adapter's current backend operations
  (`SandboxManager.PauseSandbox` / `ResumeSandbox`) are synchronous, so the
  202 is returned only after the state change has already completed; a
  polling client sees the sandbox already in its terminal state on the very
  next GET. True asynchronous pause/resume (returning 202 before completion)
  is tracked as follow-up work, not implemented here.

## Known Gaps (tracked, not silently swept under the rug)

- Error responses reuse `web.ApiError` (`{code:<http status>, message}`)
  rather than the spec's `{code:<string>, message}` machine-readable
  envelope. Byte-for-byte error-shape parity is lower priority than endpoint
  and state-machine coverage for this initial slice.
- Snapshot creation from `image`+`snapshotId` restore-on-create and
  Kubernetes-CRD-native concepts without a spec equivalent (e.g. per-sandbox
  runtime provider selection, in-place image update) are recorded in
  `docs/proposals/` rather than expanded here.
