# Sandbox CR Infrastructure

This package implements `pkg/sandbox-manager/infra` using OpenKruise Agents CRDs,
controller-runtime clients, informer cache, and the local proxy route model.

## Responsibilities

- `Infra` wires cache, API reader, proxy, route reconciliation, claim concurrency, and sandbox create rate limiting.
- Claim flow validates options, selects or creates a sandbox, locks it, waits for readiness, initializes runtime, mounts CSI storage, and cleans up failed claims.
- Clone flow resolves checkpoint/template data, creates a sandbox from checkpoint state, waits for readiness, then restores runtime and CSI mounts.
- `Sandbox` wraps the CR object and exposes manager operations such as pause, resume, kill, timeout updates, route lookup, runtime requests, and checkpoint creation.

## Local Guidance

- Keep validation in the `ValidateAndInit...` function closest to the flow it protects.
- Preserve cache-first reads with API-reader fallback when expectations indicate stale informer data.
- Use `retriableError` only when the outer retry loop should attempt a new claim operation.
- Update `infra.ClaimMetrics` or `infra.CloneMetrics` consistently when adding operation stages.
- Preserve package-level `Default...` function variables as test seams.
- Treat `Pause` and `Resume` as first-writer-wins state transitions.
