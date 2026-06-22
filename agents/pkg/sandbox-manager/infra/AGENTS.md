# Sandbox Manager Infrastructure

This package defines the sandbox-manager infrastructure abstraction used by API
and service layers. It should describe capabilities, operation options, returned
interfaces, and metrics without depending on one concrete backend.

## Responsibilities

- Keep `Infrastructure`, `Sandbox`, and `Builder` as backend-neutral contracts.
- Treat option structs in `types.go` as manager-facing operation contracts.
- Keep claim, clone, timeout, checkpoint, runtime-init, and CSI-mount options stable for callers.
- Use `ClaimMetrics` and `CloneMetrics` for implementation timing and single-line logs.

## Local Guidance

- Add backend-specific behavior in subpackages such as `sandboxcr`, not here.
- Extend interfaces only when the sandbox-manager needs a capability from every backend.
- Keep helper methods on shared types small and serialization-safe.
- Avoid leaking controller or CR reconciliation details into this abstraction layer.
