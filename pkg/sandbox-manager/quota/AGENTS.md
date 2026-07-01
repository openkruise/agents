# `pkg/sandbox-manager/quota` Guide

This package owns API-key quota accounting, backend behavior, breaker behavior,
and anti-drift repair. It works from manager/infra-neutral inputs, not Sandbox
CRD objects.

## Boundaries

- `Manager` owns fail-open admission semantics for backend transport failures. Quota exceeded remains a typed quota error.
- `Backend` implementations own `Acquire`, `Release`, `ListEntries`, and `Cleanup` storage behavior.
- Redis scripts and key layout stay inside the Redis backend.
- Breaker wrapping belongs at the backend boundary. Admission and anti-drift release share the same hot backend when wired by the manager.
- `AntiDriftDriver` reconciles from `infra.QuotaSandboxSource` snapshots/events plus `quota/spec` subjects. It must not import Sandbox CRDs, `pkg/cache`, `toolscache`, or `pkg/utils/lifecycle`.
- `ListEntries` and `Cleanup` are maintenance/cleanup paths; do not accidentally block deleted-key cleanup behind request-path breaker decisions.

## Quota Spec

- `quota/spec` is the storage-neutral shape for dimensions, scopes, and limits.
- Supported dimensions are `sandbox.count`, `limits.cpu`, and `limits.memory`.
- Supported scopes are `all` and `running`.
- A nil spec, empty spec, empty JSON, or JSON `null` means unlimited.
- Negative, duplicate, unknown-dimension, and unknown-scope limits are invalid.
- Runtime entries store conditional scopes only; do not store `all` in `Entry.Scopes`.
