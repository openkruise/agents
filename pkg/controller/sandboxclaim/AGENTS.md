# SandboxClaim Controller

This package performs one-time batch claims from a `SandboxSet` pool.

## Local Invariants

- Preserve the terminal `Pending -> Claiming -> Completed` lifecycle.
  Completed claims do not resume management of their claimed Sandboxes.
- Deleting a completed claim through TTL removes only the `SandboxClaim`; it
  must not release or delete the claimed Sandboxes.
- Keep the reconciler responsible for status and requeue decisions and keep
  claim execution in `core`.
- Use resource-version expectations around status and claimed-object writes;
  cache lag must not duplicate a claim.
- Bound each claim batch and requeue after no-progress attempts instead of
  busy-looping.
- Keep labels, annotations, runtime initialization, CSI mounts, timeouts, and
  other claim options consistent across every claimed Sandbox in the batch.
