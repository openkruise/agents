# Sandbox CR Infrastructure

This package is the Kubernetes CRD implementation of the neutral Infra
contracts.

## Local Invariants

- Keep Kubernetes clients, informer cache access, API-reader fallbacks, CRD
  conversion, and CRD mutations inside this implementation.
- Prefer cache-backed reads; use the API reader when expectations or transition
  safety require fresher state.
- Validate backend-specific claim, clone, checkpoint, timeout, runtime-init, and
  CSI-mount inputs next to the flow they protect.
- Preserve cleanup and retry classification across multi-step claim and clone
  operations. A retriable error must mean the outer operation can safely try
  again.
- Pause and Resume are concurrent first-writer-wins transitions; losing callers
  must not overwrite the winning state.
- Convert CRDs into neutral `infra.QuotaSandboxSnapshot`,
  `infra.QuotaSandboxEvent`, and `infra.SandboxResource` values at this
  boundary. Running quota membership is live and not paused; malformed
  informer tombstones are dropped and observed.
- Keep route projection and targeted repair in Manager composition; do not
  reintroduce a local proxy route model in this implementation.
- Do not add API models, HTTP/auth semantics, quota limit evaluation, Redis
  behavior, or Manager admission/release policy here.
